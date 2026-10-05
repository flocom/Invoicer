package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/store"
)

// SQLite reuses the id of the last deleted invoice: the next invoice must not
// show the e-mails of the deleted one.
func TestDeletedInvoiceEmailsNotInherited(t *testing.T) {
	e := newEnv(t)
	api := newFakeAPIs(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.UpdateCompanyEmail(1, e.app.Box.Seal("re_test_key", "company:1:resend"), false, "billing@alpha.test", "", "")
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "email": {"ap@globex.test"}, "lang": {"en"}})
	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	newInvoice := func(action string) *store.Invoice {
		b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()},
			"due_date": {due}, "line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"10"}, "line_tax": {"0"}, "action": {action}})
		list, _, _ := e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
		return list[0]
	}
	first := newInvoice("send")
	if len(api.mailSubjects()) != 1 {
		t.Fatal("invoice e-mail not sent")
	}
	b.post("/c/1/invoices/"+itoa(first.ID)+"/destroy", url.Values{"confirm": {first.Number}})
	if _, err := e.app.Store.Invoice(1, first.ID); err == nil {
		t.Fatal("invoice not deleted")
	}
	second := newInvoice("save")
	if second.ID != first.ID {
		t.Skipf("id not reused (%d → %d): nothing to check", first.ID, second.ID)
	}
	if emails, _ := e.app.Store.EmailsForInvoice(second.ID); len(emails) != 0 {
		t.Fatalf("new invoice inherited %d e-mail(s)", len(emails))
	}
	b.get("/c/1/invoices/" + itoa(second.ID))
	if strings.Contains(b.last, "/c/1/emails/") {
		t.Fatal("history links to the deleted invoice's e-mail")
	}
	// the e-mail stays in the list of sent e-mails
	b.get("/c/1/emails")
	b.must("Invoice " + first.Number)
}
