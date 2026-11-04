package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/store"
)

var dataCSRF = regexp.MustCompile(`data-csrf="([^"]+)"`)

// postJS sends what pay.js sends: a form body with the CSRF header.
func (b *browser) postJS(path, body string) (int, map[string]string) {
	b.e.t.Helper()
	m := dataCSRF.FindStringSubmatch(b.last)
	if m == nil {
		b.e.t.Fatal("no data-csrf on the page")
	}
	req, _ := http.NewRequest("POST", b.e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", m[1])
	req.Header.Set("Origin", b.e.srv.URL)
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]string
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestStripeCardFormOnThePaymentPage(t *testing.T) {
	e := newEnv(t)
	api := newFakeAPIs(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.SetSetting("base_url", e.srv.URL)
	e.app.Store.UpdateCompanyStripe(1, e.app.Box.Seal("sk_test_xxxxxxxxxxxxxxxxxxxx", "company:1:stripe"),
		e.app.Box.Seal("whsec", "company:1:stripe_whsec"), "we_1", "Test")
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "lang": {"en"}})
	newInvoice := func(price string) *store.Invoice {
		b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()},
			"due_date": {time.Now().AddDate(0, 0, 30).Format("2006-01-02")}, "line_desc": {"Work"}, "line_qty": {"1"},
			"line_price": {price}, "line_tax": {"0"}, "action": {"issue"}, "card_payment": {"1"}})
		list, _, _ := e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
		inv, _ := e.app.Store.Invoice(1, list[0].ID)
		return inv
	}
	inv := newInvoice("120")
	anon := e.browser()
	anon.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// without a publishable key: the button to Stripe Checkout, as before
	anon.get("/i/" + inv.PublicToken)
	anon.must(`href="/pay/` + inv.PublicToken + `"`)
	if strings.Contains(anon.last, "js.stripe.com") || strings.Contains(anon.last, "data-paybox") {
		t.Fatal("card form shown without a publishable key")
	}

	// the key must be a publishable key of the same mode as the secret key
	b.post("/c/1/settings/stripe/form", url.Values{"stripe_publishable": {"sk_test_notpublishablexxxxxxxx"}})
	b.must("does not look like a Stripe publishable key")
	b.post("/c/1/settings/stripe/form", url.Values{"stripe_publishable": {"pk_live_aaaaaaaaaaaaaaaaaaaaaaaa"}})
	b.must("must both be live keys or both test keys")
	b.post("/c/1/settings/stripe/form", url.Values{"stripe_publishable": {"pk_test_aaaaaaaaaaaaaaaaaaaaaaaa"}})
	b.get("/c/1/settings?tab=payments")
	b.must("The card form is shown on the payment page")

	// with it: the card form, loaded from Stripe, the button kept as a fallback
	resp, err := anon.c.Get(e.srv.URL + "/i/" + inv.PublicToken)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "https://js.stripe.com") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("payment page CSP: %s", csp)
	}
	anon.get("/i/" + inv.PublicToken)
	anon.must(`<script src="https://js.stripe.com/v3/" defer></script>`)
	anon.must(`data-stripe-key="pk_test_aaaaaaaaaaaaaaaaaaaaaaaa"`)
	anon.must(`data-amount="12000"`)
	anon.must("data-card-fallback")
	anon.must("Pay €120.00")
	// other pages keep the strict policy
	resp, _ = http.Get(e.srv.URL + "/login")
	resp.Body.Close()
	if strings.Contains(resp.Header.Get("Content-Security-Policy"), "stripe") {
		t.Fatal("Stripe allowed outside the payment page")
	}

	// the payment is created when the client submits; retried, it is reused
	code, out := anon.postJS("/pay/"+inv.PublicToken+"/intent", "")
	if code != 200 || out["clientSecret"] != "pi_form_1_secret_x" {
		t.Fatalf("intent: %d %v", code, out)
	}
	if f := api.intents["pi_form_1"]; f.Get("amount") != "12000" || f.Get("payment_method_types[0]") != "card" || f.Get("customer") != "" {
		t.Fatalf("intent request: %v", f)
	}
	if _, out = anon.postJS("/pay/"+inv.PublicToken+"/intent", ""); out["clientSecret"] != "pi_form_1_secret_x" || len(api.intents) != 1 {
		t.Fatalf("a declined card is retried on the same payment: %v", out)
	}
	// without the page's token the request is refused
	req, _ := http.NewRequest("POST", e.srv.URL+"/pay/"+inv.PublicToken+"/intent", nil)
	req.Header.Set("Origin", e.srv.URL)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 403 {
		t.Fatalf("intent without CSRF: %d", r.StatusCode)
	}

	// a declined card comes back with a message, nothing recorded
	loc := func(path string) string {
		r, err := anon.c.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.Header.Get("Location")
	}
	if l := loc("/pay/" + inv.PublicToken + "/done?payment_intent=pi_form_1&redirect_status=failed"); !strings.HasSuffix(l, "?card=failed") {
		t.Fatalf("declined: %s", l)
	}
	anon.get("/i/" + inv.PublicToken + "?card=failed")
	anon.must("The card payment did not go through")

	// confirmed in the browser (after 3-D Secure): recorded once
	api.intentSt["pi_form_1"] = "succeeded"
	if l := loc("/pay/" + inv.PublicToken + "/done?payment_intent=pi_form_1&redirect_status=succeeded"); !strings.HasSuffix(l, "?paid=1") {
		t.Fatalf("done: %s", l)
	}
	loc("/pay/" + inv.PublicToken + "/done?payment_intent=pi_form_1&redirect_status=succeeded")
	pays, _ := e.app.Store.Payments(inv.ID)
	if got, _ := e.app.Store.Invoice(1, inv.ID); got.Status != store.StatusPaid || len(pays) != 1 || pays[0].Reference != "pi_form_1" {
		t.Fatalf("paid with the card form: %s %+v", got.Status, pays)
	}
	// another invoice's payment cannot be applied here
	if l := loc("/pay/" + newInvoice("5").PublicToken + "/done?payment_intent=pi_form_1"); strings.Contains(l, "paid=1") {
		t.Fatalf("payment of another invoice accepted: %s", l)
	}

	// a client who closed the page after paying is caught by the follow-up
	inv = newInvoice("30")
	anon.get("/i/" + inv.PublicToken)
	_, out = anon.postJS("/pay/"+inv.PublicToken+"/intent", "")
	id := strings.TrimSuffix(out["clientSecret"], "_secret_x")
	api.intentSt[id] = "succeeded"
	e.app.ReconcileStripe(t.Context())
	if got, _ := e.app.Store.Invoice(1, inv.ID); got.Status != store.StatusPaid {
		t.Fatalf("follow-up: %s", got.Status)
	}

	// an unconfirmed payment is cancelled when the invoice is paid otherwise
	inv = newInvoice("40")
	anon.get("/i/" + inv.PublicToken)
	_, out = anon.postJS("/pay/"+inv.PublicToken+"/intent", "")
	id = strings.TrimSuffix(out["clientSecret"], "_secret_x")
	b.get("/c/1/invoices/" + itoa(inv.ID))
	b.post("/c/1/invoices/"+itoa(inv.ID)+"/payments", url.Values{"amount": {"40"}, "method": {"bank_transfer"}, "paid_on": {e.app.Today()}})
	if got, _ := e.app.Store.Invoice(1, inv.ID); got.Status != store.StatusPaid {
		t.Fatalf("manual payment: %s", got.Status)
	}
	found := false
	for _, c := range api.canceled {
		found = found || c == id
	}
	if !found {
		t.Fatalf("open card payment not cancelled: %v", api.canceled)
	}
}
