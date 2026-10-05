package web

import (
	"net/url"
	"testing"

	"github.com/flocom/invoicer/internal/app"
)

// Saving a schedule due today and clicking "generate now" right after must
// not produce two invoices, nor must a double click.
func TestGenerateNowNeverDuplicates(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "lang": {"en"}})
	today := e.app.Today()
	b.post("/c/1/recurring/new", url.Values{"name": {"Hosting"}, "client_id": {"1"}, "currency": {"EUR"}, "interval_count": {"1"},
		"interval_unit": {"month"}, "next_run": {today}, "due_days": {"30"},
		"line_desc": {"Hosting"}, "line_qty": {"1"}, "line_price": {"10"}, "line_tax": {"0"}})
	count := func() int {
		invs, _ := e.app.Store.RecurringInvoices(1, 1)
		return len(invs)
	}
	// generated before the page is shown
	if n := count(); n != 1 {
		t.Fatalf("after save: %d invoice(s)", n)
	}
	r, _ := e.app.Store.Recurring(1, 1)
	if r.NextRun <= today {
		t.Fatalf("next run not advanced: %s", r.NextRun)
	}
	b.must(`name="expect" value="` + app.RunToken(r) + `"`)

	// a page loaded before that generation asks for today's invoice again: refused
	b.post("/c/1/recurring/1/run", url.Values{"expect": {today + "/0"}})
	b.must("already been generated")
	if n := count(); n != 1 {
		t.Fatalf("stale page generated again: %d invoice(s)", n)
	}

	// "generate now" for the next period works once, a double click does nothing
	b.post("/c/1/recurring/1/run", url.Values{"expect": {app.RunToken(r)}})
	b.must("Invoice generated.")
	b.post("/c/1/recurring/1/run", url.Values{"expect": {app.RunToken(r)}})
	b.must("already been generated")
	if n := count(); n != 2 {
		t.Fatalf("after generate now: %d invoice(s)", n)
	}
}
