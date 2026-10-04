// Package updater implements signed self-updates from GitHub Releases.
//
// Every release publishes manifest.json (version, minimum supported version,
// SHA-256 of each binary) and manifest.json.sig, an Ed25519 signature made
// with a key that never leaves the release pipeline. The public key is
// compiled into the binary, so a compromised download location cannot push
// code: the manifest must verify, its version must be newer, and the binary
// must match the signed hash.
//
// New binaries are stored in <data>/bin and the process re-executes itself
// into them. At start the launcher picks the newest verified binary, and
// falls back automatically if a version fails to boot three times.
package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// PublicKey is the base64 Ed25519 key that signs release manifests (see key.go).
var PublicKey = publicKey

type Asset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Version     string           `json:"version"`
	MinVersion  string           `json:"min_version"`
	Critical    bool             `json:"critical"`
	Notes       string           `json:"notes"`
	PublishedAt string           `json:"published_at"`
	Assets      map[string]Asset `json:"assets"`
}

type Status struct {
	Current     string
	Latest      string
	Notes       string
	Critical    bool
	Required    bool // current < min_version
	Available   bool
	LastCheck   time.Time
	LastError   string
	State       string // idle, checking, downloading, restarting
	AutoInstall bool
	Enabled     bool // false for dev builds or missing public key
}

type Updater struct {
	Repo        string
	Current     string
	BinDir      string
	AutoInstall bool
	// BeforeRestart runs after a successful download and before the process
	// is replaced (database backup, graceful HTTP shutdown).
	BeforeRestart func(newVersion string)

	base     string // https://github.com/ (overridden in tests)
	mu       sync.Mutex
	status   Status
	manifest *Manifest
	busy     bool
	http     *http.Client
}

func New(repo, current, binDir string, autoInstall bool) *Updater {
	u := &Updater{Repo: repo, Current: current, BinDir: binDir, AutoInstall: autoInstall, base: "https://github.com/",
		http: &http.Client{Timeout: 5 * time.Minute}}
	u.status = Status{Current: current, State: "idle", AutoInstall: autoInstall, Enabled: u.enabled()}
	return u
}

func (u *Updater) enabled() bool {
	_, ok := parseVersion(u.Current)
	return ok && PublicKey != "" && !strings.Contains(u.Current, "dev")
}

func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

func (u *Updater) setErr(err error) {
	u.mu.Lock()
	u.status.LastError = ""
	if err != nil {
		u.status.LastError = err.Error()
	}
	u.status.State = "idle"
	u.mu.Unlock()
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Invoicer/"+u.Current)
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("download too large")
	}
	return b, nil
}

func verifyManifest(raw, sig []byte) (*Manifest, error) {
	pub, err := base64.StdEncoding.DecodeString(PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("invalid embedded public key")
	}
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(pub, raw, s) {
		return nil, errors.New("manifest signature verification failed")
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if _, ok := parseVersion(m.Version); !ok {
		return nil, errors.New("manifest has an invalid version")
	}
	return &m, nil
}

// Check fetches and verifies the latest manifest. When an update is required
// (critical or below the minimum version) or auto-install is on, it installs.
func (u *Updater) Check(ctx context.Context, allowInstall bool) error {
	if !u.enabled() {
		return errors.New("updates are disabled for development builds")
	}
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return errors.New("an update operation is already running")
	}
	u.busy = true
	u.status.State = "checking"
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.busy = false; u.mu.Unlock() }()

	base := u.base + u.Repo + "/releases/latest/download/"
	raw, err := u.get(ctx, base+"manifest.json", 1<<20)
	if err != nil {
		u.setErr(err)
		return err
	}
	sig, err := u.get(ctx, base+"manifest.json.sig", 4096)
	if err != nil {
		u.setErr(err)
		return err
	}
	m, err := verifyManifest(raw, sig)
	if err != nil {
		u.setErr(err)
		return err
	}
	newer := compareVersions(m.Version, u.Current) > 0
	required := m.MinVersion != "" && compareVersions(u.Current, m.MinVersion) < 0
	u.mu.Lock()
	u.manifest = m
	u.status.LastCheck = time.Now()
	u.status.Latest = m.Version
	u.status.Notes = m.Notes
	u.status.Critical = m.Critical && newer
	u.status.Required = required
	u.status.Available = newer
	u.status.LastError = ""
	u.status.State = "idle"
	u.mu.Unlock()
	if newer && allowInstall && (u.AutoInstall || m.Critical || required) {
		slog.Info("installing update", "from", u.Current, "to", m.Version, "critical", m.Critical, "required", required)
		return u.install(ctx, m, raw, sig)
	}
	return nil
}

// Install downloads and activates the latest verified release (manual action).
func (u *Updater) Install(ctx context.Context) error {
	if err := u.Check(ctx, false); err != nil {
		return err
	}
	u.mu.Lock()
	m := u.manifest
	if u.busy {
		u.mu.Unlock()
		return errors.New("an update operation is already running")
	}
	u.busy = true
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.busy = false; u.mu.Unlock() }()
	if m == nil || compareVersions(m.Version, u.Current) <= 0 {
		return errors.New("already up to date")
	}
	base := u.base + u.Repo + "/releases/download/" + m.Version + "/"
	raw, err := u.get(ctx, base+"manifest.json", 1<<20)
	if err != nil {
		return err
	}
	sig, err := u.get(ctx, base+"manifest.json.sig", 4096)
	if err != nil {
		return err
	}
	m2, err := verifyManifest(raw, sig)
	if err != nil || m2.Version != m.Version {
		return errors.New("release manifest mismatch")
	}
	return u.install(ctx, m2, raw, sig)
}

func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func (u *Updater) install(ctx context.Context, m *Manifest, rawManifest, sig []byte) error {
	final, err := u.fetch(ctx, m, rawManifest, sig)
	if err != nil {
		u.setErr(err)
		return err
	}
	u.mu.Lock()
	u.status.State = "restarting"
	u.mu.Unlock()
	if u.BeforeRestart != nil {
		u.BeforeRestart(m.Version)
	}
	u.cleanup(m.Version)
	return Exec(final, u.BinDir, m.Version)
}

// fetch downloads, verifies and stores the binary of a release.
func (u *Updater) fetch(ctx context.Context, m *Manifest, rawManifest, sig []byte) (string, error) {
	asset, ok := m.Assets[platform()]
	if !ok {
		return "", fmt.Errorf("release %s has no binary for %s", m.Version, platform())
	}
	u.mu.Lock()
	u.status.State = "downloading"
	u.mu.Unlock()
	bin, err := u.get(ctx, u.base+u.Repo+"/releases/download/"+m.Version+"/"+asset.Name, 200<<20)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(bin)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), asset.SHA256) {
		return "", errors.New("downloaded binary does not match the signed checksum")
	}
	if err := os.MkdirAll(u.BinDir, 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(u.BinDir, "invoicer-"+m.Version)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, bin, 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	os.WriteFile(final+".manifest", rawManifest, 0o600)
	os.WriteFile(final+".sig", sig, 0o600)
	os.Remove(final + ".boots")
	os.Remove(final + ".ok")
	return final, nil
}

// Download installs the latest release into the bin directory without
// restarting (used by the "invoicer update" command). It returns the
// installed version, or "" when already up to date.
func (u *Updater) Download(ctx context.Context) (string, error) {
	if !u.enabled() {
		return "", errors.New("updates are disabled for development builds")
	}
	base := u.base + u.Repo + "/releases/latest/download/"
	raw, err := u.get(ctx, base+"manifest.json", 1<<20)
	if err != nil {
		return "", err
	}
	sig, err := u.get(ctx, base+"manifest.json.sig", 4096)
	if err != nil {
		return "", err
	}
	m, err := verifyManifest(raw, sig)
	if err != nil {
		return "", err
	}
	if compareVersions(m.Version, u.Current) <= 0 {
		return "", nil
	}
	if _, err := u.fetch(ctx, m, raw, sig); err != nil {
		return "", err
	}
	u.cleanup(m.Version)
	return m.Version, nil
}

// Newest returns the path and version of the newest healthy verified binary
// installed in binDir that is newer than current, if any.
func Newest(current, binDir string) (string, string) {
	vs := installedVersions(binDir)
	sort.Slice(vs, func(i, j int) bool { return compareVersions(vs[i], vs[j]) > 0 })
	for _, v := range vs {
		if compareVersions(v, current) <= 0 {
			break
		}
		base := filepath.Join(binDir, "invoicer-"+v)
		boots, _ := strconv.Atoi(strings.TrimSpace(readFile(base + ".boots")))
		if _, err := os.Stat(base + ".ok"); err != nil && boots >= 3 {
			continue
		}
		if verifyInstalled(base) == nil {
			return base, v
		}
	}
	return "", ""
}

// cleanup keeps the two most recent installed versions.
func (u *Updater) cleanup(keep string) {
	vs := installedVersions(u.BinDir)
	sort.Slice(vs, func(i, j int) bool { return compareVersions(vs[i], vs[j]) > 0 })
	for i, v := range vs {
		if i < 2 || v == keep {
			continue
		}
		base := filepath.Join(u.BinDir, "invoicer-"+v)
		for _, ext := range []string{"", ".manifest", ".sig", ".boots", ".ok"} {
			os.Remove(base + ext)
		}
	}
}

// Exec replaces the current process with the given binary.
func Exec(path, binDir, version string) error {
	bootsFile := filepath.Join(binDir, "invoicer-"+version+".boots")
	n, _ := strconv.Atoi(strings.TrimSpace(readFile(bootsFile)))
	os.WriteFile(bootsFile, []byte(strconv.Itoa(n+1)), 0o600)
	env := append(os.Environ(), "INVOICER_HANDOFF="+version)
	slog.Info("restarting into new version", "version", version)
	return syscall.Exec(path, append([]string{path}, os.Args[1:]...), env)
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func installedVersions(binDir string) []string {
	matches, _ := filepath.Glob(filepath.Join(binDir, "invoicer-v*"))
	var out []string
	for _, m := range matches {
		v := strings.TrimPrefix(filepath.Base(m), "invoicer-")
		if _, ok := parseVersion(v); ok {
			out = append(out, v)
		}
	}
	return out
}

// Handoff is called first thing at startup. If a newer, verified, healthy
// binary is installed in binDir it replaces the current process with it.
func Handoff(current, binDir string) {
	if os.Getenv("INVOICER_HANDOFF") != "" || PublicKey == "" {
		return
	}
	if path, v := Newest(current, binDir); path != "" {
		if err := Exec(path, binDir, v); err != nil {
			slog.Error("handoff failed", "version", v, "err", err)
		}
	}
}

func verifyInstalled(base string) error {
	raw, err := os.ReadFile(base + ".manifest")
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(base + ".sig")
	if err != nil {
		return err
	}
	m, err := verifyManifest(raw, sig)
	if err != nil {
		return err
	}
	if "invoicer-"+m.Version != filepath.Base(base) {
		return errors.New("version mismatch")
	}
	a, ok := m.Assets[platform()]
	if !ok {
		return errors.New("no asset for platform")
	}
	f, err := os.Open(base)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), a.SHA256) {
		return errors.New("checksum mismatch")
	}
	return nil
}

// MarkHealthy records that the running handed-off version started correctly.
func MarkHealthy(binDir string) {
	v := os.Getenv("INVOICER_HANDOFF")
	if v == "" {
		return
	}
	base := filepath.Join(binDir, "invoicer-"+v)
	os.WriteFile(base+".ok", []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
}

// ---------- versions ----------

type version struct {
	major, minor, patch int
	pre                 string
}

func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return version{}, false
		}
		n[i] = v
	}
	return version{n[0], n[1], n[2], pre}, true
}

func compareVersions(a, b string) int {
	va, oka := parseVersion(a)
	vb, okb := parseVersion(b)
	if !oka || !okb {
		return 0
	}
	for _, d := range []int{va.major - vb.major, va.minor - vb.minor, va.patch - vb.patch} {
		if d != 0 {
			if d > 0 {
				return 1
			}
			return -1
		}
	}
	switch {
	case va.pre == vb.pre:
		return 0
	case va.pre == "":
		return 1
	case vb.pre == "":
		return -1
	case va.pre > vb.pre:
		return 1
	default:
		return -1
	}
}

// CompareVersions is exported for tests and the UI.
func CompareVersions(a, b string) int { return compareVersions(a, b) }
