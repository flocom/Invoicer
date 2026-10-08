// Package app holds the business services shared by the web handlers and the
// background jobs: PDF generation, e-mail delivery, Stripe payments and
// recurring invoices.
package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flocom/invoicer/internal/config"
	"github.com/flocom/invoicer/internal/geo"
	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/mailer"
	"github.com/flocom/invoicer/internal/money"
	"github.com/flocom/invoicer/internal/pdf"
	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
	"github.com/flocom/invoicer/internal/stripe"
	"github.com/flocom/invoicer/internal/updater"
)

type App struct {
	Cfg     config.Config
	Store   *store.Store
	Box     *security.Box
	Updater *updater.Updater

	mu  sync.RWMutex
	loc *time.Location

	checkoutLocks  sync.Map // invoice id → *sync.Mutex
	customerLocks  sync.Map // "company:client" → *sync.Mutex
	recurringLocks sync.Map // schedule id → *sync.Mutex
}

func New(cfg config.Config, st *store.Store, box *security.Box, up *updater.Updater) *App {
	a := &App{Cfg: cfg, Store: st, Box: box, Updater: up}
	a.loadLocation()
	return a
}

// ---------- time ----------

func (a *App) loadLocation() {
	name := a.Store.Setting("timezone")
	loc := time.Local
	if name != "" {
		if l, err := time.LoadLocation(name); err == nil {
			loc = l
		}
	}
	a.mu.Lock()
	a.loc = loc
	a.mu.Unlock()
}

func (a *App) SetTimezone(name string) error {
	if _, err := time.LoadLocation(name); err != nil {
		return err
	}
	if err := a.Store.SetSetting("timezone", name); err != nil {
		return err
	}
	a.loadLocation()
	return nil
}

func (a *App) Location() *time.Location {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.loc
}

func (a *App) Now() time.Time { return time.Now().In(a.Location()) }
func (a *App) Today() string  { return a.Now().Format("2006-01-02") }

// ---------- public URL (auto-detected domain) ----------

// BaseURL is the public origin recorded from the browser (see web.detectOrigin).
// It is stored rather than read from each request so that background e-mails
// can contain links and so a forged Host header cannot poison them.
func (a *App) BaseURL() string { return a.Store.Setting("base_url") }

// IsPublicHTTPS reports whether Stripe can reach us for webhooks.
func (a *App) IsPublicHTTPS() bool {
	u, err := url.Parse(a.BaseURL())
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback()) {
		return false
	}
	return true
}

func (a *App) PublicURL(inv *store.Invoice) string { return a.BaseURL() + "/i/" + inv.PublicToken }
func (a *App) PayURL(inv *store.Invoice) string    { return a.BaseURL() + "/pay/" + inv.PublicToken }

// ---------- secrets ----------

func aad(c *store.Company, field string) string {
	return "company:" + strconv.FormatInt(c.ID, 10) + ":" + field
}

func (a *App) ResendKey(c *store.Company) string {
	k, err := a.Box.Open(c.ResendKey, aad(c, "resend"))
	if err != nil {
		slog.Error("cannot decrypt Resend key", "company", c.ID, "err", err)
	}
	return k
}

func (a *App) StripeKey(c *store.Company) string {
	k, err := a.Box.Open(c.StripeKey, aad(c, "stripe"))
	if err != nil {
		slog.Error("cannot decrypt Stripe key", "company", c.ID, "err", err)
	}
	return k
}

func (a *App) StripeWebhookSecret(c *store.Company) string {
	k, _ := a.Box.Open(c.StripeWebhookSecret, aad(c, "stripe_whsec"))
	return k
}

func (a *App) SealResend(c *store.Company, key string) []byte {
	return a.Box.Seal(key, aad(c, "resend"))
}

// ---------- PDF ----------

// PDF renders an invoice. Issued invoices use the identities frozen at issue
// time; drafts use the live company and client records.
func (a *App) PDF(co *store.Company, inv *store.Invoice) ([]byte, error) {
	return pdf.Render(a.PDFInput(co, inv))
}

// PDFInput gathers what the invoice PDF shows.
func (a *App) PDFInput(co *store.Company, inv *store.Invoice) pdf.Input {
	in := pdf.Input{Invoice: inv, Company: co, Today: a.Today()}
	if inv.Status == store.StatusDraft || inv.CompanySnapshot == "" {
		in.Seller = store.Party{Name: co.DisplayName(), Address: co.Address, Email: co.Email, Phone: co.Phone, Website: co.Website,
			TaxID: co.TaxID, RegistrationID: co.RegistrationID}
		if cl, err := a.Store.Client(co.ID, inv.ClientID); err == nil {
			in.Buyer = store.Party{Name: cl.Name, ContactName: cl.ContactName, Address: cl.Address, Email: cl.Email, TaxID: cl.TaxID}
		}
	} else {
		in.Seller, in.Buyer = inv.Seller(), inv.Buyer()
	}
	if inv.CardOffered(co) && a.BaseURL() != "" {
		in.PayURL = a.PayURL(inv)
	}
	in.Bank = a.Store.ResolveBank(co.ID, inv.BankAccountID, inv.Currency)
	return in
}

func FileName(inv *store.Invoice) string {
	n := inv.Number
	if n == "" {
		n = fmt.Sprintf("draft-%d", inv.ID)
	}
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '"' || r < 32 {
			return '-'
		}
		return r
	}, n) + ".pdf"
}

// ---------- e-mail ----------

var ErrNoEmail = errors.New("e-mail is not configured for this company (Resend)")
var ErrNoRecipient = errors.New("the client has no e-mail address")

// SendInvoiceEmail sends the invoice (kind "invoice") or a reminder
// ("reminder_before", "reminder_due", "reminder_overdue") with the PDF
// attached. Drafts are issued first.
func (a *App) SendInvoiceEmail(ctx context.Context, co *store.Company, inv *store.Invoice, kind, message string, userID int64, idemKey string) error {
	if !co.HasResend() {
		return ErrNoEmail
	}
	cl, err := a.Store.Client(co.ID, inv.ClientID)
	if err != nil {
		return err
	}
	to := cl.Recipients()
	if len(to) == 0 {
		return ErrNoRecipient
	}
	if inv.Status == store.StatusDraft {
		if inv, err = a.Store.Issue(co.ID, inv.ID); err != nil {
			return err
		}
		a.Store.Audit(userID, co.ID, "", "invoice.issue", inv.Number)
	}
	if inv.Status != store.StatusOpen && kind != "invoice" {
		return errors.New("reminders are only sent for unpaid invoices")
	}
	lang := i18n.Norm(inv.Lang)
	c := mailer.Content{Lang: lang, Kind: kind, CompanyName: co.DisplayName(), Accent: co.AccentColor, ClientName: firstNonEmpty(cl.ContactName, cl.Name),
		Number: inv.Number, Amount: money.Format(inv.Due(), inv.Currency, lang), DueDate: i18n.Date(lang, inv.DueDate),
		Link: a.PublicURL(inv), Message: message}
	if inv.CardPayable(co) && a.BaseURL() != "" {
		c.Link2 = a.PayURL(inv) // straight to Stripe Checkout
	}
	if kind == "reminder_overdue" {
		if d, err := time.Parse("2006-01-02", inv.DueDate); err == nil {
			today, _ := time.Parse("2006-01-02", a.Today())
			c.DaysLate = int(today.Sub(d).Hours() / 24)
		}
	}
	if inv.Status == store.StatusPaid {
		c.Amount = money.Format(inv.Total, inv.Currency, lang)
	}
	// the bank account chosen on the invoice (if any) is repeated in the e-mail
	if inv.Status == store.StatusOpen && inv.Due() > 0 {
		c.Bank = BankLines(a.Store.ResolveBank(co.ID, inv.BankAccountID, inv.Currency), co, inv, lang)
	}
	subject, html, text := mailer.Render(c)
	doc, err := a.PDF(co, inv)
	if err != nil {
		return err
	}
	id, err := mailer.Send(ctx, a.ResendKey(co), mailer.Message{From: co.EmailFrom, To: to, BCC: splitEmails(co.EmailBCC),
		ReplyTo: co.EmailReplyTo, Subject: subject, HTML: html, Text: text, IdempotencyKey: idemKey,
		Attachments: []mailer.Attachment{{Filename: FileName(inv), Content: doc}}})
	log := store.EmailLog{CompanyID: co.ID, InvoiceID: inv.ID, Kind: kind, To: strings.Join(to, ", "), Subject: subject, Status: "sent", ProviderID: id, HTML: html, Text: text,
		Attachments: FileName(inv)}
	if err != nil {
		log.Status, log.Error = "failed", err.Error()
		a.Store.LogEmail(log)
		return err
	}
	a.Store.LogEmail(log)
	if kind == "invoice" {
		a.Store.MarkSent(inv.ID)
	}
	return nil
}

// SendReceipt confirms a payment to the client.
func (a *App) SendReceipt(ctx context.Context, co *store.Company, inv *store.Invoice) {
	if !co.HasResend() {
		return
	}
	cl, err := a.Store.Client(co.ID, inv.ClientID)
	if err != nil || len(cl.Recipients()) == 0 {
		return
	}
	lang := i18n.Norm(inv.Lang)
	subject, html, text := mailer.Render(mailer.Content{Lang: lang, Kind: "receipt", CompanyName: co.DisplayName(), Accent: co.AccentColor,
		ClientName: firstNonEmpty(cl.ContactName, cl.Name), Number: inv.Number, Amount: money.Format(inv.Total, inv.Currency, lang),
		Link: a.PublicURL(inv)})
	id, err := mailer.Send(ctx, a.ResendKey(co), mailer.Message{From: co.EmailFrom, To: cl.Recipients(), BCC: splitEmails(co.EmailBCC),
		ReplyTo: co.EmailReplyTo, Subject: subject, HTML: html, Text: text, IdempotencyKey: "receipt-" + inv.PublicToken})
	log := store.EmailLog{CompanyID: co.ID, InvoiceID: inv.ID, Kind: "receipt", To: strings.Join(cl.Recipients(), ", "), Subject: subject, Status: "sent", ProviderID: id, HTML: html, Text: text}
	if err != nil {
		log.Status, log.Error = "failed", err.Error()
	}
	a.Store.LogEmail(log)
}

func (a *App) SendTestEmail(ctx context.Context, co *store.Company, to, lang string) error {
	if !co.HasResend() {
		return ErrNoEmail
	}
	subject, html, text := mailer.Render(mailer.Content{Lang: lang, Kind: "test", CompanyName: co.DisplayName(), Accent: co.AccentColor})
	id, err := mailer.Send(ctx, a.ResendKey(co), mailer.Message{From: co.EmailFrom, To: []string{to}, ReplyTo: co.EmailReplyTo,
		Subject: subject, HTML: html, Text: text})
	log := store.EmailLog{CompanyID: co.ID, Kind: "test", To: to, Subject: subject, Status: "sent", ProviderID: id, HTML: html, Text: text}
	if err != nil {
		log.Status, log.Error = "failed", err.Error()
	}
	a.Store.LogEmail(log)
	return err
}

// BankLines lists the bank transfer details shown in e-mails (label, value).
func BankLines(b *store.BankAccount, co *store.Company, inv *store.Invoice, lang string) [][2]string {
	if b == nil {
		return nil
	}
	t := func(k string) string { return i18n.T(lang, k) }
	var out [][2]string
	add := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			out = append(out, [2]string{k, v})
		}
	}
	add(t("pdf.account_holder"), firstNonEmpty(b.Holder, co.DisplayName()))
	add(t("pdf.bank"), b.BankName)
	add("IBAN", groupIBAN(b.IBAN))
	add("BIC / SWIFT", b.BIC)
	for _, l := range i18n.Lines(b.Extra) {
		add("", l)
	}
	add(t("pdf.reference"), inv.Number)
	return out
}

func groupIBAN(s string) string {
	s = strings.ToUpper(strings.ReplaceAll(s, " ", ""))
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------- address autocomplete ----------

func (a *App) AddressConfig() geo.Config {
	cfg := geo.Config{Swisstopo: a.Store.Setting("addr_swisstopo") != "0", Provider: a.Store.Setting("addr_provider")}
	if cfg.Provider == "" {
		cfg.Provider = geo.ProviderOSM
	}
	if raw := a.Store.Setting("google_maps_key"); raw != "" {
		if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
			cfg.GoogleKey, _ = a.Box.Open(b, "system:google_maps")
		}
	}
	return cfg
}

// SaveAddressConfig stores the setup; an empty key keeps the current one.
func (a *App) SaveAddressConfig(swisstopo bool, provider, googleKey string, removeKey bool) error {
	sw := "1"
	if !swisstopo {
		sw = "0"
	}
	if err := a.Store.SetSetting("addr_swisstopo", sw); err != nil {
		return err
	}
	if err := a.Store.SetSetting("addr_provider", provider); err != nil {
		return err
	}
	switch {
	case removeKey:
		return a.Store.SetSetting("google_maps_key", "")
	case googleKey != "":
		return a.Store.SetSetting("google_maps_key", base64.StdEncoding.EncodeToString(a.Box.Seal(googleKey, "system:google_maps")))
	}
	return nil
}

func splitEmails(s string) []string {
	var out []string
	for _, e := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// ---------- Stripe ----------

// ConfigureStripe validates the key, registers the webhook endpoint when the
// instance is publicly reachable over HTTPS, and stores everything encrypted.
// It returns a warning message when webhooks could not be configured (payments
// are then detected by polling).
func (a *App) ConfigureStripe(ctx context.Context, co *store.Company, key string) (account string, warning string, err error) {
	key = strings.TrimSpace(key)
	if !stripe.ValidKeyFormat(key) {
		return "", "", errors.New("this does not look like a Stripe secret key (sk_… or rk_…)")
	}
	account, err = stripe.AccountName(ctx, key)
	if err != nil {
		return "", "", err
	}
	if stripe.IsTestKey(key) {
		account += " (test mode)"
	}
	// remove our previous endpoint, if any
	if old := a.StripeKey(co); old != "" && co.StripeWebhookID != "" {
		_ = stripe.DeleteWebhook(ctx, old, co.StripeWebhookID)
	}
	var whID, whSecret string
	if a.IsPublicHTTPS() {
		whID, whSecret, err = stripe.CreateWebhook(ctx, key, a.BaseURL()+"/webhooks/stripe/"+co.PublicID)
		if err != nil {
			warning = "webhook: " + err.Error()
			err = nil
		}
	} else {
		warning = "no_public_https"
	}
	err = a.Store.UpdateCompanyStripe(co.ID, a.Box.Seal(key, aad(co, "stripe")), a.Box.Seal(whSecret, aad(co, "stripe_whsec")), whID, account)
	return account, warning, err
}

func (a *App) DisconnectStripe(ctx context.Context, co *store.Company) error {
	if k := a.StripeKey(co); k != "" && co.StripeWebhookID != "" {
		_ = stripe.DeleteWebhook(ctx, k, co.StripeWebhookID)
	}
	return a.Store.UpdateCompanyStripe(co.ID, nil, nil, "", "")
}

// Checkout returns a Stripe Checkout URL for the remaining amount due,
// reusing a recent session when possible.
func (a *App) Checkout(ctx context.Context, co *store.Company, inv *store.Invoice) (string, error) {
	if !co.HasStripe() {
		return "", errors.New("online payment is not available")
	}
	if inv.Status != store.StatusOpen || inv.Due() <= 0 {
		return "", errors.New("this invoice has nothing left to pay")
	}
	// one session at a time per invoice: concurrent clicks reuse the same one
	lk, _ := a.checkoutLocks.LoadOrStore(inv.ID, &sync.Mutex{})
	lk.(*sync.Mutex).Lock()
	defer lk.(*sync.Mutex).Unlock()
	if ss, err := a.Store.ReusableStripeSession(inv.ID, inv.Due()); err == nil {
		return ss.URL, nil
	}
	// sessions created for another amount must not stay payable
	a.ExpireSessions(ctx, co, inv.ID, "")
	lang := i18n.Norm(inv.Lang)
	cl, _ := a.Store.Client(co.ID, inv.ClientID)
	email, customer := "", ""
	if cl != nil {
		email = cl.Email
		if inv.AutoCharge {
			// save the card for the next invoices of the recurring schedule
			if sc, err := a.EnsureCustomer(ctx, co, cl); err == nil {
				customer = sc.CustomerID
			} else {
				slog.Warn("stripe customer", "client", cl.ID, "err", err)
			}
		}
	}
	s, err := stripe.CreateCheckout(ctx, a.StripeKey(co), stripe.CheckoutParams{
		Amount: inv.Due(), Currency: inv.Currency, Email: email, Customer: customer, Locale: lang,
		ProductName: i18n.T(lang, "pdf.invoice") + " " + inv.Number,
		Description: co.DisplayName(),
		SuccessURL:  a.PublicURL(inv) + "?paid=1",
		CancelURL:   a.PublicURL(inv),
		Metadata:    map[string]string{"invoicer_invoice": strconv.FormatInt(inv.ID, 10), "invoicer_company": co.PublicID, "invoice_number": inv.Number},
	})
	if err != nil {
		return "", err
	}
	if err := a.Store.SaveStripeSession(store.StripeSession{ID: s.ID, InvoiceID: inv.ID, Amount: inv.Due(), URL: s.URL, ExpiresAt: s.ExpiresAt}); err != nil {
		return "", err
	}
	a.logStripe(co, inv, store.StripeLog{Event: "checkout.created", Ref: s.ID, Amount: inv.Due()})
	return s.URL, nil
}

// ExpireSessions closes every still-payable Checkout Session of an invoice
// except `keep` (called when the invoice is voided or its amount due changes).
func (a *App) ExpireSessions(ctx context.Context, co *store.Company, invoiceID int64, keep string) {
	if !co.HasStripe() {
		return
	}
	sessions, err := a.Store.OpenStripeSessions(invoiceID)
	if err != nil {
		return
	}
	key := a.StripeKey(co)
	inv, _ := a.Store.Invoice(co.ID, invoiceID)
	for _, ss := range sessions {
		if ss.ID == keep {
			continue
		}
		if err := stripe.ExpireCheckout(ctx, key, ss.ID); err != nil {
			slog.Warn("could not expire Stripe session", "session", ss.ID, "err", err)
			a.logStripe(co, inv, store.StripeLog{Event: "checkout.expire_failed", Level: "warn", Ref: ss.ID, Detail: err.Error()})
			continue
		}
		a.Store.SetStripeSessionStatus(ss.ID, "expired")
		a.logStripe(co, inv, store.StripeLog{Event: "checkout.closed", Ref: ss.ID, Amount: ss.Amount})
	}
}

// ApplyStripeSession records the payment of a completed Checkout Session.
// It is safe to call several times for the same session.
func (a *App) ApplyStripeSession(ctx context.Context, co *store.Company, s *stripe.Session) error {
	if s.Mode == "setup" {
		return a.ApplySetupSession(ctx, co, s)
	}
	known, err := a.Store.StripeSession(s.ID)
	if err != nil {
		return nil // not one of ours
	}
	inv, err := a.Store.Invoice(co.ID, known.InvoiceID)
	if err != nil {
		return nil
	}
	switch {
	case s.Status == "expired":
		if known.Status == "open" {
			a.Store.SetStripeSessionStatus(s.ID, "expired")
			a.logStripe(co, inv, store.StripeLog{Event: "checkout.expired", Ref: s.ID, Amount: known.Amount})
		}
		return nil
	case s.PaymentStatus != "paid":
		return nil
	}
	if !strings.EqualFold(s.Currency, inv.Currency) {
		a.Store.Audit(0, co.ID, "", "payment.stripe_currency_mismatch", s.ID)
		if known.Status == "open" {
			a.Store.SetStripeSessionStatus(s.ID, "mismatch")
			a.logStripe(co, inv, store.StripeLog{Event: "payment.currency_mismatch", Level: "error", Ref: s.ID, Amount: s.AmountTotal,
				Currency: strings.ToUpper(s.Currency)})
		}
		return nil // do not make Stripe retry: needs a manual check
	}
	if inv.Status == store.StatusVoid {
		// money was taken on a voided invoice: keep a trace so it can be refunded
		a.Store.SetStripeSessionStatus(s.ID, "complete")
		a.Store.Audit(0, co.ID, "", "payment.stripe_on_void_invoice",
			fmt.Sprintf("%s %s — refund needed (%s)", inv.Number, money.Format(s.AmountTotal, inv.Currency, "en"), firstNonEmpty(s.PaymentIntent, s.ID)))
		slog.Error("Stripe payment received for a voided invoice", "invoice", inv.ID, "session", s.ID)
		if known.Status != "complete" {
			a.logStripe(co, inv, store.StripeLog{Event: "payment.on_void", Level: "error", Ref: firstNonEmpty(s.PaymentIntent, s.ID),
				Amount: s.AmountTotal})
		}
		return nil
	}
	if s.AmountTotal > inv.Due() {
		a.Store.Audit(0, co.ID, "", "payment.stripe_overpaid",
			fmt.Sprintf("%s paid %s, due %s", inv.Number, money.Format(s.AmountTotal, inv.Currency, "en"), money.Format(inv.Due(), inv.Currency, "en")))
	}
	recorded, becamePaid, err := a.Store.RecordPaymentOnce(inv.ID, store.Payment{Amount: s.AmountTotal, Method: "stripe",
		Reference: firstNonEmpty(s.PaymentIntent, s.ID), PaidOn: a.Today(), StripeSessionID: s.ID}, 0)
	if err != nil {
		return err
	}
	a.Store.SetStripeSessionStatus(s.ID, "complete")
	if !recorded {
		return nil // already applied (webhook and return page racing)
	}
	a.logStripe(co, inv, store.StripeLog{Event: "payment.received", Level: "ok", Ref: firstNonEmpty(s.PaymentIntent, s.ID), Amount: s.AmountTotal})
	if s.AmountTotal > inv.Due() {
		a.logStripe(co, inv, store.StripeLog{Event: "payment.overpaid", Level: "warn", Ref: firstNonEmpty(s.PaymentIntent, s.ID),
			Amount: s.AmountTotal - inv.Due()})
	}
	a.saveCheckoutCard(ctx, co, inv, s)
	a.ExpireSessions(ctx, co, inv.ID, s.ID)
	a.Store.Audit(0, co.ID, "", "payment.stripe", fmt.Sprintf("%s %s", inv.Number, money.Format(s.AmountTotal, inv.Currency, "en")))
	if becamePaid {
		if inv, err := a.Store.Invoice(co.ID, inv.ID); err == nil {
			a.SendReceipt(ctx, co, inv)
		}
	}
	a.notifyOwner(ctx, co, inv, ownerNote{Kind: "owner_paid", Amount: s.AmountTotal, IdempotencyKey: "owner-paid-" + s.ID})
	return nil
}
