package backup

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
)

func newBox(t *testing.T) *security.Box {
	k := make([]byte, 32)
	rand.Read(k)
	b, err := security.NewBox(k)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBackupMovesToAServerWithAnotherKey(t *testing.T) {
	oldDir := t.TempDir()
	st, err := store.Open(filepath.Join(oldDir, "invoicer.db"))
	if err != nil {
		t.Fatal(err)
	}
	oldBox := newBox(t)
	u, err := st.CreateFirstOwner("o@example.test", "Owner", security.HashPassword("correct horse battery"), "fr")
	if err != nil {
		t.Fatal(err)
	}
	co, _ := st.CreateCompany("pubold", "Alpha", "EUR", "fr", u.ID)
	st.UpdateCompanyStripe(co.ID, oldBox.Seal("sk_live_secret", "company:1:stripe"), oldBox.Seal("whsec_old", "company:1:stripe_whsec"), "we_old", "Alpha")
	st.UpdateCompanyEmail(co.ID, oldBox.Seal("re_key", "company:1:resend"), false, "billing@alpha.test", "", "")
	st.SetTOTP(u.ID, oldBox.Seal("TOTPSECRET", "user:1:totp"), true, "")
	st.SetSetting("google_maps_key", base64.StdEncoding.EncodeToString(oldBox.Seal("AIzaKey", "system:google_maps")))
	st.SetSetting("base_url", "https://old.example.com")
	if _, err := st.DB.Exec(`INSERT INTO sessions(id_hash, user_id, csrf, created_at, last_seen, expires_at) VALUES(x'01', 1, 'c', 0, 0, 9999999999)`); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(&buf, st, oldBox, "v1.2.3", "short"); !errors.Is(err, ErrShortPassword) {
		t.Fatalf("short passphrase: %v", err)
	}
	if err := Write(&buf, st, oldBox, "v1.2.3", "a long passphrase"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if bytes.Contains(buf.Bytes(), []byte("Alpha")) || bytes.Contains(buf.Bytes(), []byte("SQLite")) {
		t.Fatal("the backup is not encrypted")
	}
	if _, err := Read(bytes.NewReader(buf.Bytes()), "wrong passphrase"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := Read(bytes.NewReader([]byte("hello")), "x"); !errors.Is(err, ErrNotBackup) {
		t.Fatalf("not a backup: %v", err)
	}
	b, err := Read(bytes.NewReader(buf.Bytes()), "a long passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if b.Manifest.BaseURL != "https://old.example.com" || b.Manifest.Companies != 1 || b.Manifest.Users != 1 || b.Manifest.App != "v1.2.3" {
		t.Fatalf("manifest: %+v", b.Manifest)
	}

	// the new server already has (other) data and its own master key
	newDir := t.TempDir()
	cur, _ := store.Open(filepath.Join(newDir, "invoicer.db"))
	cur.CreateFirstOwner("someone@else.test", "Else", security.HashPassword("correct horse battery"), "en")
	cur.Close()
	newBox := newBox(t)
	if err := Stage(newDir, b, newBox, "https://new.example.org", true); err != nil {
		t.Fatal(err)
	}
	if !Pending(newDir) {
		t.Fatal("restore not staged")
	}
	applied, err := ApplyPending(newDir)
	if err != nil || !applied {
		t.Fatalf("apply: %v %v", applied, err)
	}
	if Pending(newDir) {
		t.Fatal("restore still pending")
	}
	if kept, _ := filepath.Glob(filepath.Join(newDir, "backups", "pre-restore-*.db")); len(kept) != 1 {
		t.Fatalf("the replaced data should be kept: %v", kept)
	}
	st, err = store.Open(filepath.Join(newDir, "invoicer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UserByEmail("o@example.test"); err != nil {
		t.Fatal("restored users missing")
	}
	if _, err := st.UserByEmail("someone@else.test"); err == nil {
		t.Fatal("the previous data should be replaced")
	}
	c, _ := st.Company(1)
	if v, err := newBox.Open(c.StripeKey, "company:1:stripe"); err != nil || v != "sk_live_secret" {
		t.Fatalf("stripe key re-encrypted: %q %v", v, err)
	}
	if v, _ := newBox.Open(c.ResendKey, "company:1:resend"); v != "re_key" {
		t.Fatalf("resend key: %q", v)
	}
	if v, _ := newBox.Open(c.StripeWebhookSecret, "company:1:stripe_whsec"); v != "whsec_old" {
		t.Fatalf("webhook secret: %q", v)
	}
	o, _ := st.UserByEmail("o@example.test")
	if v, _ := newBox.Open(o.TOTPSecret, "user:1:totp"); v != "TOTPSECRET" || !o.TOTPEnabled {
		t.Fatalf("2FA secret: %q %v", v, o.TOTPEnabled)
	}
	raw, _ := base64.StdEncoding.DecodeString(st.Setting("google_maps_key"))
	if v, _ := newBox.Open(raw, "system:google_maps"); v != "AIzaKey" {
		t.Fatalf("google key: %q", v)
	}
	if st.Setting("base_url") != "https://new.example.org" || st.Setting("stripe_webhooks_refresh") != "1" {
		t.Fatalf("new address: %q refresh=%q", st.Setting("base_url"), st.Setting("stripe_webhooks_refresh"))
	}
	var n int
	st.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Fatal("sessions of the old server should be dropped")
	}
	os.RemoveAll(newDir)
}
