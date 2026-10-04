package web

import (
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
)

// ---------- first-run setup ----------

// LogFirstRun reminds the operator, in the logs, that the instance has no
// account yet: the first account created becomes the owner.
func (s *Server) LogFirstRun() {
	if n, _ := s.Store.UserCount(); n > 0 {
		return
	}
	slog.Info("first start: open Invoicer in your browser and create the owner account (the first account gets full control)")
}

type setupData struct {
	Name, Email, Lang string
}

func (s *Server) setupForm(c *Ctx) error {
	if n, _ := s.Store.UserCount(); n > 0 {
		return c.redirect("/login")
	}
	p := s.page(c, c.t("setup.title"), "", &setupData{Lang: c.Lang})
	return s.render(c, 200, "setup", p)
}

func (s *Server) setupSubmit(c *Ctx) error {
	if n, _ := s.Store.UserCount(); n > 0 {
		return c.redirect("/login")
	}
	d := &setupData{Name: c.form("name"), Email: strings.ToLower(c.form("email")), Lang: i18n.Norm(c.form("lang"))}
	c.Lang = d.Lang
	fail := func(key string) error {
		p := s.page(c, c.t("setup.title"), "", d)
		p.Error = c.t(key)
		return s.render(c, 400, "setup", p)
	}
	if !s.limiter.allow("setup:"+c.IP, 10, 10*time.Minute) {
		return fail("err.rate_limited")
	}
	if d.Name == "" {
		return fail("err.name_required")
	}
	if _, err := mail.ParseAddress(d.Email); err != nil || strings.ContainsAny(d.Email, "<> ") {
		return fail("err.email_invalid")
	}
	pw := c.R.PostFormValue("password")
	if k := security.PasswordProblem(pw); k != "" {
		return fail(k)
	}
	if pw != c.R.PostFormValue("password2") {
		return fail("err.password_mismatch")
	}
	u, err := s.Store.CreateFirstOwner(d.Email, d.Name, security.HashPassword(pw), d.Lang)
	if err != nil {
		return c.redirect("/login")
	}
	if o := s.detectOrigin(c.R); o != "" {
		s.Store.SetSetting("base_url", o)
	}
	if tz := c.form("tz"); tz != "" {
		s.App.SetTimezone(tz)
	}
	s.Store.Audit(u.ID, 0, c.IP, "setup.owner_created", u.Email)
	if err := s.startSession(c, u, false); err != nil {
		return err
	}
	c.Lang = u.Lang
	c.ok("setup.done")
	return c.redirect("/companies/new")
}

// ---------- login ----------

type loginData struct {
	Email, Next string
}

func safeNext(n string) string {
	if !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.Contains(n, "\\") || len(n) > 300 {
		return "/"
	}
	return n
}

func (s *Server) loginForm(c *Ctx) error {
	if n, _ := s.Store.UserCount(); n == 0 {
		return c.redirect("/setup")
	}
	if c.User != nil {
		return c.redirect("/")
	}
	p := s.page(c, c.t("login.title"), "", &loginData{Next: safeNext(c.R.URL.Query().Get("next"))})
	return s.render(c, 200, "login", p)
}

func (s *Server) loginSubmit(c *Ctx) error {
	d := &loginData{Email: strings.ToLower(c.form("email")), Next: safeNext(c.form("next"))}
	fail := func(key string, status int) error {
		p := s.page(c, c.t("login.title"), "", d)
		p.Error = c.t(key)
		return s.render(c, status, "login", p)
	}
	if !s.limiter.allow("login-ip:"+c.IP, 10, 5*time.Minute) || !s.limiter.allow("login-email:"+d.Email, 8, 10*time.Minute) {
		return fail("err.rate_limited", 429)
	}
	pw := c.R.PostFormValue("password")
	u, err := s.Store.UserByEmail(d.Email)
	if err != nil {
		security.CheckPassword("", pw) // constant time
		return fail("err.login", 401)
	}
	if u.LockedUntil > time.Now().Unix() {
		security.CheckPassword("", pw)
		return fail("err.locked", 429)
	}
	if !security.CheckPassword(u.PasswordHash, pw) {
		s.Store.RecordLoginFailure(u.ID)
		s.Store.Audit(u.ID, 0, c.IP, "login.failed", "")
		return fail("err.login", 401)
	}
	if u.Disabled {
		return fail("err.disabled", 403)
	}
	if u.TOTPEnabled {
		if err := s.startSession(c, u, true); err != nil {
			return err
		}
		return c.redirect("/login/2fa?next=" + urlQuery(d.Next))
	}
	s.Store.RecordLoginSuccess(u.ID)
	s.Store.Audit(u.ID, 0, c.IP, "login.success", "")
	if err := s.startSession(c, u, false); err != nil {
		return err
	}
	return c.redirect(d.Next)
}

func (s *Server) mfaForm(c *Ctx) error {
	if c.Sess == nil || !c.Sess.MFAPending {
		return c.redirect("/login")
	}
	p := s.page(c, c.t("mfa.title"), "", &loginData{Next: safeNext(c.R.URL.Query().Get("next"))})
	return s.render(c, 200, "mfa", p)
}

func (s *Server) mfaSubmit(c *Ctx) error {
	if c.Sess == nil || !c.Sess.MFAPending {
		return c.redirect("/login")
	}
	d := &loginData{Next: safeNext(c.form("next"))}
	u, err := s.Store.User(c.Sess.UserID)
	if err != nil {
		return c.redirect("/login")
	}
	fail := func(key string) error {
		p := s.page(c, c.t("mfa.title"), "", d)
		p.Error = c.t(key)
		return s.render(c, 401, "mfa", p)
	}
	if !s.limiter.allow("mfa:"+hex.EncodeToString(c.SessID[:8]), 5, 5*time.Minute) || u.LockedUntil > time.Now().Unix() {
		return fail("err.rate_limited")
	}
	code := strings.ToLower(strings.ReplaceAll(c.form("code"), " ", ""))
	okCode := false
	secret, _ := s.App.Box.Open(u.TOTPSecret, totpAAD(u.ID))
	if ctr, valid := security.CheckTOTP(secret, code, time.Now()); valid && s.Store.UseTOTPCounter(u.ID, int64(ctr)) {
		okCode = true
	} else if len(code) == 11 && consumeRecovery(s.Store, u, code) {
		okCode = true
		s.Store.Audit(u.ID, 0, c.IP, "login.recovery_code_used", "")
	}
	if !okCode {
		s.Store.RecordLoginFailure(u.ID)
		return fail("err.mfa_code")
	}
	s.Store.RecordLoginSuccess(u.ID)
	s.Store.Audit(u.ID, 0, c.IP, "login.success", "2fa")
	// rotate the session after the second factor
	if err := s.startSession(c, u, false); err != nil {
		return err
	}
	return c.redirect(d.Next)
}

func totpAAD(uid int64) string { return "user:" + itoa(uid) + ":totp" }

func consumeRecovery(st *store.Store, u *store.User, code string) bool {
	var hashes []string
	json.Unmarshal([]byte(u.RecoveryCodes), &hashes)
	h := hex.EncodeToString(security.HashToken(code))
	for i, x := range hashes {
		if security.Equal(x, h) {
			hashes = append(hashes[:i], hashes[i+1:]...)
			b, _ := json.Marshal(hashes)
			st.SetRecoveryCodes(u.ID, string(b))
			return true
		}
	}
	return false
}

func (s *Server) logout(c *Ctx) error {
	if c.User != nil {
		c.audit("logout", "")
	}
	s.clearSession(c)
	return c.redirect("/login")
}

// ---------- invitations ----------

type inviteData struct {
	Email, Name, Lang string
}

func (s *Server) inviteForm(c *Ctx) error {
	inv, err := s.Store.Invite(security.HashToken(c.R.PathValue("token")))
	if err != nil {
		s.renderError(c, http.StatusNotFound, "err.invite_invalid")
		return nil
	}
	p := s.page(c, c.t("invite.title"), "", &inviteData{Email: inv.Email, Lang: c.Lang})
	p.Bare = true
	return s.render(c, 200, "invite", p)
}

func (s *Server) inviteSubmit(c *Ctx) error {
	tokHash := security.HashToken(c.R.PathValue("token"))
	inv, err := s.Store.Invite(tokHash)
	if err != nil {
		s.renderError(c, http.StatusNotFound, "err.invite_invalid")
		return nil
	}
	d := &inviteData{Email: inv.Email, Name: c.form("name"), Lang: i18n.Norm(c.form("lang"))}
	c.Lang = d.Lang
	fail := func(key string) error {
		p := s.page(c, c.t("invite.title"), "", d)
		p.Bare = true
		p.Error = c.t(key)
		return s.render(c, 400, "invite", p)
	}
	if !s.limiter.allow("invite:"+c.IP, 10, 10*time.Minute) {
		return fail("err.rate_limited")
	}
	if d.Name == "" {
		return fail("err.name_required")
	}
	pw := c.R.PostFormValue("password")
	if k := security.PasswordProblem(pw); k != "" {
		return fail(k)
	}
	if pw != c.R.PostFormValue("password2") {
		return fail("err.password_mismatch")
	}
	u, err := s.Store.AcceptInvite(tokHash, d.Name, security.HashPassword(pw), d.Lang)
	if err != nil {
		return fail("err.invite_accept")
	}
	s.Store.Audit(u.ID, 0, c.IP, "invite.accepted", u.Email)
	if err := s.startSession(c, u, false); err != nil {
		return err
	}
	c.Lang = u.Lang
	c.ok("invite.welcome")
	return c.redirect("/")
}

// ---------- password reset (links created by an admin or the CLI) ----------

func (s *Server) resetForm(c *Ctx) error {
	u, err := s.Store.PasswordResetUser(security.HashToken(c.R.PathValue("token")))
	if err != nil {
		s.renderError(c, http.StatusNotFound, "err.reset_invalid")
		return nil
	}
	p := s.page(c, c.t("reset.title"), "", map[string]string{"Email": u.Email})
	p.Bare = true
	return s.render(c, 200, "reset", p)
}

func (s *Server) resetSubmit(c *Ctx) error {
	tokHash := security.HashToken(c.R.PathValue("token"))
	u, err := s.Store.PasswordResetUser(tokHash)
	if err != nil {
		s.renderError(c, http.StatusNotFound, "err.reset_invalid")
		return nil
	}
	fail := func(key string) error {
		p := s.page(c, c.t("reset.title"), "", map[string]string{"Email": u.Email})
		p.Bare = true
		p.Error = c.t(key)
		return s.render(c, 400, "reset", p)
	}
	if !s.limiter.allow("reset:"+c.IP, 10, 10*time.Minute) {
		return fail("err.rate_limited")
	}
	pw := c.R.PostFormValue("password")
	if k := security.PasswordProblem(pw); k != "" {
		return fail(k)
	}
	if pw != c.R.PostFormValue("password2") {
		return fail("err.password_mismatch")
	}
	if err := s.Store.ConsumePasswordReset(tokHash, security.HashPassword(pw)); err != nil {
		return fail("err.reset_invalid")
	}
	s.Store.Audit(u.ID, 0, c.IP, "password.reset", "")
	s.clearSession(c)
	c.Lang = u.Lang
	c.ok("reset.done")
	return c.redirect("/login")
}
