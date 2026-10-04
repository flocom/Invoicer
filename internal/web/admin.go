package web

import (
	"context"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
	"github.com/flocom/invoicer/internal/updater"
)

type usersData struct {
	Users       []*store.User
	Companies   []*store.Company
	Memberships map[int64]map[int64]bool
	InviteLink  string
	ResetLink   string
	ResetFor    string
}

func (s *Server) usersData(c *Ctx) (*usersData, error) {
	users, err := s.Store.Users()
	if err != nil {
		return nil, err
	}
	cos, _ := s.Store.CompaniesFor(c.User, false)
	d := &usersData{Users: users, Companies: cos, Memberships: map[int64]map[int64]bool{}}
	for _, u := range users {
		d.Memberships[u.ID], _ = s.Store.UserCompanyIDs(u.ID)
	}
	return d, nil
}

func (s *Server) usersPage(c *Ctx) error {
	d, err := s.usersData(c)
	if err != nil {
		return err
	}
	return s.render(c, 200, "users", s.page(c, c.t("users.title"), "users", d))
}

// canManage reports whether the current user may change target.
func canManage(actor, target *store.User) bool {
	if actor.ID == target.ID || target.IsOwner() {
		return false
	}
	if target.Role == store.RoleAdmin {
		return actor.IsOwner()
	}
	return actor.IsAdmin()
}

func (s *Server) formCompanies(c *Ctx) []int64 {
	var ids []int64
	for _, v := range c.R.PostForm["companies"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && s.Store.CanAccessCompany(c.User, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Server) inviteCreate(c *Ctx) error {
	email := strings.ToLower(c.form("email"))
	if _, err := mail.ParseAddress(email); err != nil || strings.ContainsAny(email, "<> ") {
		c.bad("err.email_invalid")
		return c.redirect("/admin/users")
	}
	if _, err := s.Store.UserByEmail(email); err == nil {
		c.bad("err.email_taken")
		return c.redirect("/admin/users")
	}
	role := store.RoleMember
	if c.form("role") == store.RoleAdmin && c.User.IsOwner() {
		role = store.RoleAdmin
	}
	tok := security.Token(32)
	if err := s.Store.CreateInvite(security.HashToken(tok), email, role, s.formCompanies(c), c.User.ID, 7*24*time.Hour); err != nil {
		return err
	}
	c.audit("invite.created", email+" ("+role+")")
	d, err := s.usersData(c)
	if err != nil {
		return err
	}
	d.InviteLink = s.App.BaseURL() + "/invite/" + tok
	p := s.page(c, c.t("users.title"), "users", d)
	p.Flash = &Flash{Kind: "ok", Msg: c.t("users.invite_created", email)}
	return s.render(c, 200, "users", p)
}

func (s *Server) targetUser(c *Ctx) (*store.User, error) {
	u, err := s.Store.User(c.id("uid"))
	if err != nil {
		return nil, err
	}
	if !canManage(c.User, u) {
		return nil, errForbidden
	}
	return u, nil
}

func (s *Server) userUpdate(c *Ctx) error {
	u, err := s.targetUser(c)
	if err != nil {
		return err
	}
	role := c.form("role")
	if role != store.RoleAdmin && role != store.RoleMember {
		role = u.Role
	}
	if role == store.RoleAdmin && !c.User.IsOwner() {
		role = u.Role
	}
	if role != u.Role {
		if err := s.Store.SetUserRole(u.ID, role); err != nil {
			return err
		}
		c.audit("user.role", fmt.Sprintf("%s → %s", u.Email, role))
	}
	disabled := c.form("disabled") == "1"
	if disabled != u.Disabled {
		s.Store.SetUserDisabled(u.ID, disabled)
		c.audit("user.disabled", fmt.Sprintf("%s = %v", u.Email, disabled))
	}
	if err := s.Store.SetUserCompanies(u.ID, s.formCompanies(c)); err != nil {
		return err
	}
	c.ok("flash.saved")
	return c.redirect("/admin/users")
}

func (s *Server) userReset(c *Ctx) error {
	u, err := s.targetUser(c)
	if err != nil {
		return err
	}
	tok := security.Token(32)
	if err := s.Store.CreatePasswordReset(security.HashToken(tok), u.ID, 24*time.Hour); err != nil {
		return err
	}
	c.audit("password.reset_link", u.Email)
	d, err := s.usersData(c)
	if err != nil {
		return err
	}
	d.ResetLink = s.App.BaseURL() + "/reset/" + tok
	d.ResetFor = u.Email
	return s.render(c, 200, "users", s.page(c, c.t("users.title"), "users", d))
}

func (s *Server) userDelete(c *Ctx) error {
	u, err := s.targetUser(c)
	if err != nil {
		return err
	}
	if err := s.Store.DeleteUser(u.ID); err != nil {
		return err
	}
	c.audit("user.deleted", u.Email)
	c.ok("users.deleted")
	return c.redirect("/admin/users")
}

func (s *Server) ownershipTransfer(c *Ctx) error {
	target, err := s.Store.User(c.id("uid"))
	if err != nil || target.ID == c.User.ID || target.Disabled {
		return errForbidden
	}
	if !security.CheckPassword(c.User.PasswordHash, c.R.PostFormValue("current")) {
		c.bad("err.password_current")
		return c.redirect("/admin/users")
	}
	if err := s.Store.TransferOwnership(c.User.ID, target.ID); err != nil {
		return err
	}
	c.audit("ownership.transferred", target.Email)
	c.ok("users.transferred", target.Name)
	return c.redirect("/admin/users")
}

// ---------- system ----------

type systemData struct {
	Update    updater.Status
	Timezone  string
	Zones     []string
	Backups   []backupInfo
	DataDir   string
	DetectedO string
	PublicTLS bool
}

type backupInfo struct {
	Name string
	Size int64
	Time time.Time
}

var commonZones = []string{"UTC", "Europe/Paris", "Europe/London", "Europe/Brussels", "Europe/Zurich", "Europe/Berlin",
	"Europe/Madrid", "Europe/Rome", "Europe/Amsterdam", "Europe/Lisbon", "America/Montreal", "America/Toronto",
	"America/Vancouver", "America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles",
	"America/Sao_Paulo", "Africa/Casablanca", "Asia/Dubai", "Asia/Singapore", "Asia/Tokyo", "Australia/Sydney",
	"Pacific/Auckland", "Indian/Reunion", "America/Guadeloupe", "America/Martinique", "Pacific/Tahiti", "Pacific/Noumea"}

func (s *Server) systemPage(c *Ctx) error {
	d := &systemData{Timezone: s.App.Location().String(), Zones: commonZones, DataDir: s.App.Cfg.DataDir,
		DetectedO: s.detectOrigin(c.R), PublicTLS: s.App.IsPublicHTTPS()}
	if s.App.Updater != nil {
		d.Update = s.App.Updater.Status()
	}
	found := false
	for _, z := range d.Zones {
		if z == d.Timezone {
			found = true
		}
	}
	if !found {
		d.Zones = append([]string{d.Timezone}, d.Zones...)
	}
	files, _ := filepath.Glob(s.App.Cfg.Path("backups", "*.db"))
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			d.Backups = append(d.Backups, backupInfo{Name: filepath.Base(f), Size: st.Size(), Time: st.ModTime()})
		}
	}
	sort.Slice(d.Backups, func(i, j int) bool { return d.Backups[i].Time.After(d.Backups[j].Time) })
	if len(d.Backups) > 20 {
		d.Backups = d.Backups[:20]
	}
	return s.render(c, 200, "system", s.page(c, c.t("system.title"), "system", d))
}

func (s *Server) systemSave(c *Ctx) error {
	if tz := c.form("timezone"); tz != "" {
		if err := s.App.SetTimezone(tz); err != nil {
			c.bad("err.timezone")
			return c.redirect("/admin/system")
		}
	}
	c.audit("system.saved", "timezone="+c.form("timezone"))
	c.ok("flash.saved")
	return c.redirect("/admin/system")
}

// systemDomain records the origin the administrator is currently using as the
// public URL (used in e-mails, payment links and Stripe webhooks).
func (s *Server) systemDomain(c *Ctx) error {
	o := s.detectOrigin(c.R)
	if o == "" {
		return errForbidden
	}
	s.Store.SetSetting("base_url", o)
	c.audit("system.base_url", o)
	c.ok("system.domain_saved", o)
	back := safeNext(c.form("back"))
	return c.redirect(back)
}

func (s *Server) updateCheck(c *Ctx) error {
	ctx, cancel := context.WithTimeout(c.R.Context(), 30*time.Second)
	defer cancel()
	if err := s.App.Updater.Check(ctx, false); err != nil {
		c.flash("err", err.Error())
	} else if st := s.App.Updater.Status(); st.Available {
		c.ok("system.update_available", st.Latest)
	} else {
		c.ok("system.up_to_date")
	}
	return c.redirect("/admin/system")
}

func (s *Server) updateInstall(c *Ctx) error {
	c.audit("system.update_install", "")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.App.Updater.Install(ctx); err != nil {
			s.Store.Audit(0, 0, "", "system.update_failed", err.Error())
		}
	}()
	c.ok("system.update_started")
	return c.redirect("/admin/system")
}

func (s *Server) backupNow(c *Ctx) error {
	name, err := s.Store.Backup(s.App.Cfg.Path("backups"), "manual", 10)
	if err != nil {
		return err
	}
	c.audit("system.backup", filepath.Base(name))
	c.ok("system.backup_done", filepath.Base(name))
	return c.redirect("/admin/system")
}

func (s *Server) auditPage(c *Ctx) error {
	entries, err := s.Store.AuditEntries(300)
	if err != nil {
		return err
	}
	return s.render(c, 200, "audit", s.page(c, c.t("audit.title"), "audit", entries))
}
