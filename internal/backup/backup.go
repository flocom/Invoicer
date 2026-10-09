// Package backup makes and restores full backups: the whole database plus
// the master key that encrypts the secrets stored in it, in one file
// encrypted with a passphrase. It is how an instance moves to another
// server or domain.
//
// File layout: magic "INVOICER-BACKUP\n", version byte, Argon2id salt (16),
// nonce (12), then AES-256-GCM over a gzipped tar holding manifest.json
// and invoicer.db.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
)

const (
	magic   = "INVOICER-BACKUP\n"
	version = 1
	// MaxSize bounds what a restore accepts (and keeps in memory).
	MaxSize = 1 << 30
	// MinPassphrase is the shortest accepted passphrase.
	MinPassphrase = 10
)

var (
	ErrNotBackup     = errors.New("this file is not an Invoicer backup")
	ErrPassphrase    = errors.New("wrong passphrase, or damaged file")
	ErrNewerSchema   = errors.New("this backup comes from a newer version of Invoicer: update this server first")
	ErrShortPassword = fmt.Errorf("the passphrase must be at least %d characters", MinPassphrase)
)

// Manifest describes a backup.
type Manifest struct {
	Format    int    `json:"format"`
	App       string `json:"app_version"`
	Schema    int    `json:"schema"`
	CreatedAt int64  `json:"created_at"`
	BaseURL   string `json:"base_url"`
	MasterKey []byte `json:"master_key"`
	Companies int    `json:"companies"`
	Invoices  int    `json:"invoices"`
	Users     int    `json:"users"`
}

// Bundle is a decrypted backup.
type Bundle struct {
	Manifest Manifest
	DB       []byte
}

// Write makes a full backup of st to w.
func Write(w io.Writer, st *store.Store, box *security.Box, appVersion, passphrase string) error {
	if len([]rune(passphrase)) < MinPassphrase {
		return ErrShortPassword
	}
	tmp, err := os.MkdirTemp("", "invoicer-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	dbPath := filepath.Join(tmp, "invoicer.db")
	if err := st.Snapshot(dbPath); err != nil {
		return err
	}
	db, err := os.ReadFile(dbPath)
	if err != nil {
		return err
	}
	m := Manifest{Format: version, App: appVersion, Schema: store.SchemaVersion(), CreatedAt: time.Now().Unix(),
		BaseURL: st.Setting("base_url"), MasterKey: box.MasterKey()}
	st.DB.QueryRow(`SELECT COUNT(*) FROM companies`).Scan(&m.Companies)
	st.DB.QueryRow(`SELECT COUNT(*) FROM invoices`).Scan(&m.Invoices)
	st.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&m.Users)
	mj, _ := json.MarshalIndent(m, "", "  ")

	var plain bytes.Buffer
	gz := gzip.NewWriter(&plain)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{{"manifest.json", mj}, {"invoicer.db", db}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.data)), ModTime: time.Unix(m.CreatedAt, 0)}); err != nil {
			return err
		}
		if _, err := tw.Write(f.data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	aead, err := cipherFor(passphrase, salt)
	if err != nil {
		return err
	}
	header := append(append(append([]byte(magic), version), salt...), nonce...)
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err = w.Write(aead.Seal(nil, nonce, plain.Bytes(), header))
	return err
}

func cipherFor(passphrase string, salt []byte) (cipher.AEAD, error) {
	key := argon2.IDKey([]byte(passphrase), salt, 3, 64*1024, 2, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Read decrypts and checks a backup.
func Read(r io.Reader, passphrase string) (*Bundle, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxSize {
		return nil, errors.New("this file is too large")
	}
	hl := len(magic) + 1 + 16 + 12
	if len(raw) < hl || string(raw[:len(magic)]) != magic {
		return nil, ErrNotBackup
	}
	if raw[len(magic)] != version {
		return nil, ErrNewerSchema
	}
	header := raw[:hl]
	salt := raw[len(magic)+1 : len(magic)+17]
	nonce := raw[len(magic)+17 : hl]
	aead, err := cipherFor(passphrase, salt)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, raw[hl:], header)
	if err != nil {
		return nil, ErrPassphrase
	}
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, ErrNotBackup
	}
	b := &Bundle{}
	tr := tar.NewReader(gz)
	var haveManifest bool
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, ErrNotBackup
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxSize))
		if err != nil {
			return nil, err
		}
		switch h.Name {
		case "manifest.json":
			if err := json.Unmarshal(data, &b.Manifest); err != nil {
				return nil, ErrNotBackup
			}
			haveManifest = true
		case "invoicer.db":
			b.DB = data
		}
	}
	if !haveManifest || len(b.DB) == 0 || len(b.Manifest.MasterKey) != 32 {
		return nil, ErrNotBackup
	}
	if b.Manifest.Schema > store.SchemaVersion() {
		return nil, ErrNewerSchema
	}
	return b, nil
}
