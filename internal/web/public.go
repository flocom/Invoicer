package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/flocom/invoicer/internal/app"
	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/paypal"
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
	d := map[string]any{
		"Invoice": inv, "Co": co, "Seller": inv.Seller(), "Buyer": inv.Buyer(), "TaxGroups": store.TaxGroups(inv.Lines),
		"CanPay": inv.CardPayable(co), "CanPayPal": inv.PayPalPayable(co),
		"PayPalCard":  inv.PayPalPayable(co) && co.PayPalCard && !inv.CardPayable(co),
		"PayPalIssue": map[string]string{"changed": "changed", "declined": "declined", "error": "error"}[c.R.URL.Query().Get("paypal")],
		"Bank":        s.Store.ResolveBank(co.ID, inv.BankAccountID, inv.Currency),
		"Paid":        c.R.URL.Query().Get("paid") == "1", "Token": inv.PublicToken, "HasLogo": len(co.Logo) > 0,
		"CardIssue": c.R.URL.Query().Get("card") == "failed",
	}
	// card forms shown on the page itself (otherwise the buttons open
	// Stripe Checkout or PayPal's page, as without JavaScript)
	var scripts []string
	if key := app.CardFormKey(co); key != "" && inv.CardPayable(co) {
		d["StripeKey"], d["SavesCard"] = key, app.SavesCard(co, inv)
		scripts = append(scripts, "https://js.stripe.com/v3/")
	}
	if inv.PayPalPayable(co) && co.PayPalCard && !inv.CardPayable(co) {
		d["PayPalFields"] = true
		scripts = append(scripts, "https://www.paypal.com/sdk/js?"+url.Values{"client-id": {co.PayPalClientID}, "components": {"card-fields"},
			"currency": {strings.ToUpper(inv.Currency)}, "intent": {"capture"}}.Encode())
	}
	d["AmountCents"], d["CurrencyLower"] = inv.Due(), strings.ToLower(inv.Currency)
	p := s.page(c, i18n.T(c.Lang, "pdf.invoice")+" "+inv.Number, "", d)
	p.Bare = true
	if len(scripts) > 0 {
		p.Scripts = append(scripts, "/static/pay.js?v="+s.assetVer)
		h := c.W.Header()
		h.Set("Content-Security-Policy", paymentCSP)
		h.Set("Permissions-Policy", `camera=(), microphone=(), geolocation=(), usb=(), payment=(self "https://js.stripe.com")`)
	}
	c.W.Header().Set("X-Robots-Tag", "noindex, nofollow")
	c.W.Header().Set("Referrer-Policy", "no-referrer")
	return s.render(c, 200, "public_invoice", p)
}

// paymentCSP lets the payment page load Stripe's and PayPal's card forms
// (their fields live in their own frames: card numbers never reach us).
const paymentCSP = "default-src 'self'; img-src 'self' data: https://*.stripe.com https://*.paypal.com https://*.paypalobjects.com; " +
	"style-src 'self' 'unsafe-inline'; " +
	"script-src 'self' https://js.stripe.com https://*.js.stripe.com https://www.paypal.com https://*.paypal.com https://*.paypalobjects.com; " +
	"frame-src https://js.stripe.com https://*.js.stripe.com https://hooks.stripe.com https://*.paypal.com; " +
	"connect-src 'self' https://api.stripe.com https://*.paypal.com https://*.paypalobjects.com; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// publicIntent starts the payment confirmed by the Stripe card form.
func (s *Server) publicIntent(c *Ctx) error {
	inv, co, err := s.publicLoad(c)
	if err != nil {
		return err
	}
	lang := i18n.Norm(inv.Lang)
	if !s.limiter.allow("pay:"+inv.PublicToken, 20, 10*time.Minute) {
		return c.json(http.StatusTooManyRequests, map[string]string{"error": i18n.T(lang, "err.rate_limited")})
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 25*time.Second)
	defer cancel()
	secret, err := s.App.StripeIntent(ctx, co, inv)
	if err != nil {
		slog.Error("stripe intent", "invoice", inv.ID, "err", err)
		return c.json(http.StatusBadGateway, map[string]string{"error": i18n.T(lang, "err.checkout")})
	}
	return c.json(http.StatusOK, map[string]string{"clientSecret": secret})
}

// publicIntentDone is where Stripe sends the client back after the card
// form (and 3-D Secure): the payment is recorded now.
func (s *Server) publicIntentDone(c *Ctx) error {
	inv, co, err := s.publicLoad(c)
	if err != nil {
		return err
	}
	back := "/i/" + inv.PublicToken
	id := c.R.URL.Query().Get("payment_intent")
	if it, err := s.Store.StripeIntent(id); err != nil || it.InvoiceID != inv.ID {
		http.Redirect(c.W, c.R, back, http.StatusSeeOther)
		return nil
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 30*time.Second)
	defer cancel()
	paid, err := s.App.ApplyStripeIntent(ctx, co, id)
	switch {
	case err != nil:
		slog.Error("stripe payment", "invoice", inv.ID, "payment", id, "err", err)
		back += "?paid=1" // checked again by the follow-up
	case paid || c.R.URL.Query().Get("redirect_status") != "failed":
		back += "?paid=1"
	default:
		back += "?card=failed"
	}
	http.Redirect(c.W, c.R, back, http.StatusSeeOther)
	return nil
}

// publicPayPalOrder creates the order paid with the PayPal card fields.
func (s *Server) publicPayPalOrder(c *Ctx) error {
	inv, co, err := s.publicLoad(c)
	if err != nil {
		return err
	}
	lang := i18n.Norm(inv.Lang)
	if !s.limiter.allow("pay:"+inv.PublicToken, 20, 10*time.Minute) {
		return c.json(http.StatusTooManyRequests, map[string]string{"error": i18n.T(lang, "err.rate_limited")})
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 25*time.Second)
	defer cancel()
	id, err := s.App.PayPalCardOrder(ctx, co, inv)
	if err != nil {
		slog.Error("paypal card order", "invoice", inv.ID, "err", err)
		return c.json(http.StatusBadGateway, map[string]string{"error": i18n.T(lang, "err.checkout")})
	}
	return c.json(http.StatusOK, map[string]string{"id": id})
}

// publicPayPalCapture takes the money once the card fields approved the order.
func (s *Server) publicPayPalCapture(c *Ctx) error {
	inv, co, err := s.publicLoad(c)
	if err != nil {
		return err
	}
	lang := i18n.Norm(inv.Lang)
	id := c.form("order")
	if o, err := s.Store.PayPalOrder(id); err != nil || o.InvoiceID != inv.ID {
		return c.json(http.StatusNotFound, map[string]string{"error": i18n.T(lang, "public.paypal_error")})
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 30*time.Second)
	defer cancel()
	paid, err := s.App.ApplyPayPalOrder(ctx, co, id)
	switch {
	case errors.Is(err, app.ErrPayPalChanged):
		return c.json(http.StatusConflict, map[string]string{"error": i18n.T(lang, "public.paypal_changed")})
	case paypal.HasIssue(err, "INSTRUMENT_DECLINED") || paypal.HasIssue(err, "PAYER_ACTION_REQUIRED"):
		return c.json(http.StatusPaymentRequired, map[string]string{"error": i18n.T(lang, "public.paypal_declined")})
	case err != nil:
		slog.Error("paypal card capture", "invoice", inv.ID, "order", id, "err", err)
		return c.json(http.StatusBadGateway, map[string]string{"error": i18n.T(lang, "public.paypal_error")})
	case !paid:
		return c.json(http.StatusPaymentRequired, map[string]string{"error": i18n.T(lang, "public.paypal_declined")})
	}
	return c.json(http.StatusOK, map[string]string{"redirect": "/i/" + inv.PublicToken + "?paid=1"})
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
	if !inv.CardPayable(co) {
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

// publicPayPal creates a PayPal order and sends the client to PayPal's page,
// on their PayPal account, or on the card form with ?card=1.
func (s *Server) publicPayPal(c *Ctx) error {
	inv, co, err := s.publicLoad(c)
	if err != nil {
		return err
	}
	if !s.limiter.allow("pay:"+inv.PublicToken, 10, 10*time.Minute) {
		s.renderError(c, http.StatusTooManyRequests, "err.rate_limited")
		return nil
	}
	if !inv.PayPalPayable(co) {
		http.Redirect(c.W, c.R, "/i/"+inv.PublicToken, http.StatusSeeOther)
		return nil
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 25*time.Second)
	defer cancel()
	url, err := s.App.PayPalCheckout(ctx, co, inv, c.R.URL.Query().Get("card") == "1")
	if err != nil {
		slog.Error("paypal checkout", "invoice", inv.ID, "err", err)
		c.Lang = i18n.Norm(inv.Lang)
		s.renderError(c, http.StatusBadGateway, "err.checkout")
		return nil
	}
	http.Redirect(c.W, c.R, url, http.StatusSeeOther)
	return nil
}

// publicPayPalReturn is where PayPal sends the client back after approving
// the payment: the order is captured now.
func (s *Server) publicPayPalReturn(c *Ctx) error {
	inv, co, err := s.publicLoad(c)
	if err != nil {
		return err
	}
	back := "/i/" + inv.PublicToken
	orderID := c.R.URL.Query().Get("token") // PayPal's name for the order id
	if known, err := s.Store.PayPalOrder(orderID); err != nil || known.InvoiceID != inv.ID {
		http.Redirect(c.W, c.R, back, http.StatusSeeOther)
		return nil
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 30*time.Second)
	defer cancel()
	paid, err := s.App.ApplyPayPalOrder(ctx, co, orderID)
	switch {
	case errors.Is(err, app.ErrPayPalChanged):
		back += "?paypal=changed"
	case paypal.HasIssue(err, "INSTRUMENT_DECLINED") || paypal.HasIssue(err, "PAYER_ACTION_REQUIRED"):
		back += "?paypal=declined"
	case err != nil:
		slog.Error("paypal capture", "invoice", inv.ID, "order", orderID, "err", err)
		back += "?paypal=error"
	case paid:
		back += "?paid=1"
	default:
		back += "?paid=1" // pending at PayPal: shown as being processed
	}
	http.Redirect(c.W, c.R, back, http.StatusSeeOther)
	return nil
}

// paypalWebhook receives PayPal events for one company. PayPal is asked to
// confirm each one comes from it before anything is done; the order is then
// read again from the PayPal API.
func (s *Server) paypalWebhook(w http.ResponseWriter, r *http.Request) {
	co, err := s.Store.CompanyByPublicID(r.PathValue("pid"))
	if err != nil || !co.HasPayPal() {
		http.Error(w, "unknown endpoint", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := paypal.VerifyWebhook(ctx, s.App.PayPalCredentials(co), co.PayPalWebhookID, r.Header, body); err != nil {
		slog.Warn("paypal webhook rejected", "company", co.ID, "err", err)
		http.Error(w, "not verified", http.StatusBadRequest)
		return
	}
	var ev paypal.Event
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	entry := store.StripeLog{CompanyID: co.ID, Event: "webhook", Ref: ev.ID, Detail: "PayPal " + ev.EventType}
	if o, err := s.Store.PayPalOrder(ev.OrderID()); err == nil && o.CompanyID == co.ID {
		entry.InvoiceID = o.InvoiceID
		if inv, err := s.Store.Invoice(co.ID, o.InvoiceID); err == nil {
			entry.ClientID, entry.Number = inv.ClientID, inv.Number
		}
	}
	s.Store.LogStripe(entry)
	if err := s.App.HandlePayPalEvent(ctx, co, &ev); err != nil {
		slog.Error("paypal webhook", "type", ev.EventType, "err", err)
		entry.Event, entry.Level, entry.Detail = "webhook.failed", "error", "PayPal "+ev.EventType+" — "+err.Error()
		s.Store.LogStripe(entry)
		http.Error(w, "retry", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
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
	var obj struct {
		ID string `json:"id"`
	}
	json.Unmarshal(ev.Data.Object, &obj)
	entry := store.StripeLog{CompanyID: co.ID, Event: "webhook", Ref: ev.ID, Detail: ev.Type}
	if ss, err := s.Store.StripeSession(obj.ID); err == nil {
		entry.InvoiceID = ss.InvoiceID
		if inv, err := s.Store.Invoice(co.ID, ss.InvoiceID); err == nil {
			entry.ClientID, entry.Number = inv.ClientID, inv.Number
		}
	} else if it, err := s.Store.StripeIntent(obj.ID); err == nil && it.CompanyID == co.ID {
		entry.InvoiceID = it.InvoiceID
		if inv, err := s.Store.Invoice(co.ID, it.InvoiceID); err == nil {
			entry.ClientID, entry.Number = inv.ClientID, inv.Number
		}
	} else if cs, err := s.Store.CardSetup(obj.ID); err == nil && cs.CompanyID == co.ID {
		entry.ClientID = cs.ClientID
	}
	s.Store.LogStripe(entry)
	switch ev.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded", "checkout.session.expired":
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		sess, err := stripe.GetCheckout(ctx, s.App.StripeKey(co), obj.ID)
		if err == nil {
			err = s.App.ApplyStripeSession(ctx, co, sess)
		}
		if err != nil {
			slog.Error("stripe webhook", "type", ev.Type, "err", err)
			entry.Event, entry.Level, entry.Detail = "webhook.failed", "error", ev.Type+" — "+err.Error()
			s.Store.LogStripe(entry)
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
	case "payment_intent.succeeded", "payment_intent.payment_failed":
		if entry.InvoiceID == 0 {
			break // an off-session charge, or not ours
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if _, err := s.App.ApplyStripeIntent(ctx, co, obj.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Error("stripe webhook", "type", ev.Type, "err", err)
			entry.Event, entry.Level, entry.Detail = "webhook.failed", "error", ev.Type+" — "+err.Error()
			s.Store.LogStripe(entry)
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"received":true}`))
}
