package web

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/app"
	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
)

func TestCopyClientSuggestionsBankAndDestroy(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)
	e.app.SaveAddressConfig(false, "off", "", false) // no network in tests
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	b.post("/companies/new", url.Values{"name": {"Beta"}, "currency": {"CHF"}, "lang": {"en"}})
	b.post("/c/1/settings/bank", url.Values{"iban": {"CH9300762011623852957"}, "bic": {"POFICHBEXXX"}, "email_bank_details": {"1"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "email": {"ap@globex.test"}, "lang": {"fr"}})
	today := e.app.Today()
	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"fr"}, "issue_date": {today}, "due_date": {due},
		"line_desc": {"Monthly maintenance"}, "line_qty": {"1"}, "line_price": {"120"}, "line_tax": {"8.1"}, "action": {"issue"}})

	// products & services suggestions
	b.get("/c/1/api/lines?q=maint")
	var sug struct {
		Suggestions []struct{ Description, Price, Tax, Currency string }
	}
	json.Unmarshal([]byte(b.last), &sug)
	if len(sug.Suggestions) != 1 || sug.Suggestions[0].Price != "120.00" || sug.Suggestions[0].Tax != "8.1" || sug.Suggestions[0].Currency != "EUR" {
		t.Fatalf("line suggestions: %s", b.last)
	}

	// address API answers (empty with lookups disabled) and requires a session
	b.get("/api/address?q=Bahnhofstrasse")
	if !strings.Contains(b.last, `"suggestions":[]`) {
		t.Fatalf("address api: %s", b.last)
	}
	anon := e.browser()
	anon.get("/api/address?q=Bahnhofstrasse")
	if strings.Contains(anon.last, "suggestions") {
		t.Fatal("address api must require login")
	}

	// copy the client to Beta, then its Alpha invoices show on the Beta page
	b.get("/c/1/clients/1")
	b.post("/c/1/clients/1/copy", url.Values{"company": {"2"}})
	b.must("Client copied to Beta")
	b.must("Invoices from your other companies")
	b.must("Alpha")
	if cl, err := e.app.Store.Client(2, 2); err != nil || cl.Email != "ap@globex.test" || cl.Lang != "fr" {
		t.Fatalf("copied client: %+v %v", cl, err)
	}
	b.post("/c/1/clients/1/copy", url.Values{"company": {"2"}})
	b.must("already exists in Beta")

	// bank details are rendered in the e-mail content
	co, _ := e.app.Store.Company(1)
	inv, _ := e.app.Store.Invoice(1, 1)
	lines := app.BankLines(co, inv, "fr")
	if len(lines) == 0 || lines[len(lines)-1][1] != inv.Number {
		t.Fatalf("bank lines: %v", lines)
	}

	// deleting an issued invoice: members cannot, admins must type the number
	e.app.Store.CreateUser("mem@example.test", "Mem", security.HashPassword(pw), store.RoleMember, "en")
	e.app.Store.SetUserCompanies(2, []int64{1})
	m := e.browser()
	m.get("/login")
	m.post("/login", url.Values{"email": {"mem@example.test"}, "password": {pw}})
	m.get("/c/1/invoices/1")
	m.post("/c/1/invoices/1/destroy", url.Values{"confirm": {inv.Number}})
	if m.status != 403 {
		t.Fatalf("members must not delete issued invoices, got %d", m.status)
	}
	b.get("/c/1/invoices/1")
	b.must("generally prohibited")
	b.post("/c/1/invoices/1/destroy", url.Values{"confirm": {"WRONG"}})
	b.must("does not match")
	b.post("/c/1/invoices/1/destroy", url.Values{"confirm": {inv.Number}})
	b.must("deleted (recorded in the audit log)")
	if _, err := e.app.Store.Invoice(1, 1); err == nil {
		t.Fatal("invoice still exists")
	}
	entries, _ := e.app.Store.AuditEntries(20)
	found := false
	for _, a := range entries {
		found = found || a.Action == "invoice.deleted_issued"
	}
	if !found {
		t.Fatal("deletion not audited")
	}
}
