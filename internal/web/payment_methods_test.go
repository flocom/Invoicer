package web

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/store"
)

func TestPaymentMethodsPerInvoice(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "lang": {"en"}})
	b.post("/c/1/settings/bank/accounts", url.Values{"currency": {"EUR"}, "label": {"Main"}, "iban": {"CH9300762011623852957"}, "bic": {"POFICHBEXXX"}})

	// without Stripe the card option is shown disabled and its value kept
	b.get("/c/1/recurring/new")
	b.must("Stripe is not set up")
	b.must(`<input type="hidden" name="card_payment" value="1">`)

	e.app.Store.UpdateCompanyStripe(1, e.app.Box.Seal("sk_test_xxxxxxxxxxxxxxxxxxxx", "company:1:stripe"),
		e.app.Box.Seal("whsec", "company:1:stripe_whsec"), "we_1", "Test")
	b.get("/c/1/invoices/new")
	b.must(`name="card_payment" value="1" checked`)

	today := e.app.Today()
	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	issue := func(card string) *store.Invoice {
		form := url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {today}, "due_date": {due},
			"line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"100"}, "line_tax": {"0"}, "action": {"issue"}, "bank_account": {"none"}}
		if card != "" {
			form.Set("card_payment", card)
		}
		b.post("/c/1/invoices/new", form)
		list, _, _ := e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
		inv, err := e.app.Store.Invoice(1, list[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		return inv
	}
	co, _ := e.app.Store.Company(1)

	withCard := issue("1")
	if !withCard.CardPayment || !withCard.CardPayable(co) {
		t.Fatal("card payment should be offered")
	}
	anon := e.browser()
	anon.get("/i/" + withCard.PublicToken)
	anon.must("/pay/" + withCard.PublicToken)

	noCard := issue("")
	if noCard.CardPayment || noCard.CardPayable(co) {
		t.Fatal("card payment should be off")
	}
	anon.get("/i/" + noCard.PublicToken)
	if strings.Contains(anon.last, "/pay/"+noCard.PublicToken) {
		t.Fatal("pay button shown although card payment is off")
	}
	// the pay link falls back to the invoice page instead of creating a Checkout session
	resp, err := anon.c.Get(e.srv.URL + "/pay/" + noCard.PublicToken)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasSuffix(resp.Request.URL.Path, "/i/"+noCard.PublicToken) {
		t.Fatalf("pay redirect: %s", resp.Request.URL)
	}

	// payment methods can be changed on an unpaid invoice
	b.post("/c/1/invoices/"+strconv.FormatInt(noCard.ID, 10)+"/bank", url.Values{"bank_account": {"auto"}, "card_payment": {"1"}})
	noCard, _ = e.app.Store.Invoice(1, noCard.ID)
	if !noCard.CardPayment || noCard.BankAccountID != store.BankAuto {
		t.Fatalf("update: card=%v bank=%d", noCard.CardPayment, noCard.BankAccountID)
	}

	// recurring schedules pass the choice on to generated invoices
	b.post("/c/1/recurring/new", url.Values{"name": {"Hosting"}, "client_id": {"1"}, "currency": {"EUR"}, "interval_count": {"1"},
		"interval_unit": {"month"}, "next_run": {today}, "due_days": {"30"}, "bank_account": {"none"},
		"line_desc": {"Hosting"}, "line_qty": {"1"}, "line_price": {"10"}, "line_tax": {"0"}})
	recs, _ := e.app.Store.RecurringList(1)
	if len(recs) != 1 || recs[0].CardPayment || recs[0].BankAccountID != store.BankNone {
		t.Fatalf("recurring saved: %+v", recs)
	}
	b.post("/c/1/recurring/"+strconv.FormatInt(recs[0].ID, 10)+"/run", nil)
	list, _, _ := e.app.Store.Invoices(1, store.InvoiceFilter{Limit: 1})
	gen, _ := e.app.Store.Invoice(1, list[0].ID)
	if gen.RecurringID != recs[0].ID || gen.CardPayment || gen.BankAccountID != store.BankNone {
		t.Fatalf("generated invoice: recurring=%d card=%v bank=%d", gen.RecurringID, gen.CardPayment, gen.BankAccountID)
	}
}
