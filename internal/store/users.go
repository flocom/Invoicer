package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
)

type User struct {
	ID              int64
	Email           string
	Name            string
	PasswordHash    string
	Role            string
	Lang            string
	TOTPSecret      []byte
	TOTPEnabled     bool
	TOTPLastCounter int64
	RecoveryCodes   string
	Disabled        bool
	FailedLogins    int
	LockedUntil     int64
	CreatedAt       int64
	LastLoginAt     int64
}

func (u *User) IsAdmin() bool { return u.Role == RoleOwner || u.Role == RoleAdmin }
func (u *User) IsOwner() bool { return u.Role == RoleOwner }

const userCols = `id, email, name, password_hash, role, lang, totp_secret, totp_enabled, totp_last_counter,
	recovery_codes, disabled, failed_logins, locked_until, created_at, last_login_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.Lang, &u.TOTPSecret, &u.TOTPEnabled,
		&u.TOTPLastCounter, &u.RecoveryCodes, &u.Disabled, &u.FailedLogins, &u.LockedUntil, &u.CreatedAt, &u.LastLoginAt)
	if err != nil {
		return nil, notFound(err)
	}
	return u, nil
}

func (s *Store) UserCount() (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateFirstOwner atomically creates the owner account, failing if any user
// already exists. This guarantees that only the very first account gets
// ultimate control.
func (s *Store) CreateFirstOwner(email, name, hash, lang string) (*User, error) {
	var id int64
	err := withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errors.New("already initialised")
		}
		res, err := tx.Exec(`INSERT INTO users(email, name, password_hash, role, lang, created_at) VALUES(?,?,?,?,?,?)`,
			email, name, hash, RoleOwner, lang, now())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.User(id)
}

func (s *Store) CreateUser(email, name, hash, role, lang string) (*User, error) {
	if role == RoleOwner {
		return nil, errors.New("there can only be one owner")
	}
	res, err := s.DB.Exec(`INSERT INTO users(email, name, password_hash, role, lang, created_at) VALUES(?,?,?,?,?,?)`,
		email, name, hash, role, lang, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.User(id)
}

func (s *Store) User(id int64) (*User, error) {
	return scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *Store) UserByEmail(email string) (*User, error) {
	return scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE email = ?`, strings.TrimSpace(email)))
}

func (s *Store) Users() ([]*User, error) {
	rows, err := s.DB.Query(`SELECT ` + userCols + ` FROM users ORDER BY CASE role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UpdateUserProfile(id int64, name, lang string) error {
	_, err := s.DB.Exec(`UPDATE users SET name = ?, lang = ? WHERE id = ?`, name, lang, id)
	return err
}

func (s *Store) SetPassword(id int64, hash string) error {
	_, err := s.DB.Exec(`UPDATE users SET password_hash = ?, failed_logins = 0, locked_until = 0 WHERE id = ?`, hash, id)
	s.DeletePasswordResets(id)
	return err
}

// DeletePasswordResets invalidates every outstanding reset link of a user.
// It is called whenever the account's password, role or ownership changes so
// a link created under earlier permissions cannot be replayed.
func (s *Store) DeletePasswordResets(userID int64) {
	s.DB.Exec(`DELETE FROM password_resets WHERE user_id = ?`, userID)
}

func (s *Store) SetUserRole(id int64, role string) error {
	if role == RoleOwner {
		return errors.New("use TransferOwnership")
	}
	_, err := s.DB.Exec(`UPDATE users SET role = ? WHERE id = ? AND role != 'owner'`, role, id)
	s.DeletePasswordResets(id)
	return err
}

func (s *Store) SetUserDisabled(id int64, disabled bool) error {
	_, err := s.DB.Exec(`UPDATE users SET disabled = ? WHERE id = ? AND role != 'owner'`, b2i(disabled), id)
	s.DeletePasswordResets(id)
	if err == nil && disabled {
		_, err = s.DB.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
	}
	return err
}

func (s *Store) DeleteUser(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM users WHERE id = ? AND role != 'owner'`, id)
	return err
}

func (s *Store) TransferOwnership(from, to int64) error {
	return withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE users SET role = 'admin' WHERE id = ? AND role = 'owner'`, from); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM password_resets WHERE user_id IN (?, ?)`, from, to); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE users SET role = 'owner' WHERE id = ? AND disabled = 0`, to)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// RecordLoginFailure counts failures for information only. Throttling is done
// per client and account (see web.loginFailKey) so that nobody can lock an
// account out by guessing wrong on purpose.
func (s *Store) RecordLoginFailure(id int64) {
	s.DB.Exec(`UPDATE users SET failed_logins = failed_logins + 1 WHERE id = ?`, id)
}

func (s *Store) RecordLoginSuccess(id int64) {
	s.DB.Exec(`UPDATE users SET failed_logins = 0, locked_until = 0, last_login_at = ? WHERE id = ?`, now(), id)
}

func (s *Store) SetTOTP(id int64, secret []byte, enabled bool, recovery string) error {
	_, err := s.DB.Exec(`UPDATE users SET totp_secret = ?, totp_enabled = ?, recovery_codes = ?, totp_last_counter = 0 WHERE id = ?`,
		secret, b2i(enabled), recovery, id)
	return err
}

// UseTOTPCounter stores the last accepted counter; it fails if the code was
// already used (replay protection).
func (s *Store) UseTOTPCounter(id int64, counter int64) bool {
	res, err := s.DB.Exec(`UPDATE users SET totp_last_counter = ? WHERE id = ? AND totp_last_counter < ?`, counter, id, counter)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// ReplaceRecoveryCodes swaps the code list only if it still equals old.
func (s *Store) ReplaceRecoveryCodes(id int64, old, codes string) bool {
	res, err := s.DB.Exec(`UPDATE users SET recovery_codes = ? WHERE id = ? AND recovery_codes = ?`, codes, id, old)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// UpdatePasswordHash re-hashes a password with current parameters (same
// password: sessions and reset links are kept).
func (s *Store) UpdatePasswordHash(id int64, hash string) {
	s.DB.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
}

func (s *Store) SetRecoveryCodes(id int64, codes string) error {
	_, err := s.DB.Exec(`UPDATE users SET recovery_codes = ? WHERE id = ?`, codes, id)
	return err
}

// ---------- sessions ----------

type Session struct {
	IDHash     []byte
	UserID     int64
	CSRF       string
	MFAPending bool
	CompanyID  int64
	IP         string
	UserAgent  string
	CreatedAt  int64
	LastSeen   int64
	ExpiresAt  int64
}

const (
	SessionIdle     = 7 * 24 * time.Hour
	SessionAbsolute = 30 * 24 * time.Hour
)

func (s *Store) CreateSession(idHash []byte, userID int64, csrf string, mfaPending bool, ip, ua string) error {
	t := now()
	_, err := s.DB.Exec(`INSERT INTO sessions(id_hash, user_id, csrf, mfa_pending, ip, user_agent, created_at, last_seen, expires_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, idHash, userID, csrf, b2i(mfaPending), ip, ua, t, t, t+int64(SessionIdle.Seconds()))
	return err
}

func (s *Store) Session(idHash []byte) (*Session, error) {
	se := &Session{}
	err := s.DB.QueryRow(`SELECT id_hash, user_id, csrf, mfa_pending, company_id, ip, user_agent, created_at, last_seen, expires_at
		FROM sessions WHERE id_hash = ?`, idHash).Scan(&se.IDHash, &se.UserID, &se.CSRF, &se.MFAPending, &se.CompanyID,
		&se.IP, &se.UserAgent, &se.CreatedAt, &se.LastSeen, &se.ExpiresAt)
	if err != nil {
		return nil, notFound(err)
	}
	t := now()
	if se.ExpiresAt < t || se.CreatedAt+int64(SessionAbsolute.Seconds()) < t {
		s.DeleteSession(idHash)
		return nil, ErrNotFound
	}
	return se, nil
}

func (s *Store) TouchSession(idHash []byte) {
	t := now()
	s.DB.Exec(`UPDATE sessions SET last_seen = ?, expires_at = ? WHERE id_hash = ?`, t, t+int64(SessionIdle.Seconds()), idHash)
}

func (s *Store) CompleteMFA(idHash []byte) error {
	_, err := s.DB.Exec(`UPDATE sessions SET mfa_pending = 0 WHERE id_hash = ?`, idHash)
	return err
}

func (s *Store) SetSessionCompany(idHash []byte, companyID int64) {
	s.DB.Exec(`UPDATE sessions SET company_id = ? WHERE id_hash = ?`, companyID, idHash)
}

func (s *Store) DeleteSession(idHash []byte) {
	s.DB.Exec(`DELETE FROM sessions WHERE id_hash = ?`, idHash)
}

func (s *Store) DeleteUserSessions(userID int64, except []byte) {
	s.DB.Exec(`DELETE FROM sessions WHERE user_id = ? AND id_hash != ?`, userID, except)
}

func (s *Store) PurgeExpired() {
	t := now()
	s.DB.Exec(`DELETE FROM sessions WHERE expires_at < ? OR created_at < ?`, t, t-int64(SessionAbsolute.Seconds()))
	s.DB.Exec(`DELETE FROM invites WHERE expires_at < ?`, t-86400*30)
	s.DB.Exec(`DELETE FROM password_resets WHERE expires_at < ?`, t-86400)
	s.DB.Exec(`DELETE FROM audit_log WHERE created_at < ?`, t-86400*730)
	// e-mails stay listed, their bodies are kept two years
	s.DB.Exec(`UPDATE email_log SET html = '', text_body = '' WHERE created_at < ? AND (html != '' OR text_body != '')`, t-86400*730)
}

// ---------- invites & password resets ----------

type Invite struct {
	Email      string
	Role       string
	CompanyIDs []int64
	ExpiresAt  int64
	UsedAt     int64
}

func (s *Store) CreateInvite(tokenHash []byte, email, role string, companies []int64, by int64, ttl time.Duration) error {
	ids := make([]string, len(companies))
	for i, c := range companies {
		ids[i] = strconv.FormatInt(c, 10)
	}
	_, err := s.DB.Exec(`INSERT INTO invites(token_hash, email, role, company_ids, created_by, created_at, expires_at) VALUES(?,?,?,?,?,?,?)`,
		tokenHash, email, role, strings.Join(ids, ","), by, now(), time.Now().Add(ttl).Unix())
	return err
}

func (s *Store) Invite(tokenHash []byte) (*Invite, error) {
	var inv Invite
	var ids string
	err := s.DB.QueryRow(`SELECT email, role, company_ids, expires_at, used_at FROM invites WHERE token_hash = ?`, tokenHash).
		Scan(&inv.Email, &inv.Role, &ids, &inv.ExpiresAt, &inv.UsedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if inv.UsedAt != 0 || inv.ExpiresAt < now() {
		return nil, ErrNotFound
	}
	for _, p := range strings.Split(ids, ",") {
		if id, err := strconv.ParseInt(p, 10, 64); err == nil {
			inv.CompanyIDs = append(inv.CompanyIDs, id)
		}
	}
	return &inv, nil
}

type PendingInvite struct {
	ID        string // hex of the token hash (cannot be used to accept)
	Email     string
	Role      string
	ExpiresAt int64
}

func (s *Store) PendingInvites() ([]PendingInvite, error) {
	rows, err := s.DB.Query(`SELECT lower(hex(token_hash)), email, role, expires_at FROM invites
		WHERE used_at = 0 AND expires_at > ? ORDER BY created_at DESC`, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingInvite
	for rows.Next() {
		var p PendingInvite
		if err := rows.Scan(&p.ID, &p.Email, &p.Role, &p.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RevokeInvite deletes a pending invitation; admins may only revoke member
// invitations, the owner any.
func (s *Store) RevokeInvite(id string, owner bool) (string, error) {
	var email, role string
	if err := s.DB.QueryRow(`SELECT email, role FROM invites WHERE lower(hex(token_hash)) = ? AND used_at = 0`, id).Scan(&email, &role); err != nil {
		return "", notFound(err)
	}
	if role != RoleMember && !owner {
		return "", ErrNotFound
	}
	_, err := s.DB.Exec(`DELETE FROM invites WHERE lower(hex(token_hash)) = ? AND used_at = 0`, id)
	return email, err
}

// AcceptInvite consumes the invite and creates the account in one transaction.
func (s *Store) AcceptInvite(tokenHash []byte, name, hash, lang string) (*User, error) {
	inv, err := s.Invite(tokenHash)
	if err != nil {
		return nil, err
	}
	var id int64
	err = withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE invites SET used_at = ? WHERE token_hash = ? AND used_at = 0`, now(), tokenHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		r, err := tx.Exec(`INSERT INTO users(email, name, password_hash, role, lang, created_at) VALUES(?,?,?,?,?,?)`,
			inv.Email, name, hash, inv.Role, lang, now())
		if err != nil {
			return err
		}
		id, _ = r.LastInsertId()
		for _, c := range inv.CompanyIDs {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO company_members(company_id, user_id) SELECT id, ? FROM companies WHERE id = ?`, id, c); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.User(id)
}

func (s *Store) CreatePasswordReset(tokenHash []byte, userID int64, ttl time.Duration) error {
	s.DeletePasswordResets(userID) // only the newest link is valid
	_, err := s.DB.Exec(`INSERT INTO password_resets(token_hash, user_id, created_at, expires_at) VALUES(?,?,?,?)`,
		tokenHash, userID, now(), time.Now().Add(ttl).Unix())
	return err
}

func (s *Store) PasswordResetUser(tokenHash []byte) (*User, error) {
	var uid int64
	err := s.DB.QueryRow(`SELECT user_id FROM password_resets WHERE token_hash = ? AND used_at = 0 AND expires_at > ?`, tokenHash, now()).Scan(&uid)
	if err != nil {
		return nil, notFound(err)
	}
	return s.User(uid)
}

func (s *Store) ConsumePasswordReset(tokenHash []byte, hash string) error {
	return withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		var uid int64
		if err := tx.QueryRow(`SELECT user_id FROM password_resets WHERE token_hash = ? AND used_at = 0 AND expires_at > ?`, tokenHash, now()).Scan(&uid); err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(`UPDATE password_resets SET used_at = ? WHERE token_hash = ?`, now(), tokenHash); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE users SET password_hash = ?, failed_logins = 0, locked_until = 0 WHERE id = ?`, hash, uid); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, uid)
		return err
	})
}
