package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/flocom/invoicer/internal/backup"
	"github.com/flocom/invoicer/internal/config"
	"github.com/flocom/invoicer/internal/security"
)

// ---------- full backup and restore (moving to another server) ----------

// movedPath tells the public pages that follow a server that moved.
func movedPath(p string) bool {
	for _, pre := range []string{"/i/", "/pay/", "/card/", "/logo/"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// mayUploadBackup lets a backup file through the request size limit only
// for those who can restore it: the owner, or anyone on a server without
// accounts yet. Others cannot make the server store a large upload.
func (s *Server) mayUploadBackup(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/setup/restore":
		n, err := s.Store.UserCount()
		return err == nil && n == 0
	case "/admin/system/restore":
		c := &Ctx{R: r, s: s}
		s.loadSession(c)
		return c.User != nil && c.User.IsOwner()
	}
	return false
}

// checkOwnerPassword guards the system actions that can leak or replace
// every piece of data.
func (s *Server) checkOwnerPassword(c *Ctx) bool {
	if !s.limiter.allow("pw:"+itoa(c.User.ID), 5, 10*time.Minute) {
		c.bad("err.rate_limited")
		return false
	}
	if !security.CheckPassword(c.User.PasswordHash, c.R.PostFormValue("current")) {
		c.bad("err.password_current")
		return false
	}
	return true
}

// fullBackup downloads the whole instance (data and the key of its stored
// secrets), encrypted with a passphrase.
func (s *Server) fullBackup(c *Ctx) error {
	back := "/admin/system#migrate"
	if !s.checkOwnerPassword(c) {
		return c.redirect(back)
	}
	pass := c.R.PostFormValue("passphrase")
	if len([]rune(pass)) < backup.MinPassphrase {
		c.bad("migrate.passphrase_short", backup.MinPassphrase)
		return c.redirect(back)
	}
	if pass != c.R.PostFormValue("passphrase2") {
		c.bad("err.password_mismatch")
		return c.redirect(back)
	}
	var buf bytes.Buffer
	if err := backup.Write(&buf, s.Store, s.App.Box, config.Version, pass); err != nil {
		return err
	}
	name := fmt.Sprintf("invoicer-%s.invbak", s.App.Now().Format("2006-01-02-1504"))
	c.audit("system.full_backup", name)
	h := c.W.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", fmt.Sprint(buf.Len()))
	_, err := c.W.Write(buf.Bytes())
	return err
}

// readUploadedBackup decrypts the backup sent with the form.
func (s *Server) readUploadedBackup(c *Ctx) (*backup.Bundle, string) {
	f, _, err := c.R.FormFile("file")
	if err != nil {
		return nil, c.t("migrate.no_file")
	}
	defer f.Close()
	b, err := backup.Read(f, c.R.PostFormValue("passphrase"))
	switch {
	case errors.Is(err, backup.ErrPassphrase):
		return nil, c.t("migrate.bad_passphrase")
	case errors.Is(err, backup.ErrNotBackup):
		return nil, c.t("migrate.not_backup")
	case errors.Is(err, backup.ErrNewerSchema):
		return nil, c.t("migrate.newer")
	case err != nil:
		return nil, err.Error()
	}
	return b, ""
}

// stageAndRestart prepares the restore and restarts into it once the
// waiting page has been sent.
func (s *Server) stageAndRestart(c *Ctx, b *backup.Bundle) error {
	if err := backup.Stage(s.App.Cfg.DataDir, b, s.App.Box, s.detectOrigin(c.R), c.form("move_webhooks") == "1"); err != nil {
		return err
	}
	p := s.page(c, c.t("migrate.restoring_title"), "", map[string]any{"Boot": s.bootID, "M": b.Manifest,
		"Created": time.Unix(b.Manifest.CreatedAt, 0)})
	p.Bare = true
	if err := s.render(c, 200, "restoring", p); err != nil {
		return err
	}
	if s.App.Restart != nil {
		go func() {
			time.Sleep(time.Second)
			s.App.Restart()
		}()
	}
	return nil
}

func (s *Server) systemRestore(c *Ctx) error {
	back := "/admin/system#migrate"
	if !s.checkOwnerPassword(c) {
		return c.redirect(back)
	}
	if c.form("confirm") != "1" {
		c.bad("migrate.confirm_required")
		return c.redirect(back)
	}
	b, msg := s.readUploadedBackup(c)
	if b == nil {
		c.flash("err", msg)
		return c.redirect(back)
	}
	c.audit("system.restore", fmt.Sprintf("backup of %s from %s", b.Manifest.BaseURL, time.Unix(b.Manifest.CreatedAt, 0).UTC().Format(time.RFC3339)))
	if err := s.stageAndRestart(c, b); err != nil {
		c.flash("err", err.Error())
		return c.redirect(back)
	}
	return nil
}

// ---------- restore on a brand new server (before any account exists) ----------

func (s *Server) setupRestoreForm(c *Ctx) error {
	if n, _ := s.Store.UserCount(); n > 0 {
		return c.redirect("/login")
	}
	return s.render(c, 200, "setup_restore", s.page(c, c.t("migrate.setup_title"), "", nil))
}

func (s *Server) setupRestore(c *Ctx) error {
	if n, _ := s.Store.UserCount(); n > 0 {
		return c.redirect("/login")
	}
	fail := func(msg string) error {
		p := s.page(c, c.t("migrate.setup_title"), "", nil)
		p.Error = msg
		return s.render(c, 400, "setup_restore", p)
	}
	if !s.limiter.allow("setup:"+c.RL, 10, 10*time.Minute) {
		return fail(c.t("err.rate_limited"))
	}
	b, msg := s.readUploadedBackup(c)
	if b == nil {
		return fail(msg)
	}
	if err := s.stageAndRestart(c, b); err != nil {
		return fail(err.Error())
	}
	return nil
}

// systemWebhooksMove takes the Stripe webhooks over from the original
// server, for a restored copy that held them.
func (s *Server) systemWebhooksMove(c *Ctx) error {
	back := "/admin/system#migrate"
	if !s.checkOwnerPassword(c) {
		return c.redirect(back)
	}
	s.Store.SetSetting("stripe_webhooks_hold", "")
	s.Store.SetSetting("stripe_webhooks_refresh", "1")
	c.audit("system.webhooks_move", s.App.BaseURL())
	if !s.App.IsPublicHTTPS() {
		c.ok("migrate.webhooks_later")
		return c.redirect(back)
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), time.Minute)
	defer cancel()
	s.App.RefreshWebhooksIfMoved(ctx)
	if s.Store.Setting("stripe_webhooks_refresh") == "" {
		c.ok("migrate.webhooks_moved")
	} else {
		c.ok("migrate.webhooks_retry")
	}
	return c.redirect(back)
}

// ---------- the old server, once moved ----------

// systemMoved records (or clears) the new address of this instance: public
// links then redirect there and automatic invoicing stops here.
func (s *Server) systemMoved(c *Ctx) error {
	back := "/admin/system#migrate"
	if !s.checkOwnerPassword(c) {
		return c.redirect(back)
	}
	to := ""
	if c.form("clear") != "1" {
		u, err := url.Parse(strings.TrimRight(strings.TrimSpace(c.form("moved_to")), "/"))
		if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || !validHost(strings.ToLower(u.Host)) {
			c.bad("migrate.moved_invalid")
			return c.redirect(back)
		}
		to = "https://" + strings.ToLower(u.Host)
		if to == s.App.BaseURL() {
			c.bad("migrate.moved_same")
			return c.redirect(back)
		}
	}
	s.Store.SetSetting("moved_to", to)
	c.audit("system.moved", to)
	if to == "" {
		c.ok("migrate.moved_cleared")
	} else {
		c.ok("migrate.moved_saved", to)
	}
	return c.redirect(back)
}
