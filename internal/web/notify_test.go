package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/store"
)

// mailsTo returns the subject and HTML of the e-mails sent to addr.
func (f *fakeAPIs) mailsTo(addr string) (subjects, bodies []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.mails {
		if strings.Contains(fmt.Sprint(m["to"]), addr) {
			subjects = append(subjects, fmt.Sprint(m["subject"]))
			bodies = append(bodies, fmt.Sprint(m["html"]))
		}
	}
	return
}

func TestOwnerPaymentNotificationsAndStripeLog(t *testing.T) {
	e := newEnv(t)
	api := newFakeAPIs(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	co, _ := e.app.Store.Company(1)
	e.app.Store.SetSetting("base_url", e.srv.URL)
	e.app.Store.UpdateCompanyStripe(1, e.app.Box.Seal("sk_test_xxxxxxxxxxxxxxxxxxxx", "company:1:stripe"),
		e.app.Box.Seal("whsec_test", "company:1:stripe_whsec"), "we_1", "Test")
	e.app.Store.UpdateCompanyEmail(1, e.app.Box.Seal("re_test_key", "company:1:resend"), false, "billing@alpha.test", "", "")
	e.app.Store.DB.Exec(`UPDATE companies SET email = 'owner@alpha.test' WHERE id = 1`)
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "email": {"ap@globex.test"}, "lang": {"en"}})

	b.get("/c/1/settings?tab=email")
	b.must("are sent to owner@alpha.test")

	// the client pays online: the owner is told once, however often Stripe calls
	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()}, "due_date": {due},
		"line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"120"}, "line_tax": {"0"}, "action": {"issue"}, "card_payment": {"1"}})
	inv, _ := e.app.Store.Invoice(1, 1)
	anon := e.browser()
	anon.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := anon.c.Get(e.srv.URL + "/pay/" + inv.PublicToken)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	webhook := func(evt, typ, obj string) int {
		payload := fmt.Sprintf(`{"id":%q,"type":%q,"data":{"object":{"id":%q}}}`, evt, typ, obj)
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte("whsec_test"))
		mac.Write([]byte(ts + "." + payload))
		req, _ := http.NewRequest("POST", e.srv.URL+"/webhooks/stripe/"+co.PublicID, strings.NewReader(payload))
		req.Header.Set("Stripe-Signature", "t="+ts+",v1="+hex.EncodeToString(mac.Sum(nil)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 2; i++ {
		if c := webhook("evt_paid_"+strconv.Itoa(i), "checkout.session.completed", "cs_payment_1"); c != 200 {
			t.Fatalf("webhook: %d", c)
		}
	}
	if inv, _ = e.app.Store.Invoice(1, 1); inv.Status != store.StatusPaid {
		t.Fatalf("online payment: %s", inv.Status)
	}
	subjects, bodies := api.mailsTo("owner@alpha.test")
	if len(subjects) != 1 || subjects[0] != "Payment received — invoice "+inv.Number+" (Globex)" ||
		!strings.Contains(bodies[0], "Globex paid invoice "+inv.Number+" online by card") || !strings.Contains(bodies[0], "€120.00") ||
		!strings.Contains(bodies[0], e.srv.URL+"/c/1/invoices/1") {
		t.Fatalf("owner notification of an online payment: %v", subjects)
	}

	// the Stripe log shows on the invoice and on its own page
	b.get("/c/1/invoices/1")
	b.must("Payment link created")
	b.must("Card payment received")
	b.must("Full Stripe log")
	b.get("/c/1/stripe-log")
	b.must("Webhook received")
	b.must("checkout.session.completed")
	b.must("evt_paid_1")
	b.must(inv.Number)

	// automatic charges: the owner hears about successes and failures
	e.app.Store.SaveStripeCustomer(store.StripeCustomer{CompanyID: 1, ClientID: 1, CustomerID: "cus_1", CardToken: "tok_aaaaaaaaaaaaaaaaaaaaaaaa"})
	e.app.Store.SaveCard(&store.SavedCard{CompanyID: 1, ClientID: 1, PaymentMethod: "pm_1", Brand: "visa", Last4: "4242", ExpMonth: 4, ExpYear: 2030})
	b.post("/c/1/recurring/new", url.Values{"name": {"Hosting"}, "client_id": {"1"}, "currency": {"EUR"}, "interval_count": {"1"},
		"interval_unit": {"month"}, "next_run": {tomorrow()}, "due_days": {"30"}, "card_payment": {"1"}, "auto_charge": {"1"}, "auto_send": {"1"},
		"line_desc": {"Hosting"}, "line_qty": {"1"}, "line_price": {"10"}, "line_tax": {"0"}})
	b.post("/c/1/recurring/1/run", nil)
	subjects, bodies = api.mailsTo("owner@alpha.test")
	if len(subjects) != 2 || !strings.HasPrefix(subjects[1], "Payment received") || !strings.Contains(bodies[1], "The saved card Visa •••• 4242") {
		t.Fatalf("owner notification of an automatic charge: %v", subjects)
	}

	e.app.Store.SaveCard(&store.SavedCard{CompanyID: 1, ClientID: 1, PaymentMethod: "pm_declined", Brand: "visa", Last4: "0002", ExpMonth: 4, ExpYear: 2030})
	b.post("/c/1/recurring/1/run", nil)
	subjects, bodies = api.mailsTo("owner@alpha.test")
	if len(subjects) != 3 || !strings.HasPrefix(subjects[2], "Card payment failed") || !strings.Contains(bodies[2], "Your card was declined.") ||
		!strings.Contains(bodies[2], "The client has been e-mailed a link") {
		t.Fatalf("owner notification of a declined charge: %v", subjects)
	}
	if s, _ := api.mailsTo("ap@globex.test"); !strings.HasPrefix(s[len(s)-1], "Card payment failed") {
		t.Fatalf("the client is told too: %v", s)
	}
	list, _, _ := e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
	b.get("/c/1/invoices/" + strconv.FormatInt(list[0].ID, 10))
	b.must("Charge declined")
	b.must("Your card was declined.")

	// a charge made from the interface shows its outcome there: no e-mail
	cards, _ := e.app.Store.Cards(1, 1)
	for _, c := range cards {
		if c.PaymentMethod == "pm_1" {
			b.post("/c/1/invoices/"+strconv.FormatInt(list[0].ID, 10)+"/charge", url.Values{"card": {strconv.FormatInt(c.ID, 10)}})
		}
	}
	if inv, _ := e.app.Store.Invoice(1, list[0].ID); inv.Status != store.StatusPaid {
		t.Fatalf("manual charge: %s", inv.Status)
	}
	if s, _ := api.mailsTo("owner@alpha.test"); len(s) != 3 {
		t.Fatalf("manual charges are not notified: %v", s)
	}

	// the "update my card" page shows the default card
	anon.get("/card/tok_aaaaaaaaaaaaaaaaaaaaaaaa")
	anon.must("•••• •••• •••• 0002")
	anon.must("Active — used for your next invoices")
	anon.must("Replace my card")
}
