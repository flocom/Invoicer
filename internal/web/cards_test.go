package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/mailer"
	"github.com/flocom/invoicer/internal/store"
	"github.com/flocom/invoicer/internal/stripe"
)

// fakeAPIs stands in for the Stripe and Resend APIs.
type fakeAPIs struct {
	mu        sync.Mutex
	srv       *httptest.Server
	charges   []url.Values
	checkouts []url.Values
	idem      []string
	mails     []map[string]any
	n         int
}

func newFakeAPIs(t *testing.T) *fakeAPIs {
	f := &fakeAPIs{}
	card := func(pm string) string {
		last4 := "4242"
		if pm == "pm_declined" {
			last4 = "0002"
		}
		return fmt.Sprintf(`{"id":%q,"customer":"cus_1","card":{"brand":"visa","last4":%q,"exp_month":4,"exp_year":2030}}`, pm, last4)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == "/emails":
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			f.mails = append(f.mails, m)
			io.WriteString(w, `{"id":"em_1"}`)
		case p == "/v1/customers":
			io.WriteString(w, `{"id":"cus_1"}`)
		case p == "/v1/checkout/sessions" && r.Method == "POST":
			f.checkouts = append(f.checkouts, r.PostForm)
			f.n++
			fmt.Fprintf(w, `{"id":"cs_%s_%d","url":"https://checkout.test/%d","status":"open","mode":%q,"expires_at":%d}`,
				r.PostForm.Get("mode"), f.n, f.n, r.PostForm.Get("mode"), time.Now().Add(time.Hour).Unix())
		case strings.HasPrefix(p, "/v1/checkout/sessions/cs_setup_"):
			fmt.Fprintf(w, `{"id":%q,"status":"complete","mode":"setup","setup_intent":"seti_1","customer":"cus_1"}`, strings.TrimPrefix(p, "/v1/checkout/sessions/"))
		case p == "/v1/setup_intents/seti_1":
			fmt.Fprintf(w, `{"status":"succeeded","payment_method":%s}`, card("pm_1"))
		case p == "/v1/payment_intents" && r.Method == "POST":
			f.charges = append(f.charges, r.PostForm)
			f.idem = append(f.idem, r.Header.Get("Idempotency-Key"))
			if r.PostForm.Get("payment_method") == "pm_declined" {
				w.WriteHeader(402)
				io.WriteString(w, `{"error":{"message":"Your card was declined.","code":"card_declined","payment_intent":{"id":"pi_fail","status":"requires_payment_method"}}}`)
				return
			}
			fmt.Fprintf(w, `{"id":"pi_%d","status":"succeeded","amount":%s,"amount_received":%s,"currency":%q}`,
				len(f.charges), r.PostForm.Get("amount"), r.PostForm.Get("amount"), r.PostForm.Get("currency"))
		case strings.HasSuffix(p, "/detach"):
			io.WriteString(w, `{}`)
		default:
			io.WriteString(w, `{"data":[]}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	oldS, oldM := stripe.APIBase, mailer.APIBase
	stripe.APIBase, mailer.APIBase = f.srv.URL, f.srv.URL
	t.Cleanup(func() { stripe.APIBase, mailer.APIBase = oldS, oldM })
	return f
}

func (f *fakeAPIs) mailSubjects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.mails {
		out = append(out, fmt.Sprint(m["subject"]))
	}
	return out
}

func TestSavedCardsAndCharges(t *testing.T) {
	e := newEnv(t)
	api := newFakeAPIs(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.SetSetting("base_url", e.srv.URL)
	e.app.Store.UpdateCompanyStripe(1, e.app.Box.Seal("sk_test_xxxxxxxxxxxxxxxxxxxx", "company:1:stripe"),
		e.app.Box.Seal("whsec", "company:1:stripe_whsec"), "we_1", "Test")
	e.app.Store.UpdateCompanyEmail(1, e.app.Box.Seal("re_test_key", "company:1:resend"), false, "billing@alpha.test", "", "")
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "email": {"ap@globex.test"}, "lang": {"en"}})

	// the owner creates the "update my card" link and e-mails it
	b.get("/c/1/clients/1")
	b.must("Saved cards")
	b.post("/c/1/clients/1/card-link", url.Values{"send": {"1"}})
	b.get("/c/1/clients/1")
	m := regexp.MustCompile(`/card/([A-Za-z0-9_-]{20,})`).FindStringSubmatch(b.last)
	if m == nil {
		t.Fatalf("no card link on the client page")
	}
	token := m[1]
	if s := api.mailSubjects(); len(s) != 1 || !strings.Contains(s[0], "payment card") {
		t.Fatalf("card link e-mail: %v", s)
	}

	// the client saves a card through Stripe Checkout (setup mode)
	anon := e.browser()
	anon.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	anon.get("/card/" + token)
	anon.must("Save a card")
	resp, err := anon.c.Get(e.srv.URL + "/card/" + token + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "https://checkout.test/") {
		t.Fatalf("setup redirect: %d %s", resp.StatusCode, loc)
	}
	var setupID string
	e.app.Store.DB.QueryRow(`SELECT id FROM card_setups`).Scan(&setupID)
	anon.get("/card/" + token + "?session_id=" + setupID)
	anon.must("your card has been saved")
	anon.must("Visa •••• 4242")
	anon.get("/card/notavalidtokennotavalidtoken")
	if anon.status != 404 {
		t.Fatalf("unknown card token: %d", anon.status)
	}

	// manual charge of an invoice, with the card chosen on the client page
	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()}, "due_date": {due},
		"line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"120"}, "line_tax": {"0"}, "action": {"issue"}, "card_payment": {"1"}})
	b.get("/c/1/clients/1")
	b.must("Charge an invoice")
	cards, _ := e.app.Store.Cards(1, 1)
	if len(cards) != 1 || !cards[0].Default {
		t.Fatalf("cards: %+v", cards)
	}
	b.post("/c/1/clients/1/charge", url.Values{"invoice": {"1"}, "card": {strconv.FormatInt(cards[0].ID, 10)}})
	inv, _ := e.app.Store.Invoice(1, 1)
	if inv.Status != store.StatusPaid {
		t.Fatalf("manual charge: status %s", inv.Status)
	}
	if c := api.charges[0]; c.Get("amount") != "12000" || c.Get("customer") != "cus_1" || c.Get("payment_method") != "pm_1" ||
		c.Get("off_session") != "true" || api.idem[0] == "" {
		t.Fatalf("charge request: %v idem=%q", c, api.idem[0])
	}
	b.get("/c/1/invoices/1")
	b.must("Card charged")

	// recurring invoice charged automatically on the default card
	b.post("/c/1/recurring/new", url.Values{"name": {"Hosting"}, "client_id": {"1"}, "currency": {"EUR"}, "interval_count": {"1"},
		"interval_unit": {"month"}, "next_run": {e.app.Today()}, "due_days": {"30"}, "card_payment": {"1"}, "auto_charge": {"1"}, "auto_send": {"1"},
		"line_desc": {"Hosting"}, "line_qty": {"1"}, "line_price": {"10"}, "line_tax": {"0"}})
	recs, _ := e.app.Store.RecurringList(1)
	if len(recs) != 1 || !recs[0].AutoCharge {
		t.Fatalf("recurring: %+v", recs)
	}
	before := len(api.mailSubjects())
	b.post("/c/1/recurring/1/run", nil)
	list, _, _ := e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
	gen, _ := e.app.Store.Invoice(1, list[0].ID)
	if gen.RecurringID != 1 || !gen.AutoCharge || gen.Status != store.StatusPaid {
		t.Fatalf("auto charge: recurring=%d auto=%v status=%s", gen.RecurringID, gen.AutoCharge, gen.Status)
	}
	if s := api.mailSubjects()[before:]; len(s) != 1 || !strings.Contains(s[0], "Payment received") {
		t.Fatalf("after an automatic charge only the receipt is sent: %v", s)
	}

	// a declined card: the client gets the failure e-mail with the update link
	e.app.Store.SaveCard(&store.SavedCard{CompanyID: 1, ClientID: 1, PaymentMethod: "pm_declined", Brand: "visa", Last4: "0002", ExpMonth: 4, ExpYear: 2030})
	before = len(api.mailSubjects())
	b.post("/c/1/recurring/1/run", nil)
	list, _, _ = e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
	failed, _ := e.app.Store.Invoice(1, list[0].ID)
	if failed.ID == gen.ID || failed.Status != store.StatusOpen || failed.SentAt == 0 {
		t.Fatalf("declined: id=%d status=%s sent=%d", failed.ID, failed.Status, failed.SentAt)
	}
	api.mu.Lock()
	mails := api.mails[before:]
	api.mu.Unlock()
	if len(mails) != 1 || !strings.Contains(fmt.Sprint(mails[0]["subject"]), "Card payment failed") ||
		!strings.Contains(fmt.Sprint(mails[0]["html"]), "/card/"+token) || !strings.Contains(fmt.Sprint(mails[0]["html"]), "/i/"+failed.PublicToken) {
		t.Fatalf("failure e-mail: %v", mails)
	}
	if atts, _ := mails[0]["attachments"].([]any); len(atts) != 1 {
		t.Fatal("the invoice should be attached to the failure e-mail")
	}
	r, _ := e.app.Store.Recurring(1, 1)
	if !strings.Contains(r.LastError, "declined") {
		t.Fatalf("recurring error: %q", r.LastError)
	}
	b.get("/c/1/invoices/" + strconv.FormatInt(failed.ID, 10))
	b.must("Card charge failed")

	// paying it online saves the new card on the client's customer
	resp, err = anon.c.Get(e.srv.URL + "/pay/" + failed.PublicToken)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if co := api.checkouts[len(api.checkouts)-1]; co.Get("mode") != "payment" || co.Get("customer") != "cus_1" ||
		co.Get("payment_intent_data[setup_future_usage]") != "off_session" {
		t.Fatalf("checkout of an auto-charged invoice: %v", co)
	}

	// a manual retry with a declined card reports the reason
	b.post("/c/1/clients/1/charge", url.Values{"invoice": {strconv.FormatInt(failed.ID, 10)}, "card": {"2"}})
	b.must("Your card was declined.")
	// then with the good card
	b.post("/c/1/clients/1/charge", url.Values{"invoice": {strconv.FormatInt(failed.ID, 10)}, "card": {strconv.FormatInt(cards[0].ID, 10)}})
	if inv, _ := e.app.Store.Invoice(1, failed.ID); inv.Status != store.StatusPaid {
		t.Fatalf("retry: %s", inv.Status)
	}

	// cards can be made default and removed
	b.post("/c/1/clients/1/cards/"+strconv.FormatInt(cards[0].ID, 10)+"/default", nil)
	if c, _ := e.app.Store.DefaultCard(1, 1); c == nil || c.PaymentMethod != "pm_1" {
		t.Fatalf("default card: %+v", c)
	}
	b.post("/c/1/clients/1/cards/"+strconv.FormatInt(cards[0].ID, 10)+"/delete", nil)
	if c, _ := e.app.Store.DefaultCard(1, 1); c == nil || c.PaymentMethod != "pm_declined" {
		t.Fatalf("after delete the remaining card becomes default: %+v", c)
	}
	// a card of another client cannot be used
	b.post("/c/1/clients/new", url.Values{"name": {"Initech"}, "lang": {"en"}})
	n := len(api.charges)
	b.post("/c/1/clients/2/charge", url.Values{"invoice": {strconv.FormatInt(failed.ID, 10)}, "card": {"2"}})
	if len(api.charges) != n {
		t.Fatal("charged an invoice of another client")
	}
}
