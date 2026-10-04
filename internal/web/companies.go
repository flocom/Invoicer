package web

import (
	"bytes"
	"context"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"

	"github.com/flocom/invoicer/internal/brand"
	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/mailer"
	"github.com/flocom/invoicer/internal/money"
	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
)

func (s *Server) home(c *Ctx) error {
	cos, err := s.Store.CompaniesFor(c.User, false)
	if err != nil {
		return err
	}
	if len(cos) == 0 {
		if c.User.IsAdmin() {
			return c.redirect("/companies/new")
		}
		return s.render(c, 200, "nocompany", s.page(c, c.t("nav.companies"), "", nil))
	}
	target := cos[0].ID
	if c.Sess.CompanyID != 0 {
		for _, co := range cos {
			if co.ID == c.Sess.CompanyID {
				target = co.ID
			}
		}
	}
	return c.redirect("/c/" + itoa(target))
}

func (s *Server) companyList(c *Ctx) error {
	cos, err := s.Store.CompaniesFor(c.User, true)
	if err != nil {
		return err
	}
	return s.render(c, 200, "companies", s.page(c, c.t("nav.companies"), "companies", cos))
}

func (s *Server) companyNew(c *Ctx) error {
	return s.render(c, 200, "company_new", s.page(c, c.t("company.new"), "companies", map[string]string{"Lang": c.Lang, "Currency": "EUR"}))
}

func (s *Server) companyCreate(c *Ctx) error {
	name := c.form("name")
	cur := c.form("currency")
	lang := i18n.Norm(c.form("lang"))
	if name == "" || len(name) > 200 || !money.ValidCurrency(cur) {
		p := s.page(c, c.t("company.new"), "companies", map[string]string{"Name": name, "Lang": lang, "Currency": cur})
		p.Error = c.t("err.name_required")
		return s.render(c, 400, "company_new", p)
	}
	co, err := s.Store.CreateCompany(security.Token(12), name, cur, lang, c.User.ID)
	if err != nil {
		return err
	}
	c.Company = co
	c.audit("company.created", name)
	c.ok("company.created")
	return c.redirect("/c/" + itoa(co.ID) + "/settings")
}

// ---------- settings ----------

type settingsData struct {
	Tab           string
	ResendKeySet  bool
	StripeKeySet  bool
	WebhookActive bool
	WebhookURL    string
	PublicHTTPS   bool
	Users         []*store.User
	Members       map[int64]bool
	Reminders     string
	Banks         []*store.BankAccount
}

func (s *Server) companySettings(c *Ctx) error {
	tab := c.R.URL.Query().Get("tab")
	switch tab {
	case "general", "invoicing", "email", "payments", "members":
	default:
		tab = "general"
	}
	d := &settingsData{Tab: tab, ResendKeySet: len(c.Company.ResendKey) > 0, StripeKeySet: c.Company.HasStripe(),
		WebhookActive: c.Company.StripeWebhookID != "", WebhookURL: s.App.BaseURL() + "/webhooks/stripe/" + c.Company.PublicID,
		PublicHTTPS: s.App.IsPublicHTTPS(), Reminders: c.Company.ReminderDays}
	if tab == "payments" {
		d.Banks, _ = s.Store.BankAccounts(c.Company.ID)
	}
	if tab == "members" {
		d.Users, _ = s.Store.Users()
		d.Members, _ = s.Store.CompanyMembers(c.Company.ID)
	}
	return s.render(c, 200, "company_settings", s.page(c, c.t("settings.title"), "settings", d))
}

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (s *Server) companySaveGeneral(c *Ctx) error {
	co := c.Company
	co.Name = clip(c.form("name"), 200)
	if co.Name == "" {
		c.bad("err.name_required")
		return c.redirect(c.cpath("/settings"))
	}
	co.LegalName = clip(c.form("legal_name"), 200)
	co.Address = clip(c.form("address"), 1000)
	co.Email = clip(c.form("email"), 200)
	co.Phone = clip(c.form("phone"), 60)
	co.Website = clip(c.form("website"), 200)
	co.TaxID = clip(c.form("tax_id"), 60)
	co.RegistrationID = clip(c.form("registration_id"), 120)
	if col := c.form("accent_color"); colorRe.MatchString(col) {
		co.AccentColor = strings.ToLower(col)
	}
	if err := s.Store.UpdateCompanyGeneral(co); err != nil {
		return err
	}
	c.audit("company.settings", "general")
	c.ok("flash.saved")
	return c.redirect(c.cpath("/settings?tab=general"))
}

// companySaveLogo accepts PNG, JPEG or GIF up to 2 MB, then decodes, resizes
// and re-encodes it as PNG so no foreign bytes are ever served back.
func (s *Server) companySaveLogo(c *Ctx) error {
	if c.form("remove") == "1" {
		s.Store.SetCompanyLogo(c.Company.ID, nil)
		c.ok("flash.saved")
		return c.redirect(c.cpath("/settings?tab=general"))
	}
	if c.form("detect") == "1" {
		if col, ok := brand.FromLogo(c.Company.Logo); ok {
			s.Store.SetCompanyAccent(c.Company.ID, col)
			c.ok("settings.color_detected", col)
		} else {
			c.bad("settings.color_not_detected")
		}
		return c.redirect(c.cpath("/settings?tab=general"))
	}
	f, _, err := c.R.FormFile("logo")
	if err != nil {
		c.bad("err.logo")
		return c.redirect(c.cpath("/settings?tab=general"))
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 2<<20+1))
	if err != nil || len(raw) > 2<<20 {
		c.bad("err.logo")
		return c.redirect(c.cpath("/settings?tab=general"))
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width > 6000 || cfg.Height > 6000 || cfg.Width*cfg.Height > 24_000_000 {
		c.bad("err.logo")
		return c.redirect(c.cpath("/settings?tab=general"))
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		c.bad("err.logo")
		return c.redirect(c.cpath("/settings?tab=general"))
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > 800 {
		h = h * 800 / w
		w = 800
	}
	if h > 400 {
		w = w * 400 / h
		h = 400
	}
	dst := image.NewNRGBA(image.Rect(0, 0, max(w, 1), max(h, 1)))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, dst); err != nil {
		return err
	}
	if err := s.Store.SetCompanyLogo(c.Company.ID, buf.Bytes()); err != nil {
		return err
	}
	c.audit("company.settings", "logo")
	// the brand colour follows the logo (it can still be changed by hand)
	if col, ok := brand.FromLogo(buf.Bytes()); ok {
		s.Store.SetCompanyAccent(c.Company.ID, col)
		c.ok("settings.logo_color", col)
	} else {
		c.ok("flash.saved")
	}
	return c.redirect(c.cpath("/settings?tab=general"))
}

func (s *Server) companySaveInvoicing(c *Ctx) error {
	co := c.Company
	if cur := c.form("default_currency"); money.ValidCurrency(cur) {
		co.DefaultCurrency = cur
	}
	co.DefaultLang = i18n.Norm(c.form("default_lang"))
	if bp, err := money.ParseRate(c.form("default_tax")); err == nil && bp >= 0 && bp <= 10000 {
		co.DefaultTaxBP = bp
	}
	co.InvoicePrefix = clip(strings.TrimSpace(c.form("invoice_prefix")), 20)
	if n, err := strconv.Atoi(c.form("payment_terms_days")); err == nil && n >= 0 && n <= 365 {
		co.PaymentTermsDays = n
	}
	co.DefaultNotes = clip(c.form("default_notes"), 2000)
	co.Footer = clip(c.form("footer"), 1000)
	co.RemindersEnabled = c.form("reminders_enabled") == "1"
	var parts []string
	seen := map[int]bool{}
	for _, p := range strings.Split(c.form("reminder_days"), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n >= -60 && n <= 365 && !seen[n] {
			seen[n] = true
			parts = append(parts, strconv.Itoa(n))
		}
	}
	co.ReminderDays = strings.Join(parts, ",")
	if err := s.Store.UpdateCompanyInvoicing(co); err != nil {
		return err
	}
	c.audit("company.settings", "invoicing")
	c.ok("flash.saved")
	return c.redirect(c.cpath("/settings?tab=invoicing"))
}

// companySaveBank stores the "bank details in e-mails" default.
func (s *Server) companySaveBank(c *Ctx) error {
	if err := s.Store.SetEmailBankDetails(c.Company.ID, c.form("email_bank_details") == "1"); err != nil {
		return err
	}
	c.audit("company.settings", "bank e-mail option")
	c.ok("flash.saved")
	return c.redirect(c.cpath("/settings?tab=payments#banks"))
}

// bankFromForm validates a bank account form.
func bankFromForm(c *Ctx, b *store.BankAccount) string {
	b.Label = clip(c.form("label"), 80)
	b.Currency = c.form("currency")
	b.Holder = clip(c.form("holder"), 200)
	b.BankName = clip(c.form("bank_name"), 200)
	b.IBAN = clip(strings.ToUpper(strings.ReplaceAll(c.form("iban"), " ", "")), 40)
	b.BIC = clip(strings.ToUpper(strings.ReplaceAll(c.form("bic"), " ", "")), 15)
	b.Extra = clip(c.form("extra"), 600)
	switch {
	case !money.ValidCurrency(b.Currency):
		return "err.currency"
	case b.IBAN == "" && b.Extra == "":
		return "err.bank_empty"
	case b.IBAN != "" && !store.ValidIBAN(b.IBAN):
		return "err.iban"
	case b.BIC != "" && !bicRe.MatchString(b.BIC):
		return "err.bic"
	}
	return ""
}

var bicRe = regexp.MustCompile(`^[A-Z]{6}[A-Z0-9]{2}([A-Z0-9]{3})?$`)

func (s *Server) bankCreate(c *Ctx) error {
	b := &store.BankAccount{CompanyID: c.Company.ID}
	if key := bankFromForm(c, b); key != "" {
		c.bad(key)
		return c.redirect(c.cpath("/settings?tab=payments#banks"))
	}
	if err := s.Store.SaveBankAccount(b); err != nil {
		return err
	}
	c.audit("bank.created", b.Name())
	c.ok("bank.saved")
	return c.redirect(c.cpath("/settings?tab=payments#banks"))
}

func (s *Server) bankUpdate(c *Ctx) error {
	b, err := s.Store.BankAccount(c.Company.ID, c.id("bid"))
	if err != nil {
		return err
	}
	if key := bankFromForm(c, b); key != "" {
		c.bad(key)
		return c.redirect(c.cpath("/settings?tab=payments#banks"))
	}
	if err := s.Store.SaveBankAccount(b); err != nil {
		return err
	}
	c.audit("bank.updated", b.Name())
	c.ok("bank.saved")
	return c.redirect(c.cpath("/settings?tab=payments#banks"))
}

func (s *Server) bankDelete(c *Ctx) error {
	b, err := s.Store.BankAccount(c.Company.ID, c.id("bid"))
	if err != nil {
		return err
	}
	if err := s.Store.DeleteBankAccount(c.Company.ID, b.ID); err != nil {
		return err
	}
	c.audit("bank.deleted", b.Name())
	c.ok("bank.deleted")
	return c.redirect(c.cpath("/settings?tab=payments#banks"))
}

func (s *Server) companySaveEmail(c *Ctx) error {
	from := clip(c.form("email_from"), 200)
	if from != "" {
		if _, err := mail.ParseAddress(from); err != nil {
			c.bad("err.email_from")
			return c.redirect(c.cpath("/settings?tab=email"))
		}
	}
	replyTo := clip(c.form("email_reply_to"), 200)
	bcc := clip(c.form("email_bcc"), 400)
	key := strings.TrimSpace(c.R.PostFormValue("resend_key"))
	keep := key == ""
	if c.form("remove_key") == "1" {
		keep = false
		key = ""
	}
	if key != "" {
		if !strings.HasPrefix(key, "re_") || len(key) > 200 {
			c.bad("err.resend_key")
			return c.redirect(c.cpath("/settings?tab=email"))
		}
		ctx, cancel := context.WithTimeout(c.R.Context(), 15*time.Second)
		defer cancel()
		if err := mailer.CheckKey(ctx, key); err != nil {
			c.flash("err", err.Error())
			return c.redirect(c.cpath("/settings?tab=email"))
		}
	}
	var sealed []byte
	if !keep {
		sealed = s.App.SealResend(c.Company, key)
	}
	if err := s.Store.UpdateCompanyEmail(c.Company.ID, sealed, keep, from, replyTo, bcc); err != nil {
		return err
	}
	c.audit("company.settings", "email")
	c.ok("flash.saved")
	return c.redirect(c.cpath("/settings?tab=email"))
}

func (s *Server) companyTestEmail(c *Ctx) error {
	if !s.limiter.allow("testmail:"+itoa(c.Company.ID), 5, 10*time.Minute) {
		c.bad("err.rate_limited")
		return c.redirect(c.cpath("/settings?tab=email"))
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 30*time.Second)
	defer cancel()
	if err := s.App.SendTestEmail(ctx, c.Company, c.User.Email, c.Lang); err != nil {
		c.flash("err", err.Error())
	} else {
		c.ok("settings.test_sent", c.User.Email)
	}
	return c.redirect(c.cpath("/settings?tab=email"))
}

func (s *Server) companySaveStripe(c *Ctx) error {
	ctx, cancel := context.WithTimeout(c.R.Context(), 30*time.Second)
	defer cancel()
	account, warning, err := s.App.ConfigureStripe(ctx, c.Company, c.R.PostFormValue("stripe_key"))
	if err != nil {
		c.flash("err", err.Error())
		return c.redirect(c.cpath("/settings?tab=payments"))
	}
	c.audit("company.settings", "stripe connected: "+account)
	switch {
	case warning == "no_public_https":
		c.flash("info", c.t("settings.stripe_polling", account))
	case warning != "":
		c.flash("info", c.t("settings.stripe_connected", account)+" — "+warning)
	default:
		c.ok("settings.stripe_connected", account)
	}
	return c.redirect(c.cpath("/settings?tab=payments"))
}

func (s *Server) companyStripeDisconnect(c *Ctx) error {
	ctx, cancel := context.WithTimeout(c.R.Context(), 20*time.Second)
	defer cancel()
	if err := s.App.DisconnectStripe(ctx, c.Company); err != nil {
		return err
	}
	c.audit("company.settings", "stripe disconnected")
	c.ok("flash.saved")
	return c.redirect(c.cpath("/settings?tab=payments"))
}

func (s *Server) companySaveMembers(c *Ctx) error {
	users, err := s.Store.Users()
	if err != nil {
		return err
	}
	selected := map[int64]bool{}
	for _, v := range c.R.PostForm["members"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			selected[id] = true
		}
	}
	for _, u := range users {
		if u.IsAdmin() {
			continue
		}
		ids, _ := s.Store.UserCompanyIDs(u.ID)
		if selected[u.ID] == ids[c.Company.ID] {
			continue
		}
		if selected[u.ID] {
			ids[c.Company.ID] = true
		} else {
			delete(ids, c.Company.ID)
		}
		var list []int64
		for id := range ids {
			list = append(list, id)
		}
		s.Store.SetUserCompanies(u.ID, list)
	}
	c.audit("company.settings", "members")
	c.ok("flash.saved")
	return c.redirect(c.cpath("/settings?tab=members"))
}

func (s *Server) companyArchive(c *Ctx) error {
	archived := c.form("archived") == "1"
	if err := s.Store.SetCompanyArchived(c.Company.ID, archived); err != nil {
		return err
	}
	c.audit("company.archived", strconv.FormatBool(archived))
	c.ok("flash.saved")
	if archived {
		return c.redirect("/companies")
	}
	return c.redirect(c.cpath("/settings"))
}

// companyLogo serves a company logo; it is public because it appears on the
// shared invoice page. Only re-encoded PNGs are ever stored.
func (s *Server) companyLogo(c *Ctx) error {
	co, err := s.Store.CompanyByPublicID(c.R.PathValue("pid"))
	if err != nil || len(co.Logo) == 0 {
		return store.ErrNotFound
	}
	c.W.Header().Set("Content-Type", "image/png")
	c.W.Header().Set("Cache-Control", "public, max-age=300")
	c.W.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	http.ServeContent(c.W, c.R, "logo.png", time.Time{}, bytes.NewReader(co.Logo))
	return nil
}
