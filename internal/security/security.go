// Package security groups the cryptographic primitives used by Invoicer:
// password hashing (Argon2id), random tokens, encryption of stored secrets
// (AES-256-GCM) and TOTP two-factor codes.
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// ---------- random tokens ----------

// Token returns a URL-safe random string carrying n bytes of entropy.
func Token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is used to store bearer tokens (sessions, invites, resets) so a
// database leak does not leak usable credentials.
func HashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---------- passwords (Argon2id) ----------

// OWASP recommended Argon2id parameters (19 MiB, 2 passes, 1 lane).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

// ErrBusy is returned when too many password hashes are being computed at
// once; callers answer "try again" instead of exhausting memory.
var ErrBusy = errors.New("password hashing is busy")

// hashSlots bounds concurrent Argon2 computations (memory = slots × 19 MiB).
var hashSlots = make(chan struct{}, 2)

func acquireHash() bool {
	select {
	case hashSlots <- struct{}{}:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

func releaseHash() { <-hashSlots }

func HashPassword(pw string) string {
	if !acquireHash() {
		// Hashing only happens on authenticated or rate-limited paths; wait
		// rather than fail so a legitimate password change still succeeds.
		hashSlots <- struct{}{}
	}
	defer releaseHash()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

var dummyHash = HashPassword(Token(16))

// CheckPassword verifies pw against an Argon2id hash. It returns false when
// the hashing capacity is exhausted (see CheckPasswordErr to tell apart).
func CheckPassword(hash, pw string) bool {
	ok, _ := CheckPasswordErr(hash, pw)
	return ok
}

// NeedsRehash reports whether a stored hash uses outdated parameters.
func NeedsRehash(hash string) bool {
	return !strings.HasPrefix(hash, fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$", argonMemory, argonTime, argonThreads))
}

func CheckPasswordErr(hash, pw string) (bool, error) {
	if len(pw) > 1024 {
		return false, nil
	}
	if !acquireHash() {
		return false, ErrBusy
	}
	defer releaseHash()
	return checkPassword(hash, pw), nil
}

func checkPassword(hash, pw string) bool {
	if hash == "" {
		// Spend the same time as a real check so unknown accounts are not
		// distinguishable by timing.
		hash = dummyHash
		pw += "\x00"
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	if m > 128*1024 || t > 10 || p == 0 || p > 8 {
		return false // refuse hashes that would cost excessive resources
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// PasswordProblem returns a translation key describing why a password is
// rejected, or "" when it is acceptable.
func PasswordProblem(pw string) string {
	if len([]rune(pw)) < 12 {
		return "err.password_short"
	}
	if len(pw) > 256 {
		return "err.password_long"
	}
	return ""
}

// ---------- encryption of stored secrets ----------

type Box struct {
	aead cipher.AEAD
	key  []byte
}

// NewBox makes a Box from a 32-byte master key.
func NewBox(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("the master key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, key: append([]byte(nil), key...)}, nil
}

// MasterKey returns a copy of the key (for full backups, which carry it
// encrypted so secrets can be moved to a server with another key).
func (b *Box) MasterKey() []byte { return append([]byte(nil), b.key...) }

// LoadBox loads the master key from env or from <dataDir>/master.key, creating
// the file on first start.
func LoadBox(dataDir, envKey string) (*Box, error) {
	var key []byte
	if envKey != "" {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(envKey))
		if err != nil || len(k) != 32 {
			return nil, errors.New("INVOICER_MASTER_KEY must be 32 bytes, base64 encoded")
		}
		key = k
	} else {
		path := filepath.Join(dataDir, "master.key")
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
			if err != nil || len(k) != 32 {
				return nil, fmt.Errorf("%s is corrupted", path)
			}
			key = k
		case errors.Is(err, os.ErrNotExist):
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}
	return NewBox(key)
}

// Seal encrypts plaintext. aad binds the ciphertext to its location (for
// example "company:12:stripe") so values cannot be swapped between rows.
func (b *Box) Seal(plaintext, aad string) []byte {
	if plaintext == "" {
		return nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
}

func (b *Box) Open(ciphertext []byte, aad string) (string, error) {
	if len(ciphertext) == 0 {
		return "", nil
	}
	ns := b.aead.NonceSize()
	if len(ciphertext) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := b.aead.Open(nil, ciphertext[:ns], ciphertext[ns:], []byte(aad))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// ---------- TOTP (RFC 6238) ----------

func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

func totpAt(secret string, counter uint64) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		return ""
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1000000
	return fmt.Sprintf("%06d", code)
}

// TOTPCode returns the code valid at t (used by tests and tooling).
func TOTPCode(secret string, t time.Time) string { return totpAt(secret, uint64(t.Unix()/30)) }

// CheckTOTP accepts the current code and one step of clock drift either way.
// It returns the matched counter so callers can reject replays.
func CheckTOTP(secret, code string, now time.Time) (uint64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return 0, false
	}
	c := uint64(now.Unix() / 30)
	for _, d := range []int64{0, -1, 1} {
		ctr := uint64(int64(c) + d)
		if subtle.ConstantTimeCompare([]byte(totpAt(secret, ctr)), []byte(code)) == 1 {
			return ctr, true
		}
	}
	return 0, false
}

func TOTPURI(issuer, account, secret string) string {
	esc := func(s string) string { return strings.ReplaceAll(urlEscape(s), "+", "%20") }
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&algorithm=SHA1&digits=6&period=30",
		esc(issuer), esc(account), secret, esc(issuer))
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '@' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Code returns a random string of n characters from an unambiguous
// upper-case alphabet.
func Code(n int) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]byte, n)
	var buf [1]byte
	for i := 0; i < n; {
		if _, err := rand.Read(buf[:]); err != nil {
			panic(err)
		}
		out[i] = alphabet[int(buf[0])%len(alphabet)] // 256 is a multiple of 32: no bias
		i++
	}
	return string(out)
}

// RecoveryCodes returns n human-friendly one-time codes.
func RecoveryCodes(n int) []string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	out := make([]string, n)
	for i := range out {
		b := make([]byte, 0, 10)
		var buf [1]byte
		for len(b) < 10 {
			if _, err := rand.Read(buf[:]); err != nil {
				panic(err)
			}
			// rejection sampling keeps the distribution uniform
			if int(buf[0]) < 256-256%len(alphabet) {
				b = append(b, alphabet[int(buf[0])%len(alphabet)])
			}
		}
		out[i] = string(b[:5]) + "-" + string(b[5:])
	}
	return out
}
