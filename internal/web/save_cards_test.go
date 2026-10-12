package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSaveCardsOnEveryPayment(t *testing.T) {
	e := newEnv(t)
	api := newFakeAPIs(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.SetSetting("base_url", e.srv.URL)
	e.app.Store.UpdateCompanyStripe(1, e.app.Box.Seal("sk_test_xxxxxxxxxxxxxxxxxxxx", "company:1:stripe"),
		e.app.Box.Seal("whsec", "company:1:stripe_whsec"), "we_1", "Test")
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "email": {"ap@globex.test"}, "lang": {"en"}})
	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()},
		"due_date": {due}, "line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"120"}, "line_tax": {"0"}, "action": {"issue"}, "card_payment": {"1"}})
	inv, _ := e.app.Store.Invoice(1, 1)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	pay := func() url.Values {
		resp, err := c.Get(e.srv.URL + "/pay/" + inv.PublicToken)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		api.mu.Lock()
		defer api.mu.Unlock()
		return api.checkouts[len(api.checkouts)-1]
	}

	// by default a one-off invoice does not keep the card
	b.get("/c/1/settings?tab=payments")
	b.must("Save the client&#39;s card on every online payment")
	if co := pay(); co.Get("customer") != "" || co.Get("payment_intent_data[setup_future_usage]") != "" {
		t.Fatalf("card saved without the setting: %v", co)
	}

	// once turned on, the payment page saves the card (a new session is made)
	b.post("/c/1/settings/stripe/cards", url.Values{"save_cards": {"1"}})
	if co, _ := e.app.Store.Company(1); !co.SaveCards {
		t.Fatal("setting not saved")
	}
	n := len(api.checkouts)
	if co := pay(); len(api.checkouts) != n+1 || co.Get("customer") != "cus_1" || co.Get("payment_intent_data[setup_future_usage]") != "off_session" {
		t.Fatalf("card not saved with the setting: %v", co)
	}
	// the same session is reused while the setting does not change
	pay()
	if len(api.checkouts) != n+1 {
		t.Fatal("the open session should be reused")
	}

	// turned off again: a session that would keep the card is not reused
	b.post("/c/1/settings/stripe/cards", nil)
	if co := pay(); len(api.checkouts) != n+2 || co.Get("customer") != "" {
		t.Fatalf("card kept after turning the setting off: %v", co)
	}

	// the name of a recurring invoice is not in bold in the list
	b.post("/c/1/recurring/new", url.Values{"name": {"Hosting"}, "client_id": {"1"}, "currency": {"EUR"}, "interval_count": {"1"},
		"interval_unit": {"month"}, "next_run": {tomorrow()}, "due_days": {"30"}, "line_desc": {"Hosting"}, "line_qty": {"1"},
		"line_price": {"10"}, "line_tax": {"0"}})
	b.get("/c/1/recurring")
	if !strings.Contains(b.last, `/recurring/1">Hosting</a>`) {
		t.Fatal("recurring name should be a plain link")
	}
}
