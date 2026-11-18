package web

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// The client record keeps a phone number, and invoice lists show and search
// the client's contact person.
func TestClientPhoneAndInvoiceContact(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Acme"}, "contact_name": {"Jane Roe"}, "phone": {"+33 6 12 34 56 78"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Other Co"}, "contact_name": {"Bob Smith"}, "lang": {"en"}})

	cl, err := e.app.Store.Client(1, 1)
	if err != nil || cl.Phone != "+33 6 12 34 56 78" {
		t.Fatalf("phone not saved: %+v %v", cl, err)
	}
	b.get("/c/1/clients/1")
	b.must(`href="tel:&#43;33%206%2012%2034%2056%2078"`)
	b.get("/c/1/clients/1/edit")
	b.must(`value="&#43;33 6 12 34 56 78"`)
	b.get("/c/1/clients?q=12+34")
	b.must("Acme")

	// editing keeps or clears it
	b.post("/c/1/clients/1/edit", url.Values{"name": {"Acme"}, "contact_name": {"Jane Roe"}, "lang": {"en"}})
	if cl, _ := e.app.Store.Client(1, 1); cl.Phone != "" {
		t.Fatalf("phone not cleared: %q", cl.Phone)
	}

	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	for _, id := range []string{"1", "2"} {
		b.post("/c/1/invoices/new", url.Values{"client_id": {id}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()}, "due_date": {due},
			"line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"100"}, "line_tax": {"0"}, "action": {"issue"}})
	}

	// the contact person shows under the client name
	b.get("/c/1/invoices")
	b.must(`Acme<div class="muted small">Jane Roe</div>`)
	b.must(`Other Co<div class="muted small">Bob Smith</div>`)

	// and the search finds invoices by contact person
	b.get("/c/1/invoices?q=jane")
	b.must("Acme")
	if strings.Contains(b.last, "Other Co") {
		t.Fatal("contact search matched the wrong client")
	}
}
