package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/flocom/invoicer/internal/stripe"
)

// fakeHooks is a Stripe account's list of webhook endpoints.
type fakeHooks struct {
	mu       sync.Mutex
	eps      map[string]string // id → url
	n        int
	failPost bool
	failDel  map[string]bool
	deleted  []string
}

func newFakeHooks(t *testing.T, eps map[string]string) *fakeHooks {
	f := &fakeHooks{eps: eps, failDel: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/webhook_endpoints":
			var ids []string
			for id := range f.eps {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			var parts []string
			for _, id := range ids {
				parts = append(parts, fmt.Sprintf(`{"id":%q,"url":%q}`, id, f.eps[id]))
			}
			fmt.Fprintf(w, `{"data":[%s],"has_more":false}`, strings.Join(parts, ","))
		case r.Method == "POST" && r.URL.Path == "/v1/webhook_endpoints":
			if f.failPost {
				w.WriteHeader(500)
				io.WriteString(w, `{"error":{"message":"boom"}}`)
				return
			}
			f.n++
			id := fmt.Sprintf("we_new%d", f.n)
			f.eps[id] = r.PostForm.Get("url")
			fmt.Fprintf(w, `{"id":%q,"secret":"whsec_%d"}`, id, f.n)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/webhook_endpoints/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/webhook_endpoints/")
			if f.failDel[id] {
				w.WriteHeader(500)
				io.WriteString(w, `{"error":{"message":"boom"}}`)
				return
			}
			if _, ok := f.eps[id]; !ok {
				w.WriteHeader(404)
				io.WriteString(w, `{"error":{"message":"No such webhook endpoint"}}`)
				return
			}
			delete(f.eps, id)
			f.deleted = append(f.deleted, id)
			io.WriteString(w, `{"deleted":true}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	old := stripe.APIBase
	stripe.APIBase = srv.URL
	t.Cleanup(func() { stripe.APIBase = old })
	return f
}

func TestStripeWebhooksFollowTheServer(t *testing.T) {
	e := newEnv(t)
	st := e.app.Store
	co, _ := st.CreateCompany("pubA", "Alpha", "EUR", "en", 0)
	arch, _ := st.CreateCompany("pubB", "Beta", "EUR", "en", 0)
	for _, c := range []int64{co.ID, arch.ID} {
		cs := fmt.Sprint(c)
		st.UpdateCompanyStripe(c, e.app.Box.Seal("sk_test_x", "company:"+cs+":stripe"), e.app.Box.Seal("whsec_old", "company:"+cs+":stripe_whsec"),
			map[int64]string{co.ID: "we_oldA", arch.ID: "we_oldB"}[c], "Acct")
	}
	st.SetCompanyArchived(arch.ID, true)
	f := newFakeHooks(t, map[string]string{
		"we_oldA":    "https://old.example.com/webhooks/stripe/pubA",
		"we_orphanA": "https://older.example.net/webhooks/stripe/pubA", // id forgotten long ago
		"we_oldB":    "https://old.example.com/webhooks/stripe/pubB",
		"we_other":   "https://shop.example.com/stripe/hook",          // not ours
		"we_otherco": "https://old.example.com/webhooks/stripe/XpubA", // another company
	})
	st.SetSetting("base_url", "https://new.example.org")
	st.SetSetting("stripe_webhooks_refresh", "1")
	ctx := context.Background()

	// a copy that keeps the webhooks with the original server does nothing
	st.SetSetting("stripe_webhooks_hold", "1")
	e.app.RefreshWebhooksIfMoved(ctx)
	if len(f.eps) != 5 || f.n != 0 {
		t.Fatal("a held copy must not touch the webhooks")
	}
	st.SetSetting("stripe_webhooks_hold", "")

	// Stripe refuses to create: the old endpoints stay untouched
	f.failPost = true
	e.app.RefreshWebhooksIfMoved(ctx)
	if len(f.deleted) != 0 || st.Setting("stripe_webhooks_refresh") != "1" {
		t.Fatalf("nothing may be deleted before the new endpoint exists: %v", f.deleted)
	}
	if c, _ := st.Company(co.ID); c.StripeWebhookID != "we_oldA" {
		t.Fatalf("the old id must be kept: %s", c.StripeWebhookID)
	}

	// creation works, one deletion fails: retried on the next run
	f.failPost = false
	f.failDel["we_orphanA"] = true
	e.app.RefreshWebhooksIfMoved(ctx)
	if st.Setting("stripe_webhooks_refresh") != "1" {
		t.Fatal("an endpoint left behind must be retried")
	}
	f.failDel = map[string]bool{}
	e.app.RefreshWebhooksIfMoved(ctx)
	if st.Setting("stripe_webhooks_refresh") != "" {
		t.Fatal("the move should be complete")
	}
	if f.n != 2 {
		t.Fatalf("one new endpoint per company, created once: %d", f.n)
	}
	want := map[string]bool{"we_other": true, "we_otherco": true}
	for id, u := range f.eps {
		if strings.HasPrefix(id, "we_new") {
			if !strings.HasPrefix(u, "https://new.example.org/webhooks/stripe/pub") {
				t.Fatalf("new endpoint url: %s", u)
			}
			continue
		}
		if !want[id] {
			t.Fatalf("endpoint %s (%s) should have been removed", id, u)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("endpoints of others were removed: %v", want)
	}
	for _, c := range []int64{co.ID, arch.ID} {
		cc, _ := st.Company(c)
		if !strings.HasPrefix(cc.StripeWebhookID, "we_new") || f.eps[cc.StripeWebhookID] != "https://new.example.org/webhooks/stripe/"+cc.PublicID {
			t.Fatalf("company %d webhook: %s", c, cc.StripeWebhookID)
		}
		if s := e.app.StripeWebhookSecret(cc); !strings.HasPrefix(s, "whsec_") || s == "whsec_old" {
			t.Fatalf("company %d secret: %q", c, s)
		}
	}
}
