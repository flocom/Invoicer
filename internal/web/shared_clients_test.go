package web

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSharedClients(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	b.post("/companies/new", url.Values{"name": {"Beta"}, "currency": {"EUR"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Private Co"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Shared Co"}, "lang": {"en"}, "shared": {"1"}})
	b.post("/c/1/api/clients", url.Values{"name": {"Quick Shared"}, "lang": {"en"}, "shared": {"1"}})

	// shared clients appear in every company, private ones only in theirs
	b.get("/c/2/clients")
	b.must("Shared Co")
	b.must("Quick Shared")
	if strings.Contains(b.last, "Private Co") {
		t.Fatal("private client leaked into another company")
	}
	b.get("/c/2/invoices/new")
	b.must(">Shared Co</option>")
	b.get("/c/2/clients/1")
	if b.status != 404 {
		t.Fatalf("private client reachable from another company: %d", b.status)
	}

	// invoice it from the second company
	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	b.post("/c/2/invoices/new", url.Values{"client_id": {"2"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {e.app.Today()}, "due_date": {due},
		"line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"100"}, "line_tax": {"0"}, "action": {"issue"}})
	b.get("/c/2/clients/2")
	b.must("Shared")
	b.must(`href="/c/1/clients/2"`) // the same record in the other company

	// edits made in one company apply everywhere
	b.post("/c/2/clients/2/edit", url.Values{"name": {"Shared Co Ltd"}, "lang": {"en"}, "shared": {"1"}})
	if cl, _ := e.app.Store.Client(1, 2); cl == nil || cl.Name != "Shared Co Ltd" || cl.CompanyID != 1 {
		t.Fatalf("edit from another company: %+v", cl)
	}

	// it cannot become private while another company uses it
	b.post("/c/1/clients/2/edit", url.Values{"name": {"Shared Co Ltd"}, "lang": {"en"}})
	b.must("must stay shared")
	if cl, _ := e.app.Store.Client(2, 2); cl == nil || !cl.Shared {
		t.Fatal("client was unshared although used elsewhere")
	}
	// nor be deleted while it has invoices
	b.post("/c/1/clients/2/delete", nil)
	if cl, _ := e.app.Store.Client(1, 2); cl == nil {
		t.Fatal("client with invoices deleted")
	}

	// an unused shared client made private from Beta moves to Beta
	b.post("/c/2/clients/3/edit", url.Values{"name": {"Quick Shared"}, "lang": {"en"}})
	if cl, err := e.app.Store.Client(2, 3); err != nil || cl.Shared || cl.CompanyID != 2 {
		t.Fatalf("unshare: %+v %v", cl, err)
	}
	if _, err := e.app.Store.Client(1, 3); err == nil {
		t.Fatal("unshared client still visible in the first company")
	}
}
