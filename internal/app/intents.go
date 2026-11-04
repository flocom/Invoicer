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
	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
	"github.com/flocom/invoicer/internal/stripe"
)

// CardFormKey returns the publishable key when the payment page can show
// Stripe's card form itself ("" keeps the button to Stripe Checkout).
func CardFormKey(co *store.Company) string {
	if co.HasStripe() && stripe.ValidPublishableKey(co.StripePublishable) {
		return co.StripePublishable
	}
	return ""
}

// SavesCard tells whether paying this invoice online keeps the card.
func SavesCard(co *store.Company, inv *store.Invoice) bool { return inv.AutoCharge || co.SaveCards }

// StripeIntent returns the client secret of a payment for the amount due,
// confirmed by the card form of the payment page. An unconfirmed payment of
// the same amount is reused (a declined card can be retried).
func (a *App) StripeIntent(ctx context.Context, co *store.Company, inv *store.Invoice) (string, error) {
	if !inv.CardPayable(co) || CardFormKey(co) == "" {
		return "", errors.New("this invoice cannot be paid by card here")
	}
	lk, _ := a.checkoutLocks.LoadOrStore(inv.ID, &sync.Mutex{})
	lk.(*sync.Mutex).Lock()
	defer lk.(*sync.Mutex).Unlock()
	if inv, err := a.Store.Invoice(co.ID, inv.ID); err != nil || !inv.CardPayable(co) {
		return "", errors.New("this invoice has nothing left to pay")
	}
	key := a.StripeKey(co)
	save := SavesCard(co, inv)
	open, _ := a.Store.OpenStripeIntents(inv.ID)
	for _, it := range open {
		if it.Amount != inv.Due() || it.SavesCard != save {
			continue
		}
		if pi, err := stripe.GetPaymentIntent(ctx, key, it.ID); err == nil && pi.Status == "requires_payment_method" && pi.ClientSecret != "" {
			a.ExpireSessions(ctx, co, inv.ID, it.ID)
			return pi.ClientSecret, nil
		}
	}
	// anything else that could still be paid goes away first
	a.ExpireSessions(ctx, co, inv.ID, "")
	customer := ""
	if save {
		if cl, err := a.Store.Client(co.ID, inv.ClientID); err == nil {
			if sc, err := a.EnsureCustomer(ctx, co, cl); err == nil {
				customer = sc.CustomerID
			} else {
				slog.Warn("stripe customer", "client", cl.ID, "err", err)
			}
		}
	}
	lang := i18n.Norm(inv.Lang)
	pi, err := stripe.CreatePaymentIntent(ctx, key, stripe.IntentParams{
		Amount: inv.Due(), Currency: inv.Currency, Customer: customer,
		Description:    i18n.T(lang, "pdf.invoice") + " " + inv.Number + " — " + co.DisplayName(),
		Metadata:       map[string]string{"invoicer_invoice": strconv.FormatInt(inv.ID, 10), "invoicer_company": co.PublicID, "invoice_number": inv.Number},
		IdempotencyKey: "invoicer-pi-" + security.Token(12),
	})
	if err != nil {
		return "", err
	}
	if err := a.Store.SaveStripeIntent(store.StripeIntent{ID: pi.ID, CompanyID: co.ID, InvoiceID: inv.ID, Amount: inv.Due(),
		Currency: inv.Currency, SavesCard: customer != ""}); err != nil {
		return "", err
	}
	a.logStripe(co, inv, store.StripeLog{Event: "intent.created", Ref: pi.ID, Amount: inv.Due()})
	return pi.ClientSecret, nil
}

// ApplyStripeIntent records a payment made with the card form once Stripe
// says it went through. It is safe to call several times (return page,
// webhook, follow-up job) and reports whether the payment is done.
func (a *App) ApplyStripeIntent(ctx context.Context, co *store.Company, id string) (bool, error) {
	known, err := a.Store.StripeIntent(id)
	if err != nil || known.CompanyID != co.ID {
		return false, store.ErrNotFound
	}
	if known.Status == "complete" {
		return true, nil
	}
	pi, err := stripe.GetPaymentIntent(ctx, a.StripeKey(co), id)
	if err != nil {
		return false, err
	}
	inv, err := a.Store.Invoice(co.ID, known.InvoiceID)
	if err != nil {
		return false, err
	}
	switch pi.Status {
	case "succeeded":
	case "canceled":
		a.Store.SetStripeIntentStatus(id, "canceled")
		return false, nil
	default: // not confirmed, declined (can be retried), processing, 3-D Secure pending
		return false, nil
	}
	amount := pi.AmountReceived
	if amount == 0 {
		amount = pi.Amount
	}
	if !strings.EqualFold(pi.Currency, inv.Currency) {
		a.Store.SetStripeIntentStatus(id, "complete")
		a.logStripe(co, inv, store.StripeLog{Event: "payment.currency_mismatch", Level: "error", Ref: id, Amount: amount,
			Currency: strings.ToUpper(pi.Currency)})
		return true, nil
	}
	if inv.Status == store.StatusVoid {
		a.Store.SetStripeIntentStatus(id, "complete")
		a.logStripe(co, inv, store.StripeLog{Event: "payment.on_void", Level: "error", Ref: id, Amount: amount})
		return true, nil
	}
	recorded, becamePaid, err := a.Store.RecordPaymentOnce(inv.ID, store.Payment{Amount: amount, Method: "stripe", Reference: id,
		PaidOn: a.Today(), StripeSessionID: id}, 0)
	if err != nil {
		return false, err
	}
	a.Store.SetStripeIntentStatus(id, "complete")
	if !recorded {
		return true, nil
	}
	if known.SavesCard {
		if c := pi.Card(); c != nil && c.Customer != "" {
			if sc, err := a.Store.StripeCustomer(co.ID, inv.ClientID); err == nil && sc.CustomerID == c.Customer {
				if err := a.saveCard(co, inv.ClientID, c); err != nil {
					slog.Warn("cannot save card", "err", err)
				}
			}
		}
	}
	a.ExpireSessions(ctx, co, inv.ID, id)
	a.logStripe(co, inv, store.StripeLog{Event: "payment.received", Level: "ok", Ref: id, Amount: amount})
	a.Store.Audit(0, co.ID, "", "payment.stripe", fmt.Sprintf("%s %s", inv.Number, money.Format(amount, inv.Currency, "en")))
	if becamePaid {
		if inv, err := a.Store.Invoice(co.ID, inv.ID); err == nil {
			a.SendReceipt(ctx, co, inv)
		}
	}
	a.notifyOwner(ctx, co, inv, ownerNote{Kind: "owner_paid", Amount: amount, IdempotencyKey: "owner-paid-" + id})
	return true, nil
}

// reconcileIntents follows up card form payments whose client did not come
// back to the page (closed during 3-D Secure, network loss…).
func (a *App) reconcileIntents(ctx context.Context) {
	intents, err := a.Store.PendingStripeIntents()
	if err != nil {
		return
	}
	for _, it := range intents {
		co, err := a.Store.Company(it.CompanyID)
		if err != nil || !co.HasStripe() {
			continue
		}
		if _, err := a.ApplyStripeIntent(ctx, co, it.ID); err != nil {
			slog.Warn("stripe payment follow-up", "payment", it.ID, "err", err)
		}
	}
}

// UpdateStripeWebhookEvents subscribes the webhooks created by earlier
// versions to the events of the card form, once.
func (a *App) UpdateStripeWebhookEvents(ctx context.Context) {
	if a.Store.Setting("stripe_webhook_events") == "2" {
		return
	}
	cos, err := a.Store.StripeCompanies()
	if err != nil {
		return
	}
	for _, co := range cos {
		if co.StripeWebhookID == "" {
			continue
		}
		if err := stripe.UpdateWebhookEvents(ctx, a.StripeKey(co), co.StripeWebhookID); err != nil {
			slog.Warn("stripe: webhook events not updated", "company", co.ID, "err", err)
			return
		}
	}
	a.Store.SetSetting("stripe_webhook_events", "2")
}
