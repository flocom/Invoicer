package security

import (
	"testing"
	"time"
)

func TestPassword(t *testing.T) {
	h := HashPassword("a very long passphrase")
	if !CheckPassword(h, "a very long passphrase") || CheckPassword(h, "wrong") || CheckPassword("", "") {
		t.Fatal("password check")
	}
}

func TestBox(t *testing.T) {
	b, err := LoadBox(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	ct := b.Seal("sk_live_secret", "company:1:stripe")
	if pt, err := b.Open(ct, "company:1:stripe"); err != nil || pt != "sk_live_secret" {
		t.Fatal("roundtrip")
	}
	if _, err := b.Open(ct, "company:2:stripe"); err == nil {
		t.Fatal("ciphertext must be bound to its location")
	}
}

func TestTOTP(t *testing.T) {
	// RFC 6238 test vector (SHA1, secret "12345678901234567890")
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	if _, ok := CheckTOTP(secret, "287082", time.Unix(59, 0)); !ok {
		t.Fatal("RFC vector failed")
	}
	if _, ok := CheckTOTP(secret, "000000", time.Unix(59, 0)); ok {
		t.Fatal("wrong code accepted")
	}
}
