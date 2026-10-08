package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/store"
)

func TestMakeInvoiceRecurringAndLanguage(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "lang": {"en"}})

	issue := time.Now().AddDate(0, 0, -3)
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"fr"},
		"issue_date": {issue.Format("2006-01-02")}, "due_date": {issue.AddDate(0, 0, 15).Format("2006-01-02")},
		"line_desc": {"Maintenance du site\nmois en cours"}, "line_qty": {"2"}, "line_price": {"45"}, "line_tax": {"20"},
		"notes": {"Merci !"}, "action": {"issue"}})
	inv, _ := e.app.Store.Invoice(1, 1)
	b.get("/c/1/invoices/1")
	b.must("Make it recurring")

	// the form is pre-filled from the invoice
	b.get("/c/1/recurring/new?from=1")
	b.must("Pre-filled from " + inv.Number)
	b.must(`name="from" value="1"`)
	b.must(`value="Maintenance du site"`)
	b.must(`<option value="fr" selected>`)
	b.must(`value="15"`)
	next := issue.AddDate(0, 1, 0).Format("2006-01-02")
	b.must(`value="` + next + `"`)

	b.post("/c/1/recurring/new", url.Values{"from": {"1"}, "name": {"Maintenance du site"}, "client_id": {"1"}, "currency": {"EUR"},
		"lang": {"fr"}, "interval_count": {"1"}, "interval_unit": {"month"}, "next_run": {next}, "due_days": {"15"},
		"line_desc": {"Maintenance du site\nmois en cours"}, "line_qty": {"2"}, "line_price": {"45"}, "line_tax": {"20"}})
	r, err := e.app.Store.Recurring(1, 1)
	if err != nil || r.Lang != "fr" || r.DueDays != 15 || r.NextRun != next {
		t.Fatalf("schedule: %+v %v", r, err)
	}
	if inv, _ = e.app.Store.Invoice(1, 1); inv.RecurringID != 1 {
		t.Fatalf("the invoice should become the first of the schedule: %d", inv.RecurringID)
	}
	b.get("/c/1/invoices/1")
	if strings.Contains(b.last, "Make it recurring") {
		t.Fatal("an invoice of a schedule is offered to be made recurring again")
	}

	// generated invoices use the schedule's language, or the client's by default
	b.post("/c/1/recurring/1/run", url.Values{"mode": {"none"}})
	list, _, _ := e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
	if gen, _ := e.app.Store.Invoice(1, list[0].ID); gen.ID == 1 || gen.Lang != "fr" || gen.RecurringID != 1 {
		t.Fatalf("generated: id=%d lang=%s", gen.ID, gen.Lang)
	}
	r.Lang = ""
	e.app.Store.SaveRecurring(r)
	b.post("/c/1/recurring/1/run", url.Values{"mode": {"none"}})
	list, _, _ = e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
	if gen, _ := e.app.Store.Invoice(1, list[0].ID); gen.Lang != "en" {
		t.Fatalf("client's language: %s", gen.Lang)
	}

	// deleting the schedule keeps its invoices, detached
	b.post("/c/1/recurring/1/delete", nil)
	if inv, _ = e.app.Store.Invoice(1, 1); inv.RecurringID != 0 {
		t.Fatalf("invoice still attached to a deleted schedule: %d", inv.RecurringID)
	}
}
