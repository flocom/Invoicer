package web

import (
	"encoding/json"
	"net/url"
	"testing"
)

func TestQuickCreateClient(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"fr"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "lang": {"en"}})

	// both editors offer the searchable picker and the creation dialog
	for _, p := range []string{"/c/1/invoices/new", "/c/1/recurring/new"} {
		b.get(p)
		b.must("data-client-select")
		b.must("data-client-dialog")
		b.must(`action="/c/1/api/clients"`)
	}

	b.post("/c/1/api/clients", url.Values{"name": {"Initech"}, "email": {"ap@initech.test"}, "lang": {"fr"}, "currency": {"CHF"}})
	var res struct {
		ID                   int64
		Name, Lang, Currency string
	}
	if err := json.Unmarshal([]byte(b.last), &res); err != nil || b.status != 200 || res.ID == 0 || res.Name != "Initech" ||
		res.Lang != "fr" || res.Currency != "CHF" {
		t.Fatalf("quick create: %d %s", b.status, b.last)
	}
	b.get("/c/1/invoices/new")
	b.must(`data-currency="CHF"`)
	b.must(">Initech</option>")

	// validation errors come back as JSON
	b.post("/c/1/api/clients", url.Values{"name": {""}})
	if b.status != 400 {
		t.Fatalf("empty name: %d %s", b.status, b.last)
	}
	b.must(`"error"`)
	b.post("/c/1/api/clients", url.Values{"name": {"Bad"}, "email": {"not-an-email"}})
	if b.status != 400 {
		t.Fatalf("bad email: %d %s", b.status, b.last)
	}

	// CSRF is enforced
	b.post("/c/1/api/clients", url.Values{"name": {"Evil"}, "csrf": {"nope"}})
	if b.status != 403 {
		t.Fatalf("csrf: %d", b.status)
	}
}
