package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/mailer"
	"github.com/flocom/invoicer/internal/money"
	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
	"github.com/flocom/invoicer/internal/stripe"
)

// Saved cards: a client's card is saved on the company's Stripe account when
// they pay an invoice of an auto-charged recurring schedule, or through the
// public "update my card" page. It is then charged off-session, automatically
// for each new recurring invoice or manually from the client page.

// ErrChargeFailed wraps the reason a card charge did not go through.
var ErrChargeFailed = errors.New("card payment failed")

// EnsureCustomer returns the client's Stripe customer, creating it on first use.
func (a *App) EnsureCustomer(ctx context.Context, co *store.Company, cl *store.Client) (*store.StripeCustomer, error) {
	if sc, err := a.Store.StripeCustomer(co.ID, cl.ID); err == nil {
		return sc, nil
	}
	if !co.HasStripe() {
		return nil, errors.New("online payment is not available")
	}
	lk, _ := a.customerLocks.LoadOrStore(fmt.Sprintf("%d:%d", co.ID, cl.ID), &sync.Mutex{})
	lk.(*sync.Mutex).Lock()
	defer lk.(*sync.Mutex).Unlock()
	if sc, err := a.Store.StripeCustomer(co.ID, cl.ID); err == nil {
		return sc, nil
	}
	id, err := stripe.CreateCustomer(ctx, a.StripeKey(co), cl.Name, cl.Email,
		map[string]string{"invoicer_client": strconv.FormatInt(cl.ID, 10), "invoicer_company": co.PublicID})
	if err != nil {
		return nil, err
	}
	return a.Store.SaveStripeCustomer(store.StripeCustomer{CompanyID: co.ID, ClientID: cl.ID, CustomerID: id, CardToken: security.Token(24)})
}

// CardURL is the public page where the client saves or replaces their card.
func (a *App) CardURL(sc *store.StripeCustomer) string { return a.BaseURL() + "/card/" + sc.CardToken }

// CardSetupCheckout opens a Stripe Checkout Session that only saves a card.
func (a *App) CardSetupCheckout(ctx context.Context, co *store.Company, sc *store.StripeCustomer, lang string) (string, error) {
	if !co.HasStripe() {
		return "", errors.New("online payment is not available")
	}
	s, err := stripe.CreateSetupCheckout(ctx, a.StripeKey(co), stripe.SetupParams{
		Customer:   sc.CustomerID,
		SuccessURL: a.CardURL(sc) + "?session_id={CHECKOUT_SESSION_ID}",
		CancelURL:  a.CardURL(sc),
		Locale:     lang,
		Metadata:   map[string]string{"invoicer_client": strconv.FormatInt(sc.ClientID, 10), "invoicer_company": co.PublicID},
	})
	if err != nil {
		return "", err
	}
	if err := a.Store.SaveCardSetup(s.ID, co.ID, sc.ClientID, s.ExpiresAt); err != nil {
		return "", err
	}
	return s.URL, nil
}

// ApplySetupSession saves the card of a completed setup session. It is safe
// to call several times for the same session.
func (a *App) ApplySetupSession(ctx context.Context, co *store.Company, s *stripe.Session) error {
	setup, err := a.Store.CardSetup(s.ID)
	if err != nil || setup.CompanyID != co.ID {
		return nil // not one of ours
	}
	switch {
	case s.Status == "expired":
		a.Store.SetCardSetupStatus(s.ID, "expired")
		return nil
	case s.Status != "complete" || s.SetupIntent == "" || setup.Status == "complete":
		return nil
	}
	card, err := stripe.SetupIntentCard(ctx, a.StripeKey(co), s.SetupIntent)
	if err != nil {
		return err
	}
	if err := a.saveCard(co, setup.ClientID, card); err != nil {
		return err
	}
	a.Store.SetCardSetupStatus(s.ID, "complete")
	return nil
}

func (a *App) saveCard(co *store.Company, clientID int64, c *stripe.Card) error {
	sc := &store.SavedCard{CompanyID: co.ID, ClientID: clientID, PaymentMethod: c.ID, Brand: c.Card.Brand, Last4: c.Card.Last4,
		ExpMonth: c.Card.ExpMonth, ExpYear: c.Card.ExpYear}
	if err := a.Store.SaveCard(sc); err != nil {
		return err
	}
	a.Store.Audit(0, co.ID, "", "card.saved", fmt.Sprintf("client %d: %s", clientID, sc.Label()))
	return nil
}

// saveCheckoutCard keeps the card used to pay an invoice when Checkout saved
// it on the client's customer (auto-charged recurring invoices).
func (a *App) saveCheckoutCard(ctx context.Context, co *store.Company, inv *store.Invoice, s *stripe.Session) {
	if s.Customer == "" || s.PaymentIntent == "" {
		return
	}
	sc, err := a.Store.StripeCustomer(co.ID, inv.ClientID)
	if err != nil || sc.CustomerID != s.Customer {
		return
	}
	pi, err := stripe.GetPaymentIntent(ctx, a.StripeKey(co), s.PaymentIntent)
	if err != nil {
		slog.Warn("stripe: cannot read the card of a payment", "session", s.ID, "err", err)
		return
	}
	if c := pi.Card(); c != nil && c.Customer == sc.CustomerID {
		if err := a.saveCard(co, inv.ClientID, c); err != nil {
			slog.Warn("cannot save card", "err", err)
		}
	}
}

// ChargeInvoice charges the amount due of an open invoice on a saved card of
// its client. A declined card returns the attempt and an error wrapping
// ErrChargeFailed with Stripe's reason.
func (a *App) ChargeInvoice(ctx context.Context, co *store.Company, inv *store.Invoice, card *store.SavedCard, automatic bool, userID int64) (*store.CardCharge, error) {
	if !co.HasStripe() {
		return nil, errors.New("online payment is not available")
	}
	if card.CompanyID != co.ID || card.ClientID != inv.ClientID {
		return nil, errors.New("this card does not belong to the invoice's client")
	}
	sc, err := a.Store.StripeCustomer(co.ID, inv.ClientID)
	if err != nil {
		return nil, errors.New("the client has no Stripe customer")
	}
	// one payment at a time per invoice (also excludes a concurrent Checkout)
	lk, _ := a.checkoutLocks.LoadOrStore(inv.ID, &sync.Mutex{})
	lk.(*sync.Mutex).Lock()
	defer lk.(*sync.Mutex).Unlock()
	if inv, err = a.Store.Invoice(co.ID, inv.ID); err != nil {
		return nil, err
	}
	if inv.Status != store.StatusOpen || inv.Due() <= 0 {
		return nil, errors.New("this invoice has nothing left to pay")
	}
	ch := &store.CardCharge{CompanyID: co.ID, InvoiceID: inv.ID, CardID: card.ID, CardLabel: card.Label(), Amount: inv.Due(),
		Automatic: automatic, CreatedBy: userID}
	if err := a.Store.CreateCharge(ch); err != nil {
		return nil, err
	}
	lang := i18n.Norm(inv.Lang)
	pi, err := stripe.Charge(ctx, a.StripeKey(co), stripe.ChargeParams{
		Amount: inv.Due(), Currency: inv.Currency, Customer: sc.CustomerID, PaymentMethod: card.PaymentMethod,
		Description:    i18n.T(lang, "pdf.invoice") + " " + inv.Number,
		Metadata:       map[string]string{"invoicer_invoice": strconv.FormatInt(inv.ID, 10), "invoicer_company": co.PublicID, "invoice_number": inv.Number},
		IdempotencyKey: "invoicer-charge-" + co.PublicID + "-" + strconv.FormatInt(ch.ID, 10),
	})
	if err != nil {
		msg, piID := err.Error(), ""
		var se *stripe.Error
		if errors.As(err, &se) {
			msg = se.Message
			if se.PaymentIntent != nil {
				piID = se.PaymentIntent.ID
			}
		}
		a.Store.UpdateCharge(ch.ID, piID, "failed", msg)
		ch.Status, ch.Error = "failed", msg
		a.Store.Audit(userID, co.ID, "", "card.charge_failed", fmt.Sprintf("%s %s: %s", inv.Number, money.Format(ch.Amount, inv.Currency, "en"), msg))
		return ch, fmt.Errorf("%w: %s", ErrChargeFailed, msg)
	}
	return ch, a.applyCharge(ctx, co, inv, ch, pi)
}

// applyCharge records the outcome of a payment intent.
func (a *App) applyCharge(ctx context.Context, co *store.Company, inv *store.Invoice, ch *store.CardCharge, pi *stripe.PaymentIntent) error {
	switch pi.Status {
	case "succeeded":
		a.Store.UpdateCharge(ch.ID, pi.ID, "succeeded", "")
		ch.Status = "succeeded"
		return a.recordCardPayment(ctx, co, inv, pi)
	case "processing":
		a.Store.UpdateCharge(ch.ID, pi.ID, "processing", "")
		ch.Status = "processing"
		return nil
	default: // requires_action (3-D Secure), requires_payment_method, canceled
		msg := "the bank requires the cardholder to authenticate this payment"
		if pi.LastError != nil && pi.LastError.Message != "" {
			msg = pi.LastError.Message
		}
		a.Store.UpdateCharge(ch.ID, pi.ID, "failed", msg)
		ch.Status, ch.Error = "failed", msg
		return fmt.Errorf("%w: %s", ErrChargeFailed, msg)
	}
}

func (a *App) recordCardPayment(ctx context.Context, co *store.Company, inv *store.Invoice, pi *stripe.PaymentIntent) error {
	if !strings.EqualFold(pi.Currency, inv.Currency) {
		a.Store.Audit(0, co.ID, "", "payment.stripe_currency_mismatch", pi.ID)
		return nil
	}
	amount := pi.AmountReceived
	if amount == 0 {
		amount = pi.Amount
	}
	becamePaid, err := a.Store.RecordPayment(inv.ID, store.Payment{Amount: amount, Method: "stripe", Reference: pi.ID, PaidOn: a.Today(),
		StripeSessionID: pi.ID}, 0)
	if err != nil {
		return err
	}
	a.ExpireSessions(ctx, co, inv.ID, "")
	a.Store.Audit(0, co.ID, "", "payment.card", fmt.Sprintf("%s %s", inv.Number, money.Format(amount, inv.Currency, "en")))
	if becamePaid {
		if inv, err := a.Store.Invoice(co.ID, inv.ID); err == nil {
			a.SendReceipt(ctx, co, inv)
		}
	}
	return nil
}

// ReconcileCards follows up charges still processing and card setup
// sessions, so they complete even without webhooks.
func (a *App) ReconcileCards(ctx context.Context) {
	if charges, err := a.Store.ProcessingCharges(); err == nil {
		for _, ch := range charges {
			co, err := a.Store.Company(ch.CompanyID)
			if err != nil || !co.HasStripe() {
				continue
			}
			inv, err := a.Store.Invoice(co.ID, ch.InvoiceID)
			if err != nil {
				continue
			}
			pi, err := stripe.GetPaymentIntent(ctx, a.StripeKey(co), ch.PaymentIntent)
			if err != nil || pi.Status == "processing" {
				continue
			}
			if err := a.applyCharge(ctx, co, inv, ch, pi); errors.Is(err, ErrChargeFailed) && ch.Automatic {
				a.SendChargeFailedEmail(ctx, co, inv, ch)
			}
		}
	}
	if setups, err := a.Store.PendingCardSetups(); err == nil {
		for _, cs := range setups {
			co, err := a.Store.Company(cs.CompanyID)
			if err != nil || !co.HasStripe() {
				continue
			}
			if s, err := stripe.GetCheckout(ctx, a.StripeKey(co), cs.ID); err == nil {
				if err := a.ApplySetupSession(ctx, co, s); err != nil {
					slog.Warn("stripe card setup", "session", cs.ID, "err", err)
				}
			}
		}
	}
}

// autoCharge charges a freshly issued recurring invoice on the client's
// default card. It reports whether the client was taken care of (charged, or
// told by e-mail that the payment failed), so no invoice e-mail is needed.
func (a *App) autoCharge(ctx context.Context, co *store.Company, r *store.Recurring, inv *store.Invoice) bool {
	card, err := a.Store.DefaultCard(co.ID, inv.ClientID)
	if err != nil {
		return false // no card yet: the invoice e-mail lets the client pay and save one
	}
	ch, err := a.ChargeInvoice(ctx, co, inv, card, true, 0)
	if err == nil {
		return true
	}
	a.Store.SetRecurringError(r.ID, "card: "+strings.TrimPrefix(err.Error(), ErrChargeFailed.Error()+": "))
	if ch == nil {
		return false
	}
	return a.SendChargeFailedEmail(ctx, co, inv, ch) == nil
}

// SendChargeFailedEmail tells the client an automatic card payment failed,
// with the invoice attached, a link to pay it and a link to update the card.
func (a *App) SendChargeFailedEmail(ctx context.Context, co *store.Company, inv *store.Invoice, ch *store.CardCharge) error {
	if !co.HasResend() {
		return ErrNoEmail
	}
	cl, err := a.Store.Client(co.ID, inv.ClientID)
	if err != nil {
		return err
	}
	to := cl.Recipients()
	if len(to) == 0 {
		return ErrNoRecipient
	}
	sc, err := a.Store.StripeCustomer(co.ID, cl.ID)
	if err != nil {
		return err
	}
	lang := i18n.Norm(inv.Lang)
	c := mailer.Content{Lang: lang, Kind: "charge_failed", CompanyName: co.DisplayName(), Accent: co.AccentColor,
		ClientName: firstNonEmpty(cl.ContactName, cl.Name), Number: inv.Number, Amount: money.Format(inv.Due(), inv.Currency, lang),
		DueDate: i18n.Date(lang, inv.DueDate), Card: ch.CardLabel, Link: a.CardURL(sc)}
	if inv.CardPayment {
		c.Link2 = a.PublicURL(inv)
	}
	subject, html, text := mailer.Render(c)
	doc, err := a.PDF(co, inv)
	if err != nil {
		return err
	}
	id, err := mailer.Send(ctx, a.ResendKey(co), mailer.Message{From: co.EmailFrom, To: to, BCC: splitEmails(co.EmailBCC),
		ReplyTo: co.EmailReplyTo, Subject: subject, HTML: html, Text: text, IdempotencyKey: "charge-failed-" + strconv.FormatInt(ch.ID, 10),
		Attachments: []mailer.Attachment{{Filename: FileName(inv), Content: doc}}})
	log := store.EmailLog{CompanyID: co.ID, InvoiceID: inv.ID, Kind: "charge_failed", To: strings.Join(to, ", "), Subject: subject, Status: "sent", ProviderID: id}
	if err != nil {
		log.Status, log.Error = "failed", err.Error()
		a.Store.LogEmail(log)
		return err
	}
	a.Store.LogEmail(log)
	a.Store.MarkSent(inv.ID)
	return nil
}

// SendCardLinkEmail sends the client the link to save or replace their card.
func (a *App) SendCardLinkEmail(ctx context.Context, co *store.Company, cl *store.Client) error {
	if !co.HasResend() {
		return ErrNoEmail
	}
	to := cl.Recipients()
	if len(to) == 0 {
		return ErrNoRecipient
	}
	sc, err := a.EnsureCustomer(ctx, co, cl)
	if err != nil {
		return err
	}
	lang := i18n.Norm(cl.Lang)
	subject, html, text := mailer.Render(mailer.Content{Lang: lang, Kind: "card_update", CompanyName: co.DisplayName(), Accent: co.AccentColor,
		ClientName: firstNonEmpty(cl.ContactName, cl.Name), Link: a.CardURL(sc)})
	id, err := mailer.Send(ctx, a.ResendKey(co), mailer.Message{From: co.EmailFrom, To: to, BCC: splitEmails(co.EmailBCC),
		ReplyTo: co.EmailReplyTo, Subject: subject, HTML: html, Text: text,
		IdempotencyKey: "card-link-" + sc.CardToken[:12] + "-" + strconv.FormatInt(time.Now().Unix()/60, 10)})
	log := store.EmailLog{CompanyID: co.ID, Kind: "card_update", To: strings.Join(to, ", "), Subject: subject, Status: "sent", ProviderID: id}
	if err != nil {
		log.Status, log.Error = "failed", err.Error()
	}
	a.Store.LogEmail(log)
	return err
}
