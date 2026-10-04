// Package store is the SQLite persistence layer.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	DB   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)" +
		"&_pragma=synchronous(NORMAL)&_pragma=secure_delete(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	s := &Store{DB: db, path: path}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.DB.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		slog.Warn("database schema is newer than this binary (rollback?)", "db", v, "known", len(migrations))
		return nil
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		slog.Info("applied database migration", "version", i+1)
	}
	return nil
}

// Backup writes a consistent copy of the database to dir and keeps the newest
// `keep` files sharing the same prefix.
func (s *Store) Backup(dir, prefix string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := filepath.Join(dir, fmt.Sprintf("%s-%s.db", prefix, time.Now().UTC().Format("20060102-150405")))
	if _, err := s.DB.Exec(`VACUUM INTO ?`, name); err != nil {
		return "", err
	}
	_ = os.Chmod(name, 0o600)
	if keep > 0 {
		matches, _ := filepath.Glob(filepath.Join(dir, prefix+"-*.db"))
		sort.Strings(matches)
		for len(matches) > keep {
			os.Remove(matches[0])
			matches = matches[1:]
		}
	}
	return name, nil
}

// ---------- settings ----------

func (s *Store) Setting(key string) string {
	var v string
	_ = s.DB.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---------- audit ----------

func (s *Store) Audit(userID, companyID int64, ip, action, details string) {
	_, err := s.DB.Exec(`INSERT INTO audit_log(user_id, company_id, action, details, ip, created_at) VALUES(?,?,?,?,?,?)`,
		userID, companyID, action, details, ip, time.Now().Unix())
	if err != nil {
		slog.Error("audit log", "err", err)
	}
}

type AuditEntry struct {
	ID        int64
	UserName  string
	Company   string
	Action    string
	Details   string
	IP        string
	CreatedAt int64
}

func (s *Store) AuditEntries(limit int) ([]AuditEntry, error) {
	rows, err := s.DB.Query(`SELECT a.id, COALESCE(u.name, ''), COALESCE(c.name, ''), a.action, a.details, a.ip, a.created_at
		FROM audit_log a LEFT JOIN users u ON u.id = a.user_id LEFT JOIN companies c ON c.id = a.company_id
		ORDER BY a.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.UserName, &e.Company, &e.Action, &e.Details, &e.IP, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- email log ----------

type EmailLog struct {
	ID         int64
	CompanyID  int64
	InvoiceID  int64
	Kind       string
	To         string
	Subject    string
	Status     string
	ProviderID string
	Error      string
	CreatedAt  int64
}

func (s *Store) LogEmail(e EmailLog) {
	_, err := s.DB.Exec(`INSERT INTO email_log(company_id, invoice_id, kind, to_addr, subject, status, provider_id, error, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, e.CompanyID, e.InvoiceID, e.Kind, e.To, e.Subject, e.Status, e.ProviderID, e.Error, time.Now().Unix())
	if err != nil {
		slog.Error("email log", "err", err)
	}
}

func (s *Store) EmailsForInvoice(invoiceID int64) ([]EmailLog, error) {
	rows, err := s.DB.Query(`SELECT id, company_id, invoice_id, kind, to_addr, subject, status, provider_id, error, created_at
		FROM email_log WHERE invoice_id = ? ORDER BY id DESC`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmailLog
	for rows.Next() {
		var e EmailLog
		if err := rows.Scan(&e.ID, &e.CompanyID, &e.InvoiceID, &e.Kind, &e.To, &e.Subject, &e.Status, &e.ProviderID, &e.Error, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- helpers ----------

func now() int64 { return time.Now().Unix() }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
