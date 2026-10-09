package backup

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
)

// pendingDir holds a restored database waiting for the next start.
const pendingDir = "restore"

// Stage prepares a restore: the backed-up database is written next to the
// live one, upgraded to this version's schema, its secrets re-encrypted
// with this server's master key, and its public address set to newBaseURL
// (the address the restore was made from; "" until the owner's first
// visit). With moveWebhooks false (a copy, e.g. for testing) the Stripe
// webhooks stay with the original server until the owner moves them. The
// swap happens at the next start (ApplyPending), so nothing is half-replaced
// while the server runs.
func Stage(dataDir string, b *Bundle, box *security.Box, newBaseURL string, moveWebhooks bool) error {
	tmp := filepath.Join(dataDir, pendingDir+".tmp")
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
	}()
	path := filepath.Join(tmp, "invoicer.db")
	if err := os.WriteFile(path, b.DB, 0o600); err != nil {
		return err
	}
	st, err := store.Open(path)
	if err != nil {
		return fmt.Errorf("the database of this backup cannot be opened: %w", err)
	}
	if err := prepare(st, b, box, newBaseURL, moveWebhooks); err != nil {
		st.Close()
		return err
	}
	// fold the WAL into the file so it can be moved alone
	st.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	if err := st.Close(); err != nil {
		return err
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		os.Remove(path + sfx)
	}
	dst := filepath.Join(dataDir, pendingDir)
	os.RemoveAll(dst)
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	ok = true
	return nil
}

func prepare(st *store.Store, b *Bundle, box *security.Box, newBaseURL string, moveWebhooks bool) error {
	var check string
	if err := st.DB.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		return errors.New("the database of this backup is damaged")
	}
	var users int
	st.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)
	if users == 0 {
		return errors.New("this backup has no user account")
	}
	old, err := security.NewBox(b.Manifest.MasterKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(old.MasterKey(), box.MasterKey()) {
		if err := Rekey(st.DB, old, box); err != nil {
			return err
		}
	}
	// nobody is signed in on the restored data: sessions belong to the old server
	st.DB.Exec(`DELETE FROM sessions`)
	// the new address: links in e-mails, payment pages and Stripe webhooks
	// must point to it; "" lets the owner's first visit record it
	if newBaseURL != st.Setting("base_url") {
		st.SetSetting("base_url", newBaseURL)
		st.SetSetting("stripe_webhooks_refresh", "1")
	}
	hold := ""
	if !moveWebhooks {
		hold = "1"
	}
	st.SetSetting("stripe_webhooks_hold", hold)
	st.SetSetting("restored_at", strconv.FormatInt(time.Now().Unix(), 10))
	st.Audit(0, 0, "", "system.restored", fmt.Sprintf("backup of %s made on %s (%s)", b.Manifest.BaseURL,
		time.Unix(b.Manifest.CreatedAt, 0).UTC().Format("2006-01-02 15:04 MST"), b.Manifest.App))
	return nil
}

// Rekey re-encrypts every stored secret from the old master key to the new
// one. A secret that cannot be decrypted is dropped (it must be entered
// again) rather than left unreadable.
func Rekey(db *sql.DB, old, cur *security.Box) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	move := func(blob []byte, aad string) []byte {
		if len(blob) == 0 {
			return nil
		}
		v, err := old.Open(blob, aad)
		if err != nil {
			return nil
		}
		return cur.Seal(v, aad)
	}
	type row struct {
		id     int64
		fields [][]byte
	}
	var rows []row
	q, err := tx.Query(`SELECT id, resend_key, stripe_key, stripe_webhook_secret FROM companies`)
	if err != nil {
		return err
	}
	for q.Next() {
		r := row{fields: make([][]byte, 3)}
		if err := q.Scan(&r.id, &r.fields[0], &r.fields[1], &r.fields[2]); err != nil {
			q.Close()
			return err
		}
		rows = append(rows, r)
	}
	q.Close()
	for _, r := range rows {
		p := "company:" + strconv.FormatInt(r.id, 10) + ":"
		if _, err := tx.Exec(`UPDATE companies SET resend_key = ?, stripe_key = ?, stripe_webhook_secret = ? WHERE id = ?`,
			move(r.fields[0], p+"resend"), move(r.fields[1], p+"stripe"), move(r.fields[2], p+"stripe_whsec"), r.id); err != nil {
			return err
		}
	}
	rows = rows[:0]
	q, err = tx.Query(`SELECT id, totp_secret FROM users WHERE totp_secret IS NOT NULL`)
	if err != nil {
		return err
	}
	for q.Next() {
		r := row{fields: make([][]byte, 1)}
		if err := q.Scan(&r.id, &r.fields[0]); err != nil {
			q.Close()
			return err
		}
		rows = append(rows, r)
	}
	q.Close()
	for _, r := range rows {
		sealed := move(r.fields[0], "user:"+strconv.FormatInt(r.id, 10)+":totp")
		if sealed == nil {
			// 2FA cannot be kept: turn it off rather than lock the user out
			if _, err := tx.Exec(`UPDATE users SET totp_secret = NULL, totp_enabled = 0 WHERE id = ?`, r.id); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`UPDATE users SET totp_secret = ? WHERE id = ?`, sealed, r.id); err != nil {
			return err
		}
	}
	var gm string
	tx.QueryRow(`SELECT value FROM settings WHERE key = 'google_maps_key'`).Scan(&gm)
	if gm != "" {
		v := ""
		if raw, err := base64.StdEncoding.DecodeString(gm); err == nil {
			if s := move(raw, "system:google_maps"); s != nil {
				v = base64.StdEncoding.EncodeToString(s)
			}
		}
		if _, err := tx.Exec(`UPDATE settings SET value = ? WHERE key = 'google_maps_key'`, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Pending reports whether a restore waits for the next start.
func Pending(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, pendingDir, "invoicer.db"))
	return err == nil
}

// ApplyPending swaps in a staged restore, keeping a copy of the replaced
// database in backups/. It must run before the database is opened.
func ApplyPending(dataDir string) (bool, error) {
	if !Pending(dataDir) {
		return false, nil
	}
	live := filepath.Join(dataDir, "invoicer.db")
	if _, err := os.Stat(live); err == nil {
		st, err := store.Open(live)
		if err != nil {
			return false, err
		}
		_, err = st.Backup(filepath.Join(dataDir, "backups"), "pre-restore", 5)
		st.Close()
		if err != nil {
			return false, fmt.Errorf("could not keep a copy of the current data: %w", err)
		}
	}
	for _, sfx := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(live + sfx); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	if err := os.Rename(filepath.Join(dataDir, pendingDir, "invoicer.db"), live); err != nil {
		return false, err
	}
	os.RemoveAll(filepath.Join(dataDir, pendingDir))
	return true, nil
}
