package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0}, {"v1.2.4", "v1.2.3", 1}, {"v1.10.0", "v1.9.9", 1}, {"v2.0.0", "v10.0.0", -1},
		{"v1.0.0", "v1.0.0-rc1", 1}, {"v1.0.0-rc2", "v1.0.0-rc1", 1}, {"bad", "v1.0.0", 0}, {"v2.0.0-../../x", "v1.0.0", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestVerifyManifest(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := PublicKey
	PublicKey = base64.StdEncoding.EncodeToString(pub)
	defer func() { PublicKey = old }()
	raw := []byte(`{"version":"v1.2.3","assets":{}}`)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw))
	if _, err := verifyManifest(raw, []byte(sig)); err != nil {
		t.Fatal(err)
	}
	tampered := []byte(`{"version":"v9.9.9","assets":{}}`)
	if _, err := verifyManifest(tampered, []byte(sig)); err == nil {
		t.Fatal("tampered manifest accepted")
	}
}

func TestDownloadAndHandoffSelection(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := PublicKey
	PublicKey = base64.StdEncoding.EncodeToString(pub)
	defer func() { PublicKey = old }()

	bin := []byte("#!/bin/true\nfake binary")
	sum := sha256.Sum256(bin)
	m := Manifest{Version: "v1.1.0", Assets: map[string]Asset{platform(): {Name: "invoicer-bin", SHA256: hex.EncodeToString(sum[:])}}}
	raw, _ := json.Marshal(m)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw))
	tamper := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifest.json"):
			w.Write(raw)
		case strings.HasSuffix(r.URL.Path, "/manifest.json.sig"):
			w.Write([]byte(sig))
		case strings.HasSuffix(r.URL.Path, "/invoicer-bin"):
			if tamper {
				w.Write([]byte("evil"))
				return
			}
			w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	u := New("o/r", "v1.0.0", dir, true)
	u.base = srv.URL + "/"

	tamper = true
	if _, err := u.Download(t.Context()); err == nil {
		t.Fatal("tampered binary accepted")
	}
	tamper = false
	v, err := u.Download(t.Context())
	if err != nil || v != "v1.1.0" {
		t.Fatalf("download: %v %v", v, err)
	}
	if path, nv := Newest("v1.0.0", dir); nv != "v1.1.0" || path == "" {
		t.Fatalf("newest: %s %s", path, nv)
	}
	if _, nv := Newest("v1.1.0", dir); nv != "" {
		t.Fatal("same version must not be selected")
	}
	// a version that failed to boot three times is skipped
	os.WriteFile(filepath.Join(dir, "invoicer-v1.1.0.boots"), []byte("3"), 0o600)
	if _, nv := Newest("v1.0.0", dir); nv != "" {
		t.Fatal("crashing version must be skipped")
	}
	// tampering with the stored binary is detected
	os.Remove(filepath.Join(dir, "invoicer-v1.1.0.boots"))
	os.WriteFile(filepath.Join(dir, "invoicer-v1.1.0"), []byte("evil"), 0o700)
	if _, nv := Newest("v1.0.0", dir); nv != "" {
		t.Fatal("modified binary must be rejected")
	}
}
