package web

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

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
		"interval_unit": {"month"}, "next_run": {tomorrow()}, "due_days": {"30"}, "card_payment": {"1"}, "auto_charge": {"1"},
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
	if html := api.mails[0]["html"].(string); !strings.Contains(html, "/pay/"+inv.PublicToken) || !strings.Contains(html, "Pay now by card") {
		t.Fatal("the invoice e-mail should link to the Stripe payment")
	}

	// the open invoice offers to charge the saved card by hand
	invPath := "/c/1/invoices/" + strconv.FormatInt(inv.ID, 10)
	b.get(invPath)
	b.must("Charge €10.00 to the card")
	b.post(invPath+"/charge", url.Values{"card": {"1"}})
	b.must("paid by card")
	if paid, _ := e.app.Store.Invoice(1, inv.ID); paid.Status != store.StatusPaid || len(api.charges) != 1 {
		t.Fatalf("manual charge from the invoice: %s, %d charge(s)", paid.Status, len(api.charges))
	}
	b.get(invPath)
	if strings.Contains(b.last, "to the card") {
		t.Fatal("charge button shown on a paid invoice")
	}

	// charge now
	b.post("/c/1/recurring/1/run", url.Values{"mode": {"charge"}})
	inv2 := latest()
	if inv2.ID == inv.ID || inv2.Status != store.StatusPaid || len(api.charges) != 2 {
		t.Fatalf("charge mode: id %d status %s, %d charge(s)", inv2.ID, inv2.Status, len(api.charges))
	}
	b.must("Invoice generated.")

	// a declined card is reported right away
	e.app.Store.SaveCard(&store.SavedCard{CompanyID: 1, ClientID: 1, PaymentMethod: "pm_declined", Brand: "visa", Last4: "0002", ExpMonth: 4, ExpYear: 2030})
	b.post("/c/1/recurring/1/run", url.Values{"mode": {"charge"}})
	b.must("Invoice generated, but:")
	b.must("declined")
}

// tomorrow is a first run date that saving a schedule does not generate.
func tomorrow() string { return time.Now().AddDate(0, 0, 1).Format("2006-01-02") }
