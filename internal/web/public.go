package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/store"
	"github.com/flocom/invoicer/internal/stripe"
)

// Public invoice pages are reachable with the unguessable 192-bit token that
// is sent to the client. They never expose anything beyond the invoice.

func (s *Server) publicLoad(c *Ctx) (*store.Invoice, *store.Company, error) {
	if !s.limiter.allow("public:"+c.RL, 60, time.Minute) {
		return nil, nil, store.ErrNotFound
	}
	inv, err := s.Store.InvoiceByToken(c.R.PathValue("token"))
	if err != nil {
		return nil, nil, err
	}
	co, err := s.Store.Company(inv.CompanyID)
	if err != nil {
		return nil, nil, err
	}
	return inv, co, nil
}

func (s *Server) publicInvoice(c *Ctx) error {
	inv, co, err := s.publicLoad(c)
	if err != nil {
		return err
	}
	// the page is shown in the client's language, not the visitor's account
	c.Lang = i18n.Norm(inv.Lang)
	c.User = nil
	c.Company = nil
	p := s.page(c, i18n.T(c.Lang, "pdf.invoice")+" "+inv.Number, "", map[string]any{
		"Invoice": inv, "Co": co, "Seller": inv.Seller(), "Buyer": inv.Buyer(), "TaxGroups": store.TaxGroups(inv.Lines),
		"CanPay": co.HasStripe() && inv.Status == store.StatusOpen && inv.Due() > 0,
		"Paid":   c.R.URL.Query().Get("paid") == "1", "Token": inv.PublicToken, "HasLogo": len(co.Logo) > 0,
	})
	p.Bare = true
	c.W.Header().Set("X-Robots-Tag", "noindex, nofollow")
	c.W.Header().Set("Referrer-Policy", "no-referrer")
	return s.render(c, 200, "public_invoice", p)
}

func (s *Server) publicPDF(c *Ctx) error {
	inv, co, err := s.publicLoad(c)
	if err != nil {
		return err
	}
	c.W.Header().Set("X-Robots-Tag", "noindex, nofollow")
	return s.servePDF(c, co, inv, c.R.URL.Query().Get("dl") == "1")
}

// publicPay creates (or reuses) a Stripe Checkout Session and redirects to it.
// Sessions are created on demand so a link in an old e-mail always works.
func (s *Server) publicPay(c *Ctx) error {
	inv, co, err := s.publicLoad(c)
	if err != nil {
		return err
	}
	if !s.limiter.allow("pay:"+inv.PublicToken, 10, 10*time.Minute) {
		s.renderError(c, http.StatusTooManyRequests, "err.rate_limited")
		return nil
	}
	if inv.Status != store.StatusOpen || inv.Due() <= 0 || !co.HasStripe() {
		http.Redirect(c.W, c.R, "/i/"+inv.PublicToken, http.StatusSeeOther)
		return nil
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 25*time.Second)
	defer cancel()
	url, err := s.App.Checkout(ctx, co, inv)
	if err != nil {
		slog.Error("stripe checkout", "invoice", inv.ID, "err", err)
		c.Lang = i18n.Norm(inv.Lang)
		s.renderError(c, http.StatusBadGateway, "err.checkout")
		return nil
	}
	http.Redirect(c.W, c.R, url, http.StatusSeeOther)
	return nil
}

// stripeWebhook receives Checkout events for one company. The signature is
// verified with that company's endpoint secret before anything is trusted;
// the session is then re-fetched from the Stripe API.
func (s *Server) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	co, err := s.Store.CompanyByPublicID(r.PathValue("pid"))
	if err != nil || !co.HasStripe() {
		http.Error(w, "unknown endpoint", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ev, err := stripe.VerifyWebhook(body, r.Header.Get("Stripe-Signature"), s.App.StripeWebhookSecret(co), time.Now())
	if err != nil {
		slog.Warn("stripe webhook rejected", "company", co.ID, "err", err)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}
	switch ev.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded", "checkout.session.expired":
		var obj struct {
			ID string `json:"id"`
		}
		json.Unmarshal(ev.Data.Object, &obj)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		sess, err := stripe.GetCheckout(ctx, s.App.StripeKey(co), obj.ID)
		if err != nil {
			slog.Error("stripe webhook fetch", "err", err)
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		if err := s.App.ApplyStripeSession(ctx, co, sess); err != nil {
			slog.Error("stripe webhook apply", "err", err)
			http.Error(w, "retry", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"received":true}`))
}
