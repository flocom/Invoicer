package web

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/store"
)

func TestHideLineSuggestion(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "lang": {"en"}})
	invoice := func(desc string) {
		b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()},
			"due_date": {e.app.Today()}, "line_desc": {desc}, "line_qty": {"1"}, "line_price": {"50"}, "line_tax": {"0"}, "action": {"save"}})
	}
	suggest := func() []string {
		b.get("/c/1/api/lines?q=maint")
		var r struct {
			Suggestions []struct{ Description string }
		}
		json.Unmarshal([]byte(b.last), &r)
		var out []string
		for _, s := range r.Suggestions {
			out = append(out, s.Description)
		}
		return out
	}
	invoice("Maintenance")
	invoice("Maintenance plus")
	if s := suggest(); len(s) != 2 {
		t.Fatalf("suggestions: %v", s)
	}
	b.get("/c/1/invoices/new")
	b.must(`data-suggest-remove="Remove from suggestions"`)
	b.post("/c/1/api/lines/hide", url.Values{"description": {" maintenance "}})
	if b.status != 200 {
		t.Fatalf("hide: %d %s", b.status, b.last)
	}
	if s := suggest(); len(s) != 1 || s[0] != "Maintenance plus" {
		t.Fatalf("after hiding: %v", s)
	}
	// using it again brings it back
	time.Sleep(1100 * time.Millisecond) // timestamps are in seconds
	invoice("Maintenance")
	if s := suggest(); len(s) != 2 {
		t.Fatalf("after reuse: %v", s)
	}
	b.post("/c/1/api/lines/hide", url.Values{"description": {"x"}, "csrf": {"bad"}})
	if b.status != 403 {
		t.Fatalf("csrf: %d", b.status)
	}
}

func TestAskBeforeEmailing(t *testing.T) {
	e := newEnv(t)
	api := newFakeAPIs(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "email": {"ap@globex.test"}, "lang": {"en"}})

	// without e-mail set up: a single "issue" button, no question
	b.get("/c/1/invoices/new")
	if strings.Contains(b.last, "E-mail the invoice to the client?") {
		t.Fatal("asked about e-mail although it is not set up")
	}
	e.app.Store.UpdateCompanyEmail(1, e.app.Box.Seal("re_test_key", "company:1:resend"), false, "billing@alpha.test", "", "")
	b.get("/c/1/invoices/new")
	b.must("E-mail the invoice to the client?")
	b.must(`value="send"`)
	b.must(`value="issue"`)

	// recurring "generate now": e-mail or not, whatever the schedule's setting
	b.post("/c/1/recurring/new", url.Values{"name": {"Hosting"}, "client_id": {"1"}, "currency": {"EUR"}, "interval_count": {"1"},
		"interval_unit": {"month"}, "next_run": {tomorrow()}, "due_days": {"30"}, "auto_send": {"1"},
		"line_desc": {"Hosting"}, "line_qty": {"1"}, "line_price": {"10"}, "line_tax": {"0"}})
	b.get("/c/1/recurring/1")
	b.must("Generate and e-mail it to the client")
	b.must("Generate without e-mailing the client")
	r, _ := e.app.Store.Recurring(1, 1)
	b.post("/c/1/recurring/1/run", url.Values{"mode": {"none"}, "expect": {runTokenOf(r)}})
	if n := len(api.mailSubjects()); n != 0 {
		t.Fatalf("\"without e-mail\" sent %d e-mail(s)", n)
	}
	r, _ = e.app.Store.Recurring(1, 1)
	b.post("/c/1/recurring/1/run", url.Values{"mode": {"email"}, "expect": {runTokenOf(r)}})
	if s := api.mailSubjects(); len(s) != 1 || !strings.Contains(s[0], "Invoice") {
		t.Fatalf("\"e-mail\" mode: %v", s)
	}
	invs, _ := e.app.Store.RecurringInvoices(1, 1)
	if len(invs) != 2 || invs[0].Status != store.StatusOpen {
		t.Fatalf("generated: %d", len(invs))
	}
}

func TestAskWhenSavingScheduleDueToday(t *testing.T) {
	e := newEnv(t)
	api := newFakeAPIs(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.UpdateCompanyEmail(1, e.app.Box.Seal("re_test_key", "company:1:resend"), false, "billing@alpha.test", "", "")
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "email": {"ap@globex.test"}, "lang": {"en"}})
	b.get("/c/1/recurring/new")
	b.must("data-first-run-dialog")
	b.must(`name="first_mode"`)
	save := func(mode string) {
		b.post("/c/1/recurring/new", url.Values{"name": {"Hosting " + mode}, "client_id": {"1"}, "currency": {"EUR"}, "interval_count": {"1"},
			"interval_unit": {"month"}, "next_run": {e.app.Today()}, "due_days": {"30"}, "auto_send": {"1"}, "first_mode": {mode},
			"line_desc": {"Hosting"}, "line_qty": {"1"}, "line_price": {"10"}, "line_tax": {"0"}})
	}
	// "do not send" wins over the schedule's automatic sending
	save("none")
	if n := len(api.mailSubjects()); n != 0 {
		t.Fatalf("first_mode=none sent %d e-mail(s)", n)
	}
	save("email")
	if n := len(api.mailSubjects()); n != 1 {
		t.Fatalf("first_mode=email sent %d e-mail(s)", n)
	}
	// without an answer (no JavaScript) the schedule's settings apply
	save("")
	if n := len(api.mailSubjects()); n != 2 {
		t.Fatalf("no answer: %d e-mail(s)", n)
	}
	for id := int64(1); id <= 3; id++ {
		if invs, _ := e.app.Store.RecurringInvoices(1, id); len(invs) != 1 {
			t.Fatalf("schedule %d generated %d invoice(s)", id, len(invs))
		}
	}
}
