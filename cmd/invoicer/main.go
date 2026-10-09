// Command invoicer is the whole application: web server, background jobs and
// self-updater in a single static binary.
//
//	invoicer                       start the server (default)
//	invoicer healthcheck           exit 0 if the local server answers
//	invoicer reset-password EMAIL  print a one-time password reset link
//	invoicer update                download the latest signed release and restart into it
//	invoicer backup FILE           write a full encrypted backup (to move to another server)
//	invoicer restore FILE [URL]    restore a full backup at the next start (URL: new public address)
//	invoicer version
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"golang.org/x/crypto/acme/autocert"

	"github.com/flocom/invoicer/internal/app"
	"github.com/flocom/invoicer/internal/backup"
	"github.com/flocom/invoicer/internal/config"
	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
	"github.com/flocom/invoicer/internal/updater"
	"github.com/flocom/invoicer/internal/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	cfg := config.Load()
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println(config.Version)
		return
	case "healthcheck":
		os.Exit(healthcheck(cfg))
	case "reset-password":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: invoicer reset-password EMAIL")
			os.Exit(2)
		}
		os.Exit(resetPassword(cfg, os.Args[2]))
	case "update":
		os.Exit(forceUpdate(cfg))
	case "backup", "restore":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: invoicer backup FILE | invoicer restore FILE [https://new.address]")
			os.Exit(2)
		}
		if cmd == "backup" {
			os.Exit(backupCmd(cfg, os.Args[2]))
		}
		os.Exit(restoreCmd(cfg, os.Args[2], os.Args[3:]))
	case "", "serve":
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd)
		os.Exit(2)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fatal("cannot create data directory", err)
	}
	// Run a newer verified binary installed by the self-updater, if any.
	updater.Handoff(config.Version, cfg.Path("bin"))
	if err := serve(cfg); err != nil {
		fatal("server stopped", err)
	}
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

func serve(cfg config.Config) error {
	slog.Info("starting Invoicer", "version", config.Version, "data", cfg.DataDir)
	if applied, err := backup.ApplyPending(cfg.DataDir); err != nil {
		return fmt.Errorf("restoring the backup failed: %w", err)
	} else if applied {
		slog.Info("backup restored; the previous data was kept in backups/")
	}
	st, err := store.Open(cfg.Path("invoicer.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	box, err := security.LoadBox(cfg.DataDir, cfg.MasterKey)
	if err != nil {
		return err
	}
	up := updater.New(config.Repo, config.Version, cfg.Path("bin"), cfg.AutoUpdate)
	a := app.New(cfg, st, box, up)
	var restartOnce sync.Once
	srv, err := web.New(a)
	if err != nil {
		return err
	}
	handler := srv.Handler()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var servers []*http.Server
	var mu sync.Mutex
	newServer := func(addr string, h http.Handler) *http.Server {
		s := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second,
			WriteTimeout: 120 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
		mu.Lock()
		servers = append(servers, s)
		mu.Unlock()
		return s
	}
	shutdown := func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mu.Lock()
		web.Shutdown(c, servers...)
		mu.Unlock()
	}
	a.Restart = func() {
		restartOnce.Do(func() {
			slog.Info("restarting")
			shutdown()
			st.Close()
			exe, err := os.Executable()
			if err == nil {
				err = syscall.Exec(exe, os.Args, os.Environ())
			}
			// the container's restart policy brings the server back
			slog.Error("restart failed, exiting", "err", err)
			os.Exit(1)
		})
	}
	up.BeforeRestart = func(v string) {
		if _, err := st.Backup(cfg.Path("backups"), "pre-"+v, 5); err != nil {
			slog.Error("pre-update backup failed", "err", err)
		}
		shutdown()
		st.Close()
	}

	errc := make(chan error, 2)
	if cfg.TLS == "auto" {
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(cfg.Path("certs")),
			Email:      os.Getenv("ACME_EMAIL"),
			HostPolicy: hostPolicy(a),
		}
		httpsSrv := newServer(cfg.HTTPSAddr, handler)
		httpsSrv.TLSConfig = &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2", "http/1.1", "acme-tls/1"}}
		httpSrv := newServer(cfg.HTTPAddr, m.HTTPHandler(nil)) // ACME challenges + redirect to https
		go func() { errc <- httpSrv.ListenAndServe() }()
		go func() { errc <- httpsSrv.ListenAndServeTLS("", "") }()
		slog.Info("listening with automatic HTTPS", "http", cfg.HTTPAddr, "https", cfg.HTTPSAddr)
	} else {
		s := newServer(cfg.HTTPAddr, handler)
		go func() { errc <- s.ListenAndServe() }()
		slog.Info("listening", "addr", cfg.HTTPAddr)
	}
	srv.LogFirstRun()

	// SIGHUP (sent by "invoicer update") restarts into the newest installed binary
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			path, v := updater.Newest(config.Version, cfg.Path("bin"))
			if path == "" {
				slog.Info("SIGHUP: no newer binary installed")
				continue
			}
			up.BeforeRestart(v)
			if err := updater.Exec(path, cfg.Path("bin"), v); err != nil {
				slog.Error("restart failed", "err", err)
			}
		}
	}()

	// SIGUSR1 (sent by "invoicer restore") restarts to swap in the restored data
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	go func() {
		<-usr1
		if backup.Pending(cfg.DataDir) {
			a.Restart()
		}
	}()

	go a.RunScheduler(ctx)
	go func() {
		// a version that runs 60 s without crashing is marked healthy
		select {
		case <-time.After(60 * time.Second):
			updater.MarkHealthy(cfg.Path("bin"))
		case <-ctx.Done():
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdown()
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			// closed by the updater: wait for the exec to happen
			time.Sleep(30 * time.Second)
			return nil
		}
		return err
	}
}

// hostPolicy only allows certificates for real host names, and once the
// public URL is known, only for that host.
func hostPolicy(a *app.App) autocert.HostPolicy {
	var mu sync.Mutex
	seen := map[string]time.Time{}
	return func(ctx context.Context, host string) error {
		host = strings.ToLower(host)
		if net.ParseIP(host) != nil || !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") {
			return errors.New("host not allowed")
		}
		if d := os.Getenv("DOMAIN"); d != "" {
			if strings.EqualFold(d, host) {
				return nil
			}
			return errors.New("host not allowed")
		}
		if b := a.BaseURL(); b != "" {
			if strings.EqualFold(strings.TrimPrefix(strings.TrimPrefix(b, "https://"), "http://"), host) {
				return nil
			}
			return errors.New("host not allowed")
		}
		// before setup: at most 3 names at a time, each slot freed after 10 minutes
		// so bogus SNI names cannot block the real domain for long (set DOMAIN to
		// avoid this window entirely)
		mu.Lock()
		defer mu.Unlock()
		for h, t := range seen {
			if time.Since(t) > 10*time.Minute {
				delete(seen, h)
			}
		}
		if _, ok := seen[host]; !ok && len(seen) >= 3 {
			return errors.New("host not allowed")
		}
		seen[host] = time.Now()
		return nil
	}
}

// forceUpdate is run with "docker exec invoicer /app/invoicer update".
func forceUpdate(cfg config.Config) int {
	up := updater.New(config.Repo, config.Version, cfg.Path("bin"), true)
	if path, v := updater.Newest(config.Version, cfg.Path("bin")); path != "" {
		fmt.Println("running", config.Version, "but", v, "is already installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	v, err := up.Download(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "update failed:", err)
		return 1
	}
	if v == "" {
		if path, _ := updater.Newest(config.Version, cfg.Path("bin")); path == "" {
			fmt.Println("already up to date:", config.Version)
			return 0
		}
	} else {
		fmt.Println("downloaded and verified", v)
	}
	// ask the server (PID 1 in the container) to restart into it
	if err := syscall.Kill(1, syscall.SIGHUP); err != nil {
		fmt.Println("restart the container to finish the update:", err)
		return 0
	}
	fmt.Println("the server is restarting into the new version")
	return 0
}

func healthcheck(cfg config.Config) int {
	addr := cfg.HTTPAddr
	scheme := "http" // in TLS mode the plain listener answers with a redirect, which is fine
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(scheme + "://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return 1
	}
	return 0
}

func resetPassword(cfg config.Config, email string) int {
	st, err := store.Open(cfg.Path("invoicer.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	u, err := st.UserByEmail(email)
	if err != nil {
		fmt.Fprintln(os.Stderr, "no user with this e-mail")
		return 1
	}
	tok := security.Token(32)
	if err := st.CreatePasswordReset(security.HashToken(tok), u.ID, time.Hour); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// lift any lockout so the owner can get back in
	st.RecordLoginSuccess(u.ID)
	st.Audit(0, 0, "cli", "password.reset_link", u.Email)
	base := st.Setting("base_url")
	if base == "" {
		base = "http://localhost:8080"
	}
	fmt.Printf("Password reset link for %s (valid 1 hour):\n%s/reset/%s\n", u.Email, base, tok)
	return 0
}

// passphrase reads the backup passphrase from INVOICER_BACKUP_PASSPHRASE or
// from the terminal.
func passphrase(confirm bool) string {
	if p := os.Getenv("INVOICER_BACKUP_PASSPHRASE"); p != "" {
		return p
	}
	in := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stderr, "Backup passphrase: ")
	p, _ := in.ReadString('\n')
	p = strings.TrimRight(p, "\r\n")
	if confirm {
		fmt.Fprint(os.Stderr, "Again: ")
		q, _ := in.ReadString('\n')
		if strings.TrimRight(q, "\r\n") != p {
			fmt.Fprintln(os.Stderr, "the passphrases differ")
			os.Exit(1)
		}
	}
	return p
}

// backupCmd is run with "docker exec -it invoicer /app/invoicer backup /data/x.invbak".
func backupCmd(cfg config.Config, path string) int {
	st, err := store.Open(cfg.Path("invoicer.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	box, err := security.LoadBox(cfg.DataDir, cfg.MasterKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := backup.Write(f, st, box, config.Version, passphrase(true)); err != nil {
		f.Close()
		os.Remove(path)
		fmt.Fprintln(os.Stderr, "backup failed:", err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st.Audit(0, 0, "cli", "system.full_backup", filepath.Base(path))
	fmt.Println("full backup written to", path)
	return 0
}

// restoreCmd stages a backup; the server swaps it in when it next starts.
func restoreCmd(cfg config.Config, path string, rest []string) int {
	newURL := ""
	if len(rest) > 0 {
		u, err := url.Parse(strings.TrimRight(rest[0], "/"))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" {
			fmt.Fprintln(os.Stderr, "the new address must look like https://invoices.example.com")
			return 2
		}
		newURL = u.Scheme + "://" + strings.ToLower(u.Host)
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	b, err := backup.Read(f, passphrase(false))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	box, err := security.LoadBox(cfg.DataDir, cfg.MasterKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := backup.Stage(cfg.DataDir, b, box, newURL); err != nil {
		fmt.Fprintln(os.Stderr, "restore failed:", err)
		return 1
	}
	m := b.Manifest
	fmt.Printf("Backup of %s (%s, %d companies, %d invoices) is ready.\n", m.BaseURL,
		time.Unix(m.CreatedAt, 0).Format("2006-01-02 15:04"), m.Companies, m.Invoices)
	// in the container the server is PID 1: ask it to restart into the restored data
	if pid1IsInvoicer() && syscall.Kill(1, syscall.SIGUSR1) == nil {
		fmt.Println("The server is restarting with the restored data.")
	} else {
		fmt.Println("Restart Invoicer to finish (docker restart invoicer).")
	}
	if newURL == "" {
		fmt.Println("The public address will be recorded at the owner's first sign-in.")
	}
	return 0
}

// pid1IsInvoicer tells whether the server is PID 1 (the Docker image), so
// it can be signalled; elsewhere PID 1 is the system's init.
func pid1IsInvoicer() bool {
	if os.Getpid() == 1 {
		return false
	}
	b, err := os.ReadFile("/proc/1/cmdline")
	return err == nil && strings.Contains(filepath.Base(strings.SplitN(string(b), "\x00", 2)[0]), "invoicer")
}
