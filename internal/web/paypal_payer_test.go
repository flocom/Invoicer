package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/store"
)

// The client written on the invoice pre-fills PayPal's forms; an address
// PayPal does not accept never blocks the payment.
func TestPayPalPayerDetails(t *testing.T) {
	e := newEnv(t)
	pp := newFakePayPal(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	e.app.Store.SetSetting("base_url", e.srv.URL)
	b.post("/c/1/settings/paypal", url.Values{"paypal_client_id": {"cid"}, "paypal_secret": {"sec"}, "paypal_card": {"1"}})
	b.must("PayPal is connected")
	b.post("/c/1/clients/new", url.Values{"name": {"Globex SA"}, "contact_name": {"Marie Claire Dupont"}, "email": {"ap@globex.test"},
		"address": {"Rue du Rhône 12\nCase postale 4\nCH-1204 Genève\nSuisse"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Initech"}, "contact_name": {"Bob"}, "email": {"bob@initech.test"},
		"address": {"1 Main Street\nSpringfield"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Umbrella"}, "email": {"pay@umbrella.test"},
		"address": {"Somewhere\n00000 Nowhere\nFrance"}, "lang": {"en"}})

	// the client picker also finds a client by its contact person
	b.get("/c/1/invoices/new")
	b.must(`data-contact="Marie Claire Dupont"`)

	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	pay := func(client string) string {
		t.Helper()
		b.post("/c/1/invoices/new", url.Values{"client_id": {client}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()},
			"due_date": {due}, "line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"50"}, "line_tax": {"0"}, "action": {"issue"},
			"paypal_payment": {"1"}})
		list, _, _ := e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
		inv, _ := e.app.Store.Invoice(1, list[0].ID)
		anon := e.browser()
		anon.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := anon.c.Get(e.srv.URL + "/paypal/" + inv.PublicToken)
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

	// e-mail, contact name and the address, read line by line
	p := pp.payers[pay("1")]
	name, _ := p["name"].(map[string]string)
	addr, _ := p["address"].(map[string]string)
	if p["email"] != "ap@globex.test" || name["given_name"] != "Marie Claire" || name["surname"] != "Dupont" {
		t.Fatalf("payer: %v", p)
	}
	if addr["country_code"] != "CH" || addr["postal_code"] != "1204" || addr["admin_area_2"] != "Genève" ||
		addr["address_line_1"] != "Rue du Rhône 12" || addr["address_line_2"] != "Case postale 4" {
		t.Fatalf("address: %v", addr)
	}

	// no country on the address, a one-word contact: only the e-mail is sent
	p = pp.payers[pay("2")]
	if p["email"] != "bob@initech.test" || p["name"].(map[string]string) != nil || p["address"].(map[string]string) != nil {
		t.Fatalf("partial payer: %#v", p)
	}

	// PayPal refuses the address: the order is made without it
	id := pay("3")
	p = pp.payers[id]
	if pp.rejected != 1 || p["email"] != "pay@umbrella.test" || p["address"].(map[string]string) != nil {
		t.Fatalf("address refused: rejected=%d payer=%#v", pp.rejected, p)
	}
}
