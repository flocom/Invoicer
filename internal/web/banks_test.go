package web

import (
	"net/url"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/store"
)

func TestMultipleBankAccounts(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)
	st := e.app.Store
	b.post("/companies/new", url.Values{"name": {"Multi"}, "currency": {"CHF"}, "lang": {"en"}})
	// invalid IBAN is refused
	b.post("/c/1/settings/bank/accounts", url.Values{"currency": {"EUR"}, "iban": {"FR7630006000011234567890188"}})
	b.must("not valid")
	for _, f := range []url.Values{
		{"label": {"EUR account"}, "currency": {"EUR"}, "iban": {"FR7630006000011234567890189"}, "bic": {"AGRIFRPP"}},
		{"label": {"CHF account"}, "currency": {"CHF"}, "iban": {"CH9300762011623852957"}},
		{"label": {"USD Wise"}, "currency": {"USD"}, "extra": {"Routing 026073150\nAccount 8310006087"}},
	} {
		b.post("/c/1/settings/bank/accounts", f)
		b.must("Bank account saved")
	}
	banks, _ := st.BankAccounts(1)
	if len(banks) != 3 {
		t.Fatalf("accounts: %d", len(banks))
	}
	// automatic choice follows the invoice currency; none when no account matches
	if r := st.ResolveBank(1, store.BankAuto, "CHF"); r == nil || r.Label != "CHF account" {
		t.Fatalf("auto CHF: %+v", r)
	}
	if r := st.ResolveBank(1, store.BankAuto, "GBP"); r != nil {
		t.Fatal("no GBP account: nothing should be shown")
	}
	if r := st.ResolveBank(1, banks[0].ID, "GBP"); r == nil || r.Label != "EUR account" {
		t.Fatal("an explicit choice wins")
	}
	if st.ResolveBank(1, store.BankNone, "EUR") != nil {
		t.Fatal("none means none")
	}

	// choose an account on the invoice, then change it after issue
	b.post("/c/1/clients/new", url.Values{"name": {"Client"}, "email": {"c@example.test"}, "lang": {"en"}})
	today := e.app.Today()
	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"USD"}, "lang": {"en"}, "issue_date": {today}, "due_date": {due},
		"line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"100"}, "line_tax": {"0"}, "bank_account": {"3"}, "action": {"issue"}})
	inv, _ := st.Invoice(1, 1)
	if inv.BankAccountID != 3 {
		t.Fatalf("bank choice not saved: %d", inv.BankAccountID)
	}
	b.get("/c/1/invoices/1")
	b.must("Shown: USD Wise")
	b.post("/c/1/invoices/1/bank", url.Values{"bank_account": {"none"}})
	inv, _ = st.Invoice(1, 1)
	if inv.BankAccountID != store.BankNone {
		t.Fatal("bank choice not changed")
	}
	pub := e.browser()
	pub.get("/i/" + inv.PublicToken)
	if pub.status != 200 {
		t.Fatal("public page")
	}
	if containsAny(pub.last, "8310006087") {
		t.Fatal("bank details shown although none selected")
	}
	b.post("/c/1/invoices/1/bank", url.Values{"bank_account": {"auto"}})
	pub.get("/i/" + inv.PublicToken)
	pub.must("8310006087")

	// another company's account id is ignored
	b.post("/c/2/settings/bank/accounts", url.Values{"currency": {"CHF"}, "iban": {"CH9300762011623852957"}})
	b.post("/c/1/invoices/1/bank", url.Values{"bank_account": {"4"}})
	inv, _ = st.Invoice(1, 1)
	if inv.BankAccountID != store.BankAuto {
		t.Fatalf("foreign account accepted: %d", inv.BankAccountID)
	}

	// deleting an account falls back to automatic
	b.post("/c/1/invoices/1/bank", url.Values{"bank_account": {"3"}})
	b.post("/c/1/settings/bank/accounts/3/delete", url.Values{})
	inv, _ = st.Invoice(1, 1)
	if inv.BankAccountID != store.BankAuto {
		t.Fatal("deleted account still referenced")
	}
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if len(x) > 0 && len(s) >= len(x) {
			for i := 0; i+len(x) <= len(s); i++ {
				if s[i:i+len(x)] == x {
					return true
				}
			}
		}
	}
	return false
}
