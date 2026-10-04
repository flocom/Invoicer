package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/app"
	"github.com/flocom/invoicer/internal/config"
	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	app *app.App
	web *Server
}

type browser struct {
	e      *env
	c      *http.Client
	csrf   string
	last   string
	status int
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir}
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := security.LoadBox(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(cfg, st, box, nil)
	s, err := New(a)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, app: a, web: s}
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}}
}

func (b *browser) get(path string) string {
	b.e.t.Helper()
	resp, err := b.c.Get(b.e.srv.URL + path)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	b.status = resp.StatusCode
	b.last = string(body)
	if m := csrfRe.FindStringSubmatch(b.last); m != nil {
		b.csrf = m[1]
	}
	return b.last
}

func (b *browser) post(path string, form url.Values) string {
	b.e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if form.Get("csrf") == "" {
		form.Set("csrf", b.csrf)
	}
	req, _ := http.NewRequest("POST", b.e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.e.srv.URL)
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	b.status = resp.StatusCode
	b.last = string(body)
	if m := csrfRe.FindStringSubmatch(b.last); m != nil {
		b.csrf = m[1]
	}
	return b.last
}

func (b *browser) must(substr string) {
	b.e.t.Helper()
	if !strings.Contains(b.last, substr) {
		snippet := b.last
		if len(snippet) > 3000 {
			snippet = snippet[:3000]
		}
		b.e.t.Fatalf("expected %q in response (status %d):\n%s", substr, b.status, snippet)
	}
}

const pw = "correct horse battery staple"

func setupOwner(t *testing.T, e *env) *browser {
	b := e.browser()
	b.get("/")
	b.must("Welcome to Invoicer")
	b.post("/setup", url.Values{"name": {"Owner"}, "email": {"owner@example.test"},
		"password": {pw}, "password2": {pw}, "lang": {"en"}, "tz": {"Europe/Paris"}})
	b.must("New company")
	return b
}

func TestEndToEnd(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)

	// a second setup attempt is impossible
	other := e.browser()
	other.get("/setup")
	other.must("Sign in")

	// company
	b.post("/companies/new", url.Values{"name": {"Acme"}, "currency": {"EUR"}, "lang": {"en"}})
	b.must("Company settings")
	b.post("/c/1/settings/invoicing", url.Values{"default_currency": {"EUR"}, "default_lang": {"en"}, "default_tax": {"20"},
		"invoice_prefix": {"AC-"}, "payment_terms_days": {"30"},
		"reminders_enabled": {"1"}, "reminder_days": {"-3, 0, 7, abc"}, "footer": {"Late fee 40 €"}})
	b.must("Changes saved")
	b.post("/c/1/settings/bank", url.Values{"iban": {"FR76 3000 6000 0112 3456 7890 189"}, "bic": {"AGRIFRPP"}, "email_bank_details": {"1"}})
	b.must("Changes saved")
	co, _ := e.app.Store.Company(1)
	if co.ReminderDays != "-3,0,7" || co.DefaultTaxBP != 2000 || co.IBAN != "FR7630006000011234567890189" {
		t.Fatalf("invoicing settings not saved: %+v", co)
	}

	// client (French)
	b.post("/c/1/clients/new", url.Values{"name": {"Client SARL"}, "email": {"client@example.test"}, "lang": {"fr"}, "currency": {""},
		"address": {"1 place Bellecour\n69002 Lyon"}})
	b.must("Client SARL")

	// invoice draft
	today := e.app.Today()
	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"fr"}, "issue_date": {today}, "due_date": {due},
		"line_desc": {"Développement", "Hébergement"}, "line_qty": {"2,5", "1"}, "line_price": {"400", "19.99"}, "line_tax": {"20", "5.5"},
		"reminders": {"1"}, "action": {"save"}})
	b.must("Draft saved")
	inv, err := e.app.Store.Invoice(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	// 2.5 × 400 = 1000.00 ; 19.99 ; VAT 200.00 + 1.10 ; total 1221.09
	if inv.Subtotal != 101999 || inv.TaxTotal != 20110 || inv.Total != 122109 {
		t.Fatalf("totals: %d %d %d", inv.Subtotal, inv.TaxTotal, inv.Total)
	}
	if inv.Number != "" {
		t.Fatal("drafts must not have a number")
	}

	// PDF of a draft
	resp, _ := b.c.Get(e.srv.URL + "/c/1/invoices/1/pdf")
	pdf, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(pdf), "%PDF") {
		t.Fatalf("not a PDF: %q", string(pdf[:min(40, len(pdf))]))
	}

	// issue → sequential number
	b.post("/c/1/invoices/1/issue", nil)
	year := today[:4]
	b.must("AC-" + year + "-0001")
	// issued invoices are frozen
	b.post("/c/1/invoices/1/edit", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"fr"}, "issue_date": {today}, "due_date": {due},
		"line_desc": {"x"}, "line_qty": {"1"}, "line_price": {"1"}, "line_tax": {"0"}})
	if b.status != 403 {
		t.Fatalf("editing an issued invoice should be forbidden, got %d", b.status)
	}

	// second invoice gets 0002
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"USD"}, "lang": {"en"}, "issue_date": {today}, "due_date": {due},
		"line_desc": {"Consulting"}, "line_qty": {"1"}, "line_price": {"1,234.50"}, "line_tax": {"0"}, "action": {"issue"}})
	b.must("AC-" + year + "-0002")

	// public page in the client's language, PDF available, no auth needed
	inv, _ = e.app.Store.Invoice(1, 1)
	anon := e.browser()
	anon.get("/i/" + inv.PublicToken)
	anon.must("Facture")
	anon.must("1 221,09 €")
	anon.must("Virement bancaire")
	anon.get("/i/notavalidtokennotavalidtoken")
	if anon.status != 404 {
		t.Fatalf("bad token should 404, got %d", anon.status)
	}
	resp, _ = anon.c.Get(e.srv.URL + "/i/" + inv.PublicToken + "/pdf")
	pdf, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(pdf), "%PDF") {
		t.Fatal("public PDF failed")
	}

	// partial then full payment
	b.post("/c/1/invoices/1/payments", url.Values{"amount": {"221,09"}, "method": {"bank_transfer"}, "paid_on": {today}})
	b.must("Payment recorded")
	inv, _ = e.app.Store.Invoice(1, 1)
	if inv.Status != "open" || inv.AmountPaid != 22109 {
		t.Fatalf("partial payment: %s %d", inv.Status, inv.AmountPaid)
	}
	b.post("/c/1/invoices/1/payments", url.Values{"amount": {"1000"}, "method": {"bank_transfer"}, "paid_on": {today}})
	inv, _ = e.app.Store.Invoice(1, 1)
	if inv.Status != "paid" {
		t.Fatalf("should be paid: %s", inv.Status)
	}
	// deleting a payment reopens it
	pays, _ := e.app.Store.Payments(1)
	b.post(fmt.Sprintf("/c/1/invoices/1/payments/%d/delete", pays[1].ID), nil)
	inv, _ = e.app.Store.Invoice(1, 1)
	if inv.Status != "open" || inv.AmountPaid != 22109 {
		t.Fatalf("payment deletion: %s %d", inv.Status, inv.AmountPaid)
	}

	// dashboard & lists render
	b.get("/c/1")
	b.must("Outstanding")
	b.get("/c/1/invoices?status=open")
	b.must("AC-" + year + "-0001")
	b.get("/c/1/clients")
	b.must("Client SARL")

	// recurring: monthly, starting today → generates immediately on run
	b.post("/c/1/recurring/new", url.Values{"client_id": {"1"}, "name": {"Hosting"}, "currency": {"EUR"}, "interval_unit": {"month"},
		"interval_count": {"1"}, "next_run": {today}, "due_days": {"15"}, "line_desc": {"Monthly hosting"}, "line_qty": {"1"},
		"line_price": {"49"}, "line_tax": {"20"}})
	b.must("Recurring invoice saved")
	e.app.RunRecurring(t.Context())
	e.app.RunRecurring(t.Context()) // idempotent
	r, _ := e.app.Store.Recurring(1, 1)
	invs, _ := e.app.Store.RecurringInvoices(1, 1)
	if len(invs) != 1 || invs[0].Number != "AC-"+year+"-0003" || invs[0].Lang != "fr" || invs[0].Total != 5880 {
		t.Fatalf("recurring generation: %+v", invs)
	}
	if r.NextRun <= today {
		t.Fatalf("next run not advanced: %s", r.NextRun)
	}

	// void
	b.post("/c/1/invoices/2/void", nil)
	inv2, _ := e.app.Store.Invoice(1, 2)
	if inv2.Status != "void" {
		t.Fatal("void failed")
	}

	// CSRF: missing token is rejected
	req, _ := http.NewRequest("POST", e.srv.URL+"/c/1/invoices/1/void", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, _ = b.c.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("missing CSRF should be 403, got %d", resp.StatusCode)
	}
	// cross-site origin is rejected even with a token
	form := url.Values{"csrf": {b.csrf}}
	req, _ = http.NewRequest("POST", e.srv.URL+"/c/1/invoices/1/void", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ = b.c.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("cross-origin POST should be 403, got %d", resp.StatusCode)
	}

	// unauthenticated access redirects to login
	anon.get("/c/1/invoices")
	anon.must("Sign in")

	// invite a member limited to a second company
	b.post("/companies/new", url.Values{"name": {"Other Co"}, "currency": {"CHF"}, "lang": {"en"}})
	b.post("/admin/invites", url.Values{"email": {"member@example.test"}, "role": {"member"}, "companies": {"2"}})
	m := regexp.MustCompile(`/invite/([A-Za-z0-9_-]+)`).FindStringSubmatch(b.last)
	if m == nil {
		t.Fatal("no invite link")
	}
	mb := e.browser()
	mb.get("/invite/" + m[1])
	mb.must("Join Invoicer")
	mb.post("/invite/"+m[1], url.Values{"name": {"Member"}, "password": {pw}, "password2": {pw}, "lang": {"fr"}})
	mb.get("/c/2")
	mb.must("Other Co")
	mb.get("/c/1")
	if mb.status != 403 {
		t.Fatalf("member must not access company 1, got %d", mb.status)
	}
	mb.get("/c/1/invoices/1/pdf")
	if mb.status != 403 {
		t.Fatalf("member must not read company 1 PDFs, got %d", mb.status)
	}
	mb.get("/admin/users")
	if mb.status != 403 {
		t.Fatalf("member must not access admin, got %d", mb.status)
	}
	mb.get("/c/2/settings")
	if mb.status != 403 {
		t.Fatalf("member must not access company settings, got %d", mb.status)
	}
	// invite links are single use
	again := e.browser()
	again.get("/invite/" + m[1])
	if again.status != 404 {
		t.Fatalf("reused invite should 404, got %d", again.status)
	}

	// login brute force: lockout after repeated failures
	lb := e.browser()
	lb.get("/login")
	for i := 0; i < 6; i++ {
		lb.post("/login", url.Values{"email": {"member@example.test"}, "password": {"wrong password!"}})
	}
	lb.post("/login", url.Values{"email": {"member@example.test"}, "password": {pw}})
	if lb.status != 429 {
		t.Fatalf("expected lockout, got %d", lb.status)
	}
}

func TestStripeWebhookSignature(t *testing.T) {
	e := newEnv(t)
	co, _ := e.app.Store.CreateCompany("pub123", "Co", "EUR", "en", 0)
	secret := "whsec_test"
	e.app.Store.UpdateCompanyStripe(co.ID, e.app.Box.Seal("sk_test_xxxxxxxxxxxxxxxxxxxx", "company:"+strconv.FormatInt(co.ID, 10)+":stripe"),
		e.app.Box.Seal(secret, "company:"+strconv.FormatInt(co.ID, 10)+":stripe_whsec"), "we_1", "Test")
	payload := `{"id":"evt_1","type":"customer.created","data":{"object":{"id":"cus_1"}}}`
	post := func(sig string) int {
		req, _ := http.NewRequest("POST", e.srv.URL+"/webhooks/stripe/pub123", strings.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + payload))
	good := "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	if c := post(good); c != 200 {
		t.Fatalf("valid signature rejected: %d", c)
	}
	if c := post("t=" + ts + ",v1=deadbeef"); c != 400 {
		t.Fatalf("bad signature accepted: %d", c)
	}
	old := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	mac = hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(old + "." + payload))
	if c := post("t=" + old + ",v1=" + hex.EncodeToString(mac.Sum(nil))); c != 400 {
		t.Fatalf("replayed signature accepted: %d", c)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	if strings.Contains(resp.Header.Get("Content-Security-Policy"), "unsafe-inline") {
		t.Error("CSP must not allow inline scripts or styles")
	}
}
