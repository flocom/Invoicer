package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/store"
)

func TestSentEmailsHistory(t *testing.T) {
	e := newEnv(t)
	api := newFakeAPIs(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	b.post("/companies/new", url.Values{"name": {"Beta"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.SetSetting("base_url", e.srv.URL)
	e.app.Store.UpdateCompanyEmail(1, e.app.Box.Seal("re_test_key", "company:1:resend"), false, "billing@alpha.test", "", "")
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "email": {"ap@globex.test"}, "lang": {"en"}})
	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()}, "due_date": {due},
		"line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"120"}, "line_tax": {"0"}, "action": {"send"}})
	if len(api.mailSubjects()) != 1 {
		t.Fatalf("invoice e-mail not sent: %v", api.mailSubjects())
	}
	// an e-mail logged before bodies were kept
	e.app.Store.LogEmail(store.EmailLog{CompanyID: 1, InvoiceID: 1, Kind: "reminder_due", To: "ap@globex.test", Subject: "Old reminder", Status: "sent"})

	// the invoice history links each e-mail
	b.get("/c/1/invoices/1")
	b.must(`href="/c/1/emails/1"`)
	b.must("All sent e-mails")

	b.get("/c/1/emails")
	b.must("Invoice INV-")
	b.must("Old reminder")
	b.must("ap@globex.test")

	b.get("/c/1/emails/1")
	b.must(`src="/c/1/emails/1/html"`)
	b.must("Text version")
	b.must("INV-") // attachment name
	b.get("/c/1/emails/2")
	b.must("was not kept")

	// the body is served for the preview frame only, without scripts
	resp, err := b.c.Get(e.srv.URL + "/c/1/emails/1/html")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "frame-ancestors 'self'") ||
		strings.Contains(csp, "script-src") || resp.Header.Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("preview headers: %d %q %q", resp.StatusCode, csp, resp.Header.Get("X-Frame-Options"))
	}
	b.get("/c/1/emails/1/html")
	b.must("Please find attached your invoice")
	if strings.Contains(b.last, "/pay/") {
		t.Fatal("no card payment link without Stripe")
	}

	// e-mails stay within their company
	b.get("/c/2/emails/1")
	if b.status != http.StatusNotFound {
		t.Fatalf("e-mail of another company: %d", b.status)
	}
	b.get("/c/2/emails")
	if strings.Contains(b.last, "Old reminder") {
		t.Fatal("e-mail list leaks another company's e-mails")
	}
	anon := e.browser()
	anon.get("/c/1/emails/" + strconv.Itoa(1))
	if strings.Contains(anon.last, "Text version") {
		t.Fatal("e-mail readable without logging in")
	}
}
