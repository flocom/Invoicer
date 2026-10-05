package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/flocom/invoicer/internal/store"
)

func TestGenerateNowChargeOrEmail(t *testing.T) {
	e := newEnv(t)
	api := newFakeAPIs(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.SetSetting("base_url", e.srv.URL)
	e.app.Store.UpdateCompanyStripe(1, e.app.Box.Seal("sk_test_xxxxxxxxxxxxxxxxxxxx", "company:1:stripe"),
		e.app.Box.Seal("whsec", "company:1:stripe_whsec"), "we_1", "Test")
	e.app.Store.UpdateCompanyEmail(1, e.app.Box.Seal("re_test_key", "company:1:resend"), false, "billing@alpha.test", "", "")
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "email": {"ap@globex.test"}, "lang": {"en"}})
	b.post("/c/1/recurring/new", url.Values{"name": {"Hosting"}, "client_id": {"1"}, "currency": {"EUR"}, "interval_count": {"1"},
		"interval_unit": {"month"}, "next_run": {e.app.Today()}, "due_days": {"30"}, "card_payment": {"1"}, "auto_charge": {"1"},
		"line_desc": {"Hosting"}, "line_qty": {"1"}, "line_price": {"10"}, "line_tax": {"0"}})

	// no saved card yet: the usual single button
	b.get("/c/1/recurring/1")
	if strings.Contains(b.last, `value="charge"`) {
		t.Fatal("charge choice offered without a saved card")
	}
	e.app.Store.SaveStripeCustomer(store.StripeCustomer{CompanyID: 1, ClientID: 1, CustomerID: "cus_1", CardToken: "tokentokentokentokentoken1"})
	e.app.Store.SaveCard(&store.SavedCard{CompanyID: 1, ClientID: 1, PaymentMethod: "pm_1", Brand: "visa", Last4: "4242", ExpMonth: 4, ExpYear: 2030})
	b.get("/c/1/recurring/1")
	b.must(`value="charge"`)
	b.must(`value="email"`)
	b.must("Visa •••• 4242")

	latest := func() *store.Invoice {
		list, _, _ := e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
		inv, _ := e.app.Store.Invoice(1, list[0].ID)
		return inv
	}

	// e-mail without charging (although the schedule does not e-mail on its own)
	b.post("/c/1/recurring/1/run", url.Values{"mode": {"email"}})
	inv := latest()
	if inv.Status != store.StatusOpen || len(api.charges) != 0 {
		t.Fatalf("e-mail mode: status %s, %d charge(s)", inv.Status, len(api.charges))
	}
	if s := api.mailSubjects(); len(s) != 1 || !strings.Contains(s[0], "Invoice "+inv.Number) {
		t.Fatalf("e-mail mode should send the invoice: %v", s)
	}

	// charge now
	b.post("/c/1/recurring/1/run", url.Values{"mode": {"charge"}})
	inv2 := latest()
	if inv2.ID == inv.ID || inv2.Status != store.StatusPaid || len(api.charges) != 1 {
		t.Fatalf("charge mode: id %d status %s, %d charge(s)", inv2.ID, inv2.Status, len(api.charges))
	}
	b.must("Invoice generated.")

	// a declined card is reported right away
	e.app.Store.SaveCard(&store.SavedCard{CompanyID: 1, ClientID: 1, PaymentMethod: "pm_declined", Brand: "visa", Last4: "0002", ExpMonth: 4, ExpYear: 2030})
	b.post("/c/1/recurring/1/run", url.Values{"mode": {"charge"}})
	b.must("Invoice generated, but:")
	b.must("declined")
}
