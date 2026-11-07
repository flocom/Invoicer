package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/money"
	"github.com/flocom/invoicer/internal/paypal"
	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
)

// ErrPayPalChanged is returned when an approved PayPal order no longer
// matches the invoice (paid or voided meanwhile): it is not captured.
var ErrPayPalChanged = errors.New("the invoice changed since this PayPal payment was started")

// PayPalCredentials returns the company's PayPal app credentials.
func (a *App) PayPalCredentials(co *store.Company) paypal.Credentials {
	secret, _ := a.Box.Open(co.PayPalSecret, aad(co, "paypal"))
	return paypal.Credentials{ClientID: co.PayPalClientID, Secret: secret, Sandbox: co.PayPalSandbox}
}

// ConfigurePayPal checks the credentials with PayPal, finds by itself
// whether they are live or sandbox ones, registers the webhook when this
// server is reachable over HTTPS, and stores everything. An empty secret
// keeps the saved one (to change the options only). It returns
// "no_public_https" when payments will be checked by polling only.
func (a *App) ConfigurePayPal(ctx context.Context, co *store.Company, clientID, secret string, card bool) (string, error) {
	clientID, secret = strings.TrimSpace(clientID), strings.TrimSpace(secret)
	if clientID == "" {
		return "", errors.New("the PayPal client ID is required")
	}
	if secret == "" {
		if !co.HasPayPal() || clientID != co.PayPalClientID {
			return "", errors.New("the PayPal secret is required")
		}
		secret = a.PayPalCredentials(co).Secret
	}
	creds, err := paypal.Detect(ctx, clientID, secret)
	if err != nil {
		return "", err
	}
	old := a.PayPalCredentials(co)
	same := co.HasPayPal() && old.ClientID == creds.ClientID && old.Sandbox == creds.Sandbox
	webhookID, warning := "", ""
	if same {
		webhookID = co.PayPalWebhookID
	} else if co.HasPayPal() && co.PayPalWebhookID != "" {
		_ = paypal.DeleteWebhook(ctx, old, co.PayPalWebhookID) // another app: remove our old endpoint
	}
	if webhookID == "" {
		if a.IsPublicHTTPS() {
			if webhookID, err = paypal.CreateWebhook(ctx, creds, a.payPalWebhookURL(co)); err != nil {
				warning = "webhook: " + err.Error()
				webhookID = ""
			}
		} else {
			warning = "no_public_https"
		}
	}
	if err := a.Store.UpdateCompanyPayPal(co.ID, creds.ClientID, a.Box.Seal(creds.Secret, aad(co, "paypal")), creds.Sandbox, card, webhookID); err != nil {
		return "", err
	}
	return warning, nil
}

func (a *App) payPalWebhookURL(co *store.Company) string {
	return a.BaseURL() + "/webhooks/paypal/" + co.PublicID
}

func (a *App) DisconnectPayPal(ctx context.Context, co *store.Company) error {
	if co.HasPayPal() && co.PayPalWebhookID != "" {
		_ = paypal.DeleteWebhook(ctx, a.PayPalCredentials(co), co.PayPalWebhookID)
	}
	return a.Store.UpdateCompanyPayPal(co.ID, "", nil, false, true, "")
}

// movePayPalWebhook points the company's PayPal webhook to the current
// address: the new endpoint first, then those pointing elsewhere removed.
func (a *App) movePayPalWebhook(ctx context.Context, co *store.Company) bool {
	creds := a.PayPalCredentials(co)
	target := a.payPalWebhookURL(co)
	hooks, listErr := paypal.ListWebhooks(ctx, creds)
	current := ""
	for _, h := range hooks {
		if h.URL == target {
			current = h.ID
		}
	}
	if current == "" {
		id, err := paypal.CreateWebhook(ctx, creds, target)
		if err != nil {
			slog.Warn("paypal: webhook not created", "company", co.ID, "err", err)
			return false
		}
		current = id
		a.Store.Audit(0, co.ID, "", "paypal.webhook_moved", target)
	}
	if current != co.PayPalWebhookID {
		if err := a.Store.SetPayPalWebhook(co.ID, current); err != nil {
			return false
		}
	}
	stale := map[string]bool{}
	if co.PayPalWebhookID != "" && co.PayPalWebhookID != current {
		stale[co.PayPalWebhookID] = true
	}
	for _, h := range hooks {
		if h.ID != current && strings.HasSuffix(h.URL, "/webhooks/paypal/"+co.PublicID) {
			stale[h.ID] = true
		}
	}
	ok := listErr == nil
	for id := range stale {
		if err := paypal.DeleteWebhook(ctx, creds, id); err != nil {
			slog.Warn("paypal: old webhook not removed", "company", co.ID, "webhook", id, "err", err)
			ok = false
		}
	}
	return ok
}

// HandlePayPalEvent applies a verified webhook event.
func (a *App) HandlePayPalEvent(ctx context.Context, co *store.Company, ev *paypal.Event) error {
	id := ev.OrderID()
	if id == "" {
		return nil
	}
	if _, err := a.Store.PayPalOrder(id); err != nil {
		return nil // not one of ours
	}
	_, err := a.ApplyPayPalOrder(ctx, co, id)
	if errors.Is(err, ErrPayPalChanged) || errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

var paypalLocale = map[string]string{"fr": "fr-FR", "en": "en-US"}

// PayPalCheckout creates a PayPal order for the amount due and returns the
// PayPal page where the client pays, with their PayPal account or (card) a
// bank card without account.
func (a *App) PayPalCheckout(ctx context.Context, co *store.Company, inv *store.Invoice, card bool) (string, error) {
	o, err := a.payPalOrder(ctx, co, inv, card, false)
	if err != nil {
		return "", err
	}
	return o.ApproveURL(), nil
}

// PayPalCardOrder creates the order paid with the PayPal card fields shown
// on the payment page itself, and returns its id.
func (a *App) PayPalCardOrder(ctx context.Context, co *store.Company, inv *store.Invoice) (string, error) {
	o, err := a.payPalOrder(ctx, co, inv, true, true)
	if err != nil {
		return "", err
	}
	return o.ID, nil
}

func (a *App) payPalOrder(ctx context.Context, co *store.Company, inv *store.Invoice, card, inline bool) (*paypal.Order, error) {
	if !inv.PayPalPayable(co) {
		return nil, errors.New("this invoice cannot be paid with PayPal")
	}
	lk, _ := a.checkoutLocks.LoadOrStore(inv.ID, &sync.Mutex{})
	lk.(*sync.Mutex).Lock()
	defer lk.(*sync.Mutex).Unlock()
	lang := i18n.Norm(inv.Lang)
	o, err := paypal.CreateOrder(ctx, a.PayPalCredentials(co), paypal.OrderParams{
		Amount: inv.Due(), Currency: inv.Currency, Description: i18n.T(lang, "pdf.invoice") + " " + inv.Number,
		CustomID: co.PublicID + ":" + strconv.FormatInt(inv.ID, 10), BrandName: co.DisplayName(), Locale: paypalLocale[lang],
		Card: card && co.PayPalCard, Inline: inline, ReturnURL: a.BaseURL() + "/paypal/" + inv.PublicToken + "/return", CancelURL: a.PublicURL(inv),
		RequestID: "invoicer-" + security.Token(12),
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.SavePayPalOrder(store.PayPalOrder{ID: o.ID, CompanyID: co.ID, InvoiceID: inv.ID, Amount: inv.Due(), Currency: inv.Currency}); err != nil {
		return nil, err
	}
	a.logStripe(co, inv, store.StripeLog{Event: "paypal.created", Ref: o.ID, Amount: inv.Due()})
	return o, nil
}

// ApplyPayPalOrder captures an order the client approved and records the
// payment. It is safe to call several times (return page, follow-up job).
// It reports whether the order is paid.
func (a *App) ApplyPayPalOrder(ctx context.Context, co *store.Company, orderID string) (bool, error) {
	known, err := a.Store.PayPalOrder(orderID)
	if err != nil || known.CompanyID != co.ID {
		return false, store.ErrNotFound
	}
	if known.Status == "complete" {
		return true, nil
	}
	inv, err := a.Store.Invoice(co.ID, known.InvoiceID)
	if err != nil {
		return false, err
	}
	creds := a.PayPalCredentials(co)
	o, err := paypal.GetOrder(ctx, creds, orderID)
	if err != nil {
		return false, err
	}
	switch o.Status {
	case "APPROVED":
		// only take money the invoice still asks for
		if inv.Status != store.StatusOpen || inv.Due() < known.Amount || !strings.EqualFold(inv.Currency, known.Currency) {
			a.Store.SetPayPalOrderStatus(orderID, "failed")
			a.logStripe(co, inv, store.StripeLog{Event: "paypal.stale", Level: "warn", Ref: orderID, Amount: known.Amount})
			return false, ErrPayPalChanged
		}
		if o, err = paypal.CaptureOrder(ctx, creds, orderID); err != nil {
			a.logStripe(co, inv, store.StripeLog{Event: "paypal.failed", Level: "error", Ref: orderID, Amount: known.Amount, Detail: err.Error()})
			return false, err
		}
	case "COMPLETED":
	case "VOIDED":
		a.Store.SetPayPalOrderStatus(orderID, "failed")
		return false, nil
	default: // not approved yet
		return false, nil
	}
	cp := o.Captured()
	if cp == nil {
		if o.Pending() {
			a.logStripe(co, inv, store.StripeLog{Event: "paypal.pending", Ref: orderID, Amount: known.Amount})
		}
		return false, nil
	}
	amount, err := paypal.ParseAmount(cp.Amount.Value)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(cp.Amount.CurrencyCode, inv.Currency) {
		a.Store.SetPayPalOrderStatus(orderID, "failed")
		a.logStripe(co, inv, store.StripeLog{Event: "payment.currency_mismatch", Level: "error", Ref: cp.ID, Amount: amount,
			Currency: strings.ToUpper(cp.Amount.CurrencyCode)})
		return false, nil
	}
	recorded, becamePaid, err := a.Store.RecordPaymentOnce(inv.ID, store.Payment{Amount: amount, Method: "paypal", Reference: cp.ID,
		PaidOn: a.Today(), StripeSessionID: "paypal:" + orderID}, 0)
	if err != nil {
		return false, err
	}
	a.Store.SetPayPalCapture(orderID, cp.ID)
	a.markPayPalDelivered(ctx, co, inv, orderID, cp.ID)
	if !recorded {
		return true, nil
	}
	a.logStripe(co, inv, store.StripeLog{Event: "paypal.received", Level: "ok", Ref: cp.ID, Amount: amount})
	a.Store.Audit(0, co.ID, "", "payment.paypal", fmt.Sprintf("%s %s", inv.Number, money.Format(amount, inv.Currency, "en")))
	if becamePaid {
		if inv, err := a.Store.Invoice(co.ID, inv.ID); err == nil {
			a.SendReceipt(ctx, co, inv)
		}
	}
	a.notifyOwner(ctx, co, inv, ownerNote{Kind: "owner_paid", Amount: amount, Via: "paypal", IdempotencyKey: "owner-paid-paypal-" + orderID})
	return true, nil
}

// markPayPalDelivered tells PayPal the order was delivered (no tracking
// number, buyer not notified); retried by the follow-up job if PayPal fails.
func (a *App) markPayPalDelivered(ctx context.Context, co *store.Company, inv *store.Invoice, orderID, captureID string) {
	if err := paypal.MarkDelivered(ctx, a.PayPalCredentials(co), captureID); err != nil {
		slog.Warn("paypal: not marked delivered", "order", orderID, "err", err)
		return
	}
	a.Store.SetPayPalTracked(orderID)
	a.logStripe(co, inv, store.StripeLog{Event: "paypal.delivered", Ref: captureID})
}

// ReconcilePayPal follows up orders whose client did not come back from
// PayPal after approving them, and paid orders not yet marked delivered.
func (a *App) ReconcilePayPal(ctx context.Context) {
	if paid, caps, err := a.Store.UntrackedPayPalCaptures(); err == nil {
		for _, o := range paid {
			co, err := a.Store.Company(o.CompanyID)
			if err != nil || !co.HasPayPal() {
				continue
			}
			inv, _ := a.Store.Invoice(co.ID, o.InvoiceID)
			a.markPayPalDelivered(ctx, co, inv, o.ID, caps[o.ID])
		}
	}
	orders, err := a.Store.OpenPayPalOrders()
	if err != nil {
		return
	}
	for _, o := range orders {
		co, err := a.Store.Company(o.CompanyID)
		if err != nil || !co.HasPayPal() {
			continue
		}
		if _, err := a.ApplyPayPalOrder(ctx, co, o.ID); err != nil && !errors.Is(err, ErrPayPalChanged) {
			slog.Warn("paypal follow-up", "order", o.ID, "err", err)
		}
	}
}
