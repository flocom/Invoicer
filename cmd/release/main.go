// Command release is used by the GitHub release workflow to create and sign
// the update manifest. It is not shipped in the Docker image.
//
//	go run ./cmd/release keygen                       # print a new key pair
//	RELEASE_SIGNING_KEY=... go run ./cmd/release manifest -version v1.2.3 -dir dist
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flocom/invoicer/internal/updater"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: release keygen | manifest -version vX.Y.Z -dir dist [-min vX.Y.Z] [-critical] [-notes text]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}
		fmt.Println("private (GitHub secret RELEASE_SIGNING_KEY):", base64.StdEncoding.EncodeToString(priv.Seed()))
		fmt.Println("public  (internal/updater/key.go):          ", base64.StdEncoding.EncodeToString(pub))
	case "manifest":
		fs := flag.NewFlagSet("manifest", flag.ExitOnError)
		version := fs.String("version", "", "release version (vX.Y.Z)")
		dir := fs.String("dir", "dist", "directory with invoicer-<os>-<arch> binaries")
		minV := fs.String("min", "", "minimum version allowed to keep running (forces update below)")
		critical := fs.Bool("critical", false, "install immediately on every instance")
		notes := fs.String("notes", "", "short release notes")
		fs.Parse(os.Args[2:])
		if err := manifest(*version, *dir, *minV, *critical, *notes); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
}

func manifest(version, dir, minV string, critical bool, notes string) error {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("RELEASE_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return fmt.Errorf("RELEASE_SIGNING_KEY must be a base64 Ed25519 seed")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	if base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)) != updater.PublicKey {
		return fmt.Errorf("signing key does not match the public key compiled into the updater")
	}
	m := updater.Manifest{Version: version, MinVersion: minV, Critical: critical, Notes: notes,
		PublishedAt: time.Now().UTC().Format(time.RFC3339), Assets: map[string]updater.Asset{}}
	files, _ := filepath.Glob(filepath.Join(dir, "invoicer-linux-*"))
	if len(files) == 0 {
		return fmt.Errorf("no binaries found in %s", dir)
	}
	for _, f := range files {
		name := filepath.Base(f)
		parts := strings.Split(name, "-") // invoicer-linux-amd64
		if len(parts) != 3 {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		m.Assets[parts[1]+"/"+parts[2]] = updater.Asset{Name: name, SHA256: hex.EncodeToString(sum[:])}
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	sig := ed25519.Sign(priv, raw)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json.sig"), []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("signed manifest for %s with %d assets\n", version, len(m.Assets))
	return nil
}
