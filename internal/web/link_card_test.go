package web

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/store"
)

// A client who paid with Stripe Link has a "link" payment method: no card
// details, and it must be charged as a link payment method.
func TestStripeLinkSavedCard(t *testing.T) {
	e := newEnv(t)
	api := newFakeAPIs(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.SetSetting("base_url", e.srv.URL)
	e.app.Store.UpdateCompanyStripe(1, e.app.Box.Seal("sk_test_xxxxxxxxxxxxxxxxxxxx", "company:1:stripe"),
		e.app.Box.Seal("whsec", "company:1:stripe_whsec"), "we_1", "Test")
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "email": {"ap@globex.test"}, "lang": {"en"}})
	e.app.Store.SaveStripeCustomer(store.StripeCustomer{CompanyID: 1, ClientID: 1, CustomerID: "cus_1", CardToken: "tok_linklinklinklinklinklink"})
	// saved by an earlier version: no details
	e.app.Store.SaveCard(&store.SavedCard{CompanyID: 1, ClientID: 1, PaymentMethod: "pm_link1"})
	old, _ := e.app.Store.DefaultCard(1, 1)
	if old.Label() != "Card" || old.Expired(e.app.Today()) {
		t.Fatalf("a card without details: %q expired=%v", old.Label(), old.Expired(e.app.Today()))
	}

	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()},
		"due_date": {due}, "line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"120"}, "line_tax": {"0"}, "action": {"issue"}, "card_payment": {"1"}})
	b.post("/c/1/invoices/1/charge", url.Values{"card": {strconv.FormatInt(old.ID, 10)}})
	if inv, _ := e.app.Store.Invoice(1, 1); inv.Status != store.StatusPaid {
		t.Fatalf("Link charge: %s", inv.Status)
	}
	api.mu.Lock()
	ch := api.charges[len(api.charges)-1]
	api.mu.Unlock()
	if ch.Get("payment_method") != "pm_link1" || ch.Get("payment_method_types[0]") != "link" || ch.Get("off_session") != "true" {
		t.Fatalf("Link charge request: %v", ch)
	}
	c, _ := e.app.Store.DefaultCard(1, 1)
	if !c.IsLink() || c.Label() != "Link (jane@globex.test)" {
		t.Fatalf("details read again from Stripe: %+v", c)
	}
	b.get("/c/1/clients/1")
	b.must("Link (jane@globex.test)")
	if strings.Contains(b.last, "(00/0)") {
		t.Fatal("no fake expiry for a Link wallet")
	}

	// the client's card page shows the Link account, not card numbers
	anon := e.browser()
	anon.get("/card/tok_linklinklinklinklinklink")
	anon.must("jane@globex.test")
	anon.must("LINK")
	anon.must("Active")

	// a card is still charged as a card
	e.app.Store.SaveCard(&store.SavedCard{CompanyID: 1, ClientID: 1, PaymentMethod: "pm_9", Brand: "visa", Last4: "4242", ExpMonth: 4, ExpYear: 2030})
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()},
		"due_date": {due}, "line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"50"}, "line_tax": {"0"}, "action": {"issue"}, "card_payment": {"1"}})
	visa, _ := e.app.Store.DefaultCard(1, 1)
	b.post("/c/1/invoices/2/charge", url.Values{"card": {strconv.FormatInt(visa.ID, 10)}})
	api.mu.Lock()
	ch = api.charges[len(api.charges)-1]
	api.mu.Unlock()
	if ch.Get("payment_method") != "pm_9" || ch.Get("payment_method_types[0]") != "card" {
		t.Fatalf("card charge request: %v", ch)
	}
}
