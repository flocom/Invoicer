package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/flocom/invoicer/internal/store"
)

// A schedule whose invoice could not be e-mailed because the client had no
// address must stop showing that error once the client gets one.
func TestRecurringNoRecipientErrorClearedWhenClientGetsEmail(t *testing.T) {
	e := newEnv(t)
	newFakeAPIs(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.SetSetting("base_url", e.srv.URL)
	e.app.Store.UpdateCompanyEmail(1, e.app.Box.Seal("re_test_key", "company:1:resend"), false, "billing@alpha.test", "", "")
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "lang": {"en"}})
	b.post("/c/1/recurring/new", url.Values{"name": {"Hosting"}, "client_id": {"1"}, "currency": {"EUR"}, "interval_count": {"1"},
		"interval_unit": {"month"}, "next_run": {tomorrow()}, "due_days": {"30"}, "auto_send": {"1"},
		"line_desc": {"Hosting"}, "line_qty": {"1"}, "line_price": {"10"}, "line_tax": {"0"}})

	b.post("/c/1/recurring/1/run", url.Values{"mode": {"email"}})
	r, _ := e.app.Store.Recurring(1, 1)
	if !strings.Contains(r.LastError, "no e-mail address") {
		t.Fatalf("expected the no-recipient error, got %q", r.LastError)
	}

	b.post("/c/1/clients/1/edit", url.Values{"name": {"Globex"}, "email": {"ap@globex.test"}, "lang": {"en"}})
	if r, _ := e.app.Store.Recurring(1, 1); r.LastError != "" {
		t.Fatalf("error still shown after the client got an address: %q", r.LastError)
	}
	b.get("/c/1/recurring/1")
	if strings.Contains(b.last, "no e-mail address") {
		t.Fatal("stale error still displayed on the schedule")
	}

	// a later e-mail failure, then a successful send of a schedule invoice, clears it too
	e.app.Store.SetRecurringError(1, "e-mail: boom")
	list, _, _ := e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
	b.post("/c/1/invoices/"+itoa(list[0].ID)+"/send", url.Values{})
	if r, _ := e.app.Store.Recurring(1, 1); r.LastError != "" {
		t.Fatalf("error kept after the invoice was e-mailed: %q", r.LastError)
	}
}
