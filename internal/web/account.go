package web

import (
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strconv"
	"time"

	"rsc.io/qr"

	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/security"
)

func itoa(v int64) string      { return strconv.FormatInt(v, 10) }
func urlQuery(s string) string { return url.QueryEscape(s) }

type accountData struct {
	TOTPSetup     bool
	TOTPSecret    string
	TOTPQR        []byte
	RecoveryCodes []string
	RecoveryLeft  int
}

func (s *Server) accountData(c *Ctx) *accountData {
	d := &accountData{}
	var hashes []string
	json.Unmarshal([]byte(c.User.RecoveryCodes), &hashes)
	d.RecoveryLeft = len(hashes)
	return d
}

func (s *Server) accountPage(c *Ctx) error {
	return s.render(c, 200, "account", s.page(c, c.t("account.title"), "account", s.accountData(c)))
}

func (s *Server) accountProfile(c *Ctx) error {
	name := c.form("name")
	if name == "" || len(name) > 120 {
		c.bad("err.name_required")
		return c.redirect("/account")
	}
	lang := i18n.Norm(c.form("lang"))
	if err := s.Store.UpdateUserProfile(c.User.ID, name, lang); err != nil {
		return err
	}
	c.Lang = lang
	c.ok("flash.saved")
	return c.redirect("/account")
}

func (s *Server) accountPassword(c *Ctx) error {
	if !s.limiter.allow("pw:"+itoa(c.User.ID), 5, 10*time.Minute) {
		c.bad("err.rate_limited")
		return c.redirect("/account")
	}
	if !security.CheckPassword(c.User.PasswordHash, c.R.PostFormValue("current")) {
		c.bad("err.password_current")
		return c.redirect("/account")
	}
	pw := c.R.PostFormValue("password")
	if k := security.PasswordProblem(pw); k != "" {
		c.bad(k)
		return c.redirect("/account")
	}
	if pw != c.R.PostFormValue("password2") {
		c.bad("err.password_mismatch")
		return c.redirect("/account")
	}
	if err := s.Store.SetPassword(c.User.ID, security.HashPassword(pw)); err != nil {
		return err
	}
	s.Store.DeleteUserSessions(c.User.ID, c.SessID)
	c.audit("password.changed", "")
	c.ok("account.password_changed")
	return c.redirect("/account")
}

func (s *Server) mfaStart(c *Ctx) error {
	if c.User.TOTPEnabled {
		return c.redirect("/account")
	}
	secret := security.NewTOTPSecret()
	if err := s.Store.SetTOTP(c.User.ID, s.App.Box.Seal(secret, totpAAD(c.User.ID)), false, ""); err != nil {
		return err
	}
	d := s.accountData(c)
	d.TOTPSetup = true
	d.TOTPSecret = secret
	if code, err := qr.Encode(security.TOTPURI("Invoicer", c.User.Email, secret), qr.M); err == nil {
		code.Scale = 6
		d.TOTPQR = code.PNG()
	}
	return s.render(c, 200, "account", s.page(c, c.t("account.title"), "account", d))
}

func (s *Server) mfaEnable(c *Ctx) error {
	u, _ := s.Store.User(c.User.ID)
	secret, err := s.App.Box.Open(u.TOTPSecret, totpAAD(u.ID))
	if err != nil || secret == "" || u.TOTPEnabled {
		return c.redirect("/account")
	}
	ctr, ok := security.CheckTOTP(secret, c.form("code"), time.Now())
	if !ok {
		c.bad("err.mfa_code")
		return c.redirect("/account")
	}
	codes := security.RecoveryCodes(10)
	hashes := make([]string, len(codes))
	for i, k := range codes {
		hashes[i] = hex.EncodeToString(security.HashToken(k))
	}
	b, _ := json.Marshal(hashes)
	if err := s.Store.SetTOTP(u.ID, u.TOTPSecret, true, string(b)); err != nil {
		return err
	}
	s.Store.UseTOTPCounter(u.ID, int64(ctr))
	c.audit("2fa.enabled", "")
	c.User.TOTPEnabled = true
	d := s.accountData(c)
	d.RecoveryCodes = codes
	d.RecoveryLeft = len(codes)
	p := s.page(c, c.t("account.title"), "account", d)
	p.Flash = &Flash{Kind: "ok", Msg: c.t("account.2fa_enabled")}
	return s.render(c, 200, "account", p)
}

func (s *Server) mfaDisable(c *Ctx) error {
	if !security.CheckPassword(c.User.PasswordHash, c.R.PostFormValue("current")) {
		c.bad("err.password_current")
		return c.redirect("/account")
	}
	if err := s.Store.SetTOTP(c.User.ID, nil, false, ""); err != nil {
		return err
	}
	c.audit("2fa.disabled", "")
	c.ok("account.2fa_disabled")
	return c.redirect("/account")
}

func (s *Server) revokeSessions(c *Ctx) error {
	s.Store.DeleteUserSessions(c.User.ID, c.SessID)
	c.audit("sessions.revoked", "")
	c.ok("account.sessions_revoked")
	return c.redirect("/account")
}
