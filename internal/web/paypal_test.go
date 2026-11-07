package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/paypal"
	"github.com/flocom/invoicer/internal/store"
)

// fakePayPal stands in for the PayPal REST API.
type fakePayPal struct {
	mu       sync.Mutex
	orders   map[string]map[string]any // id → order
	landing  map[string]string         // id → landing page asked
	status   map[string]string
	captures int
	n        int
	hooks    map[string]string // webhook id → url
	hookN    int
	trackers []map[string]any // tracking info added to captures
	trackErr bool             // the tracking API fails
}

func newFakePayPal(t *testing.T) *fakePayPal {
	f := &fakePayPal{orders: map[string]map[string]any{}, landing: map[string]string{}, status: map[string]string{}, hooks: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		// live app: cid/sec; sandbox app: sbx/sec2 (under /sandbox)
		user, pass, tok := "cid", "sec", "tok"
		if strings.HasPrefix(p, "/sandbox") {
			p, user, pass, tok = strings.TrimPrefix(p, "/sandbox"), "sbx", "sec2", "stok"
		}
		if p == "/v1/oauth2/token" {
			if id, sec, _ := r.BasicAuth(); id != user || sec != pass {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":"invalid_client","error_description":"Client Authentication failed"}`)
				return
			}
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, tok)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(401)
			return
		}
		order := func(id string) string {
			o := f.orders[id]
			unit := map[string]any{"custom_id": o["custom_id"], "amount": o["amount"]}
			if f.status[id] == "COMPLETED" {
				unit["payments"] = map[string]any{"captures": []map[string]any{{"id": "CAP-" + id, "status": "COMPLETED", "amount": o["amount"]}}}
			}
			b, _ := json.Marshal(map[string]any{"id": id, "status": f.status[id], "purchase_units": []any{unit},
				"links": []map[string]string{{"rel": "payer-action", "href": "https://www.paypal.test/checkoutnow?token=" + id}}})
			return string(b)
		}
		switch {
		case p == "/v1/shipping/trackers-batch" && r.Method == "POST":
			if f.trackErr {
				w.WriteHeader(500)
				io.WriteString(w, `{"name":"INTERNAL_SERVER_ERROR","message":"boom"}`)
				return
			}
			var body struct {
				Trackers []map[string]any `json:"trackers"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.trackers = append(f.trackers, body.Trackers...)
			io.WriteString(w, `{"tracker_identifiers":[],"errors":[]}`)
		case p == "/v1/notifications/webhooks" && r.Method == "POST":
			var body struct {
				URL string `json:"url"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			for _, u := range f.hooks {
				if u == body.URL {
					w.WriteHeader(400)
					io.WriteString(w, `{"name":"WEBHOOK_URL_ALREADY_EXISTS","message":"Webhook URL already exists"}`)
					return
				}
			}
			f.hookN++
			id := fmt.Sprintf("WH%d", f.hookN)
			f.hooks[id] = body.URL
			fmt.Fprintf(w, `{"id":%q,"url":%q}`, id, body.URL)
		case p == "/v1/notifications/webhooks" && r.Method == "GET":
			var parts []string
			for id, u := range f.hooks {
				parts = append(parts, fmt.Sprintf(`{"id":%q,"url":%q}`, id, u))
			}
			fmt.Fprintf(w, `{"webhooks":[%s]}`, strings.Join(parts, ","))
		case strings.HasPrefix(p, "/v1/notifications/webhooks/") && r.Method == "DELETE":
			id := strings.TrimPrefix(p, "/v1/notifications/webhooks/")
			if _, ok := f.hooks[id]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(f.hooks, id)
			w.WriteHeader(204)
		case p == "/v1/notifications/verify-webhook-signature":
			var body struct {
				Sig       string `json:"transmission_sig"`
				WebhookID string `json:"webhook_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if _, ok := f.hooks[body.WebhookID]; body.Sig == "good" && ok {
				io.WriteString(w, `{"verification_status":"SUCCESS"}`)
			} else {
				io.WriteString(w, `{"verification_status":"FAILURE"}`)
			}
		case r.Method == "POST" && p == "/v2/checkout/orders":
			var body struct {
				PurchaseUnits []map[string]any `json:"purchase_units"`
				PaymentSource struct {
					PayPal struct {
						Ctx map[string]string `json:"experience_context"`
					} `json:"paypal"`
					Card *struct {
						Attributes struct {
							Verification struct {
								Method string `json:"method"`
							} `json:"verification"`
						} `json:"attributes"`
					} `json:"card"`
				} `json:"payment_source"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.n++
			id := fmt.Sprintf("ORDER%d", f.n)
			f.orders[id] = body.PurchaseUnits[0]
			f.landing[id] = body.PaymentSource.PayPal.Ctx["landing_page"] + "|" + body.PaymentSource.PayPal.Ctx["return_url"]
			if c := body.PaymentSource.Card; c != nil {
				f.landing[id] = "CARD|" + c.Attributes.Verification.Method
			}
			f.status[id] = "PAYER_ACTION_REQUIRED"
			w.WriteHeader(201)
			io.WriteString(w, order(id))
		case r.Method == "GET" && strings.HasPrefix(p, "/v2/checkout/orders/"):
			io.WriteString(w, order(strings.TrimPrefix(p, "/v2/checkout/orders/")))
		case r.Method == "POST" && strings.HasSuffix(p, "/capture"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/v2/checkout/orders/"), "/capture")
			switch f.status[id] {
			case "APPROVED":
				f.captures++
				f.status[id] = "COMPLETED"
				w.WriteHeader(201)
				io.WriteString(w, order(id))
			case "COMPLETED":
				w.WriteHeader(422)
				io.WriteString(w, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"ORDER_ALREADY_CAPTURED"}]}`)
			default:
				w.WriteHeader(422)
				io.WriteString(w, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"ORDER_NOT_APPROVED"}]}`)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	oldL, oldS := paypal.LiveBase, paypal.SandboxBase
	paypal.LiveBase, paypal.SandboxBase = srv.URL, srv.URL+"/sandbox"
	t.Cleanup(func() { paypal.LiveBase, paypal.SandboxBase = oldL, oldS })
	return f
}

func (f *fakePayPal) approve(id string) {
	f.mu.Lock()
	f.status[id] = "APPROVED"
	f.mu.Unlock()
}

func TestPayPalPayments(t *testing.T) {
	e := newEnv(t)
	pp := newFakePayPal(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.SetSetting("base_url", e.srv.URL)
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "lang": {"en"}})

	// connecting checks the credentials with PayPal
	b.get("/c/1/settings?tab=payments")
	b.must("Connect PayPal")
	b.post("/c/1/settings/paypal", url.Values{"paypal_client_id": {"cid"}, "paypal_secret": {"wrong"}, "paypal_card": {"1"}})
	b.must("PayPal refused these credentials")
	// sandbox credentials are recognised as such, without being told
	b.post("/c/1/settings/paypal", url.Values{"paypal_client_id": {"sbx"}, "paypal_secret": {"sec2"}, "paypal_card": {"1"}})
	b.must("PayPal is connected")
	if co, _ := e.app.Store.Company(1); !co.PayPalSandbox {
		t.Fatal("sandbox credentials not detected")
	}
	b.post("/c/1/settings/paypal", url.Values{"paypal_client_id": {"cid"}, "paypal_secret": {"sec"}, "paypal_card": {"1"}})
	b.must("PayPal is connected")
	b.must("checked every 10 minutes") // no public HTTPS: no webhook
	co, _ := e.app.Store.Company(1)
	if !co.HasPayPal() || !co.PayPalCard || co.PayPalSandbox || e.app.PayPalCredentials(co).Secret != "sec" || co.PayPalWebhookID != "" {
		t.Fatalf("paypal settings: %+v", co)
	}
	// options change without typing the secret again
	b.post("/c/1/settings/paypal", url.Values{"paypal_client_id": {"cid"}})
	if co, _ = e.app.Store.Company(1); co.PayPalCard || !co.HasPayPal() {
		t.Fatal("options not saved")
	}
	b.post("/c/1/settings/paypal", url.Values{"paypal_client_id": {"cid"}, "paypal_card": {"1"}})

	newInvoice := func(price string, extra url.Values) *store.Invoice {
		v := url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()},
			"due_date": {time.Now().AddDate(0, 0, 30).Format("2006-01-02")}, "line_desc": {"Work"}, "line_qty": {"1"},
			"line_price": {price}, "line_tax": {"0"}, "action": {"issue"}}
		for k, vs := range extra {
			v[k] = vs
		}
		b.get("/c/1/invoices/new")
		b.must(`name="paypal_payment"`)
		b.post("/c/1/invoices/new", v)
		list, _, _ := e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
		inv, _ := e.app.Store.Invoice(1, list[0].ID)
		return inv
	}
	inv := newInvoice("120", url.Values{"paypal_payment": {"1"}})

	// the payment page offers PayPal and the card through PayPal
	anon := e.browser()
	anon.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	anon.get("/i/" + inv.PublicToken)
	anon.must("/paypal/" + inv.PublicToken + `"`)
	anon.must("/paypal/" + inv.PublicToken + "?card=1")
	start := func(q string) string {
		resp, err := anon.c.Get(e.srv.URL + "/paypal/" + inv.PublicToken + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		if !strings.HasPrefix(loc, "https://www.paypal.test/checkoutnow?token=") {
			t.Fatalf("paypal redirect: %d %s", resp.StatusCode, loc)
		}
		return strings.TrimPrefix(loc, "https://www.paypal.test/checkoutnow?token=")
	}
	card := start("?card=1")
	if !strings.HasPrefix(pp.landing[card], "GUEST_CHECKOUT|"+e.srv.URL+"/paypal/"+inv.PublicToken+"/return") {
		t.Fatalf("card payment page: %s", pp.landing[card])
	}
	id := start("")
	if !strings.HasPrefix(pp.landing[id], "LOGIN|") {
		t.Fatalf("paypal account page: %s", pp.landing[id])
	}
	if a := pp.orders[id]["amount"].(map[string]any); a["value"] != "120.00" || a["currency_code"] != "EUR" {
		t.Fatalf("order amount: %v", a)
	}

	// coming back before approving does nothing
	ret := func(order string) string {
		resp, err := anon.c.Get(e.srv.URL + "/paypal/" + inv.PublicToken + "/return?token=" + order + "&PayerID=X")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	ret(id)
	if inv, _ := e.app.Store.Invoice(1, inv.ID); inv.Status != store.StatusOpen {
		t.Fatal("paid without approval")
	}
	// approved on PayPal, then back: captured once, invoice paid
	pp.approve(id)
	if loc := ret(id); loc != "/i/"+inv.PublicToken+"?paid=1" {
		t.Fatalf("return: %s", loc)
	}
	ret(id)
	got, _ := e.app.Store.Invoice(1, inv.ID)
	pays, _ := e.app.Store.Payments(inv.ID)
	if got.Status != store.StatusPaid || len(pays) != 1 || pays[0].Method != "paypal" || pays[0].Amount != 12000 ||
		pays[0].Reference != "CAP-"+id || pp.captures != 1 {
		t.Fatalf("paypal payment: %s %+v captures=%d", got.Status, pays, pp.captures)
	}
	b.get("/c/1/invoices/" + strconv.FormatInt(inv.ID, 10))
	b.must("PayPal payment received")
	// marked delivered on PayPal: no tracking number, buyer not notified
	if len(pp.trackers) != 1 {
		t.Fatalf("trackers: %v", pp.trackers)
	}
	tr := pp.trackers[0]
	if tr["transaction_id"] != "CAP-"+id || tr["status"] != "DELIVERED" || tr["notify_buyer"] != false || tr["tracking_number"] != nil {
		t.Fatalf("tracker: %v", tr)
	}
	b.must("Marked delivered on PayPal")
	// the other order (card) can no longer be taken: the invoice is paid
	pp.approve(card)
	if loc := ret(card); !strings.HasSuffix(loc, "?paypal=changed") {
		t.Fatalf("stale order: %s", loc)
	}
	if pp.captures != 1 {
		t.Fatal("an order was captured on a paid invoice")
	}

	// a client approving without coming back is caught by the follow-up job;
	// if PayPal's tracking fails, marking it delivered is retried later
	inv = newInvoice("50", url.Values{"paypal_payment": {"1"}})
	anon.get("/i/" + inv.PublicToken)
	id = start("")
	pp.approve(id)
	pp.trackErr = true
	e.app.ReconcilePayPal(t.Context())
	if got, _ := e.app.Store.Invoice(1, inv.ID); got.Status != store.StatusPaid {
		t.Fatalf("follow-up: %s", got.Status)
	}
	if len(pp.trackers) != 1 {
		t.Fatal("tracking should have failed")
	}
	pp.trackErr = false
	e.app.ReconcilePayPal(t.Context())
	if len(pp.trackers) != 2 || pp.trackers[1]["transaction_id"] != "CAP-"+id {
		t.Fatalf("tracking retried: %v", pp.trackers)
	}
	e.app.ReconcilePayPal(t.Context())
	if len(pp.trackers) != 2 {
		t.Fatal("marked delivered twice")
	}

	// an invoice without PayPal does not offer it
	inv = newInvoice("10", nil)
	anon.get("/i/" + inv.PublicToken)
	if strings.Contains(anon.last, "/paypal/") {
		t.Fatal("PayPal offered on an invoice without it")
	}
	resp, _ := anon.c.Get(e.srv.URL + "/paypal/" + inv.PublicToken)
	resp.Body.Close()
	if resp.Header.Get("Location") != "/i/"+inv.PublicToken {
		t.Fatalf("pay link of an invoice without PayPal: %s", resp.Header.Get("Location"))
	}

	// once reachable over HTTPS, the webhook is set up by itself
	e.app.Store.SetSetting("base_url", "https://inv.example.org")
	b.post("/c/1/settings/paypal", url.Values{"paypal_client_id": {"cid"}, "paypal_card": {"1"}})
	co, _ = e.app.Store.Company(1)
	if co.PayPalWebhookID == "" || pp.hooks[co.PayPalWebhookID] != "https://inv.example.org/webhooks/paypal/"+co.PublicID {
		t.Fatalf("webhook: %q %v", co.PayPalWebhookID, pp.hooks)
	}
	b.get("/c/1/settings?tab=payments")
	b.must("PayPal notifies this server of payments automatically")

	// a verified event captures the order; a forged one is refused
	inv = newInvoice("75", url.Values{"paypal_payment": {"1"}})
	id = start("")
	pp.approve(id)
	hook := func(sig string) int {
		body := fmt.Sprintf(`{"id":"WH-EVT-1","event_type":"CHECKOUT.ORDER.APPROVED","resource":{"id":%q}}`, id)
		req, _ := http.NewRequest("POST", e.srv.URL+"/webhooks/paypal/"+co.PublicID, strings.NewReader(body))
		req.Header.Set("Paypal-Transmission-Sig", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := hook("forged"); c != 400 {
		t.Fatalf("forged event: %d", c)
	}
	if got, _ := e.app.Store.Invoice(1, inv.ID); got.Status != store.StatusOpen {
		t.Fatal("a forged event was applied")
	}
	if c := hook("good"); c != 200 {
		t.Fatalf("event: %d", c)
	}
	if got, _ := e.app.Store.Invoice(1, inv.ID); got.Status != store.StatusPaid {
		t.Fatalf("webhook capture: %s", got.Status)
	}

	// the webhook follows the server to a new address
	oldHook := co.PayPalWebhookID
	e.app.Store.SetSetting("base_url", "https://new.example.net")
	e.app.Store.SetSetting("stripe_webhooks_refresh", "1")
	e.app.RefreshWebhooksIfMoved(t.Context())
	co, _ = e.app.Store.Company(1)
	if _, still := pp.hooks[oldHook]; still || pp.hooks[co.PayPalWebhookID] != "https://new.example.net/webhooks/paypal/"+co.PublicID || len(pp.hooks) != 1 {
		t.Fatalf("webhook move: %q %v", co.PayPalWebhookID, pp.hooks)
	}

	// disconnecting removes it everywhere
	b.post("/c/1/settings/paypal/disconnect", nil)
	if co, _ := e.app.Store.Company(1); co.HasPayPal() || len(pp.hooks) != 0 {
		t.Fatalf("still connected: %v", pp.hooks)
	}
}

func TestPayPalCardFieldsOnThePaymentPage(t *testing.T) {
	e := newEnv(t)
	pp := newFakePayPal(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.SetSetting("base_url", e.srv.URL)
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "lang": {"en"}})
	b.post("/c/1/settings/paypal", url.Values{"paypal_client_id": {"cid"}, "paypal_secret": {"sec"}, "paypal_card": {"1"}})
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()},
		"due_date": {time.Now().AddDate(0, 0, 30).Format("2006-01-02")}, "line_desc": {"Work"}, "line_qty": {"1"},
		"line_price": {"80"}, "line_tax": {"0"}, "action": {"issue"}, "paypal_payment": {"1"}})
	inv, _ := e.app.Store.Invoice(1, 1)

	// the page loads PayPal's card fields (shown only when the account offers
	// them), and keeps the button to PayPal's card page
	anon := e.browser()
	anon.get("/i/" + inv.PublicToken)
	anon.must("https://www.paypal.com/sdk/js?client-id=cid&amp;components=card-fields&amp;currency=EUR&amp;intent=capture")
	anon.must(`data-paypal-order="/paypal/` + inv.PublicToken + `/order"`)
	anon.must(`href="/paypal/` + inv.PublicToken + `?card=1" data-card-fallback`)

	code, out := anon.postJS("/paypal/"+inv.PublicToken+"/order", "")
	if code != 200 || out["id"] == "" || pp.landing[out["id"]] != "CARD|SCA_WHEN_REQUIRED" {
		t.Fatalf("card order: %d %v %v", code, out, pp.landing)
	}
	id := out["id"]
	// not approved (3-D Secure failed): refused, nothing recorded
	if code, out = anon.postJS("/paypal/"+inv.PublicToken+"/capture", "order="+id); code != 402 || out["error"] == "" {
		t.Fatalf("capture before approval: %d %v", code, out)
	}
	pp.approve(id)
	if code, out = anon.postJS("/paypal/"+inv.PublicToken+"/capture", "order="+id); code != 200 || out["redirect"] != "/i/"+inv.PublicToken+"?paid=1" {
		t.Fatalf("capture: %d %v", code, out)
	}
	pays, _ := e.app.Store.Payments(inv.ID)
	if got, _ := e.app.Store.Invoice(1, inv.ID); got.Status != store.StatusPaid || len(pays) != 1 || pays[0].Method != "paypal" {
		t.Fatalf("paid with the card fields: %s %+v", got.Status, pays)
	}
	// an order of another invoice is refused
	if code, _ = anon.postJS("/paypal/"+inv.PublicToken+"/capture", "order=ORDER999"); code != 404 {
		t.Fatalf("unknown order: %d", code)
	}
}
