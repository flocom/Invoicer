package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/mailer"
	"github.com/flocom/invoicer/internal/money"
	"github.com/flocom/invoicer/internal/store"
)

// OwnerAddress is where payment notifications go: the company's contact
// e-mail, or its reply-to address.
func OwnerAddress(co *store.Company) string {
	return firstNonEmpty(strings.TrimSpace(co.Email), strings.TrimSpace(co.EmailReplyTo))
}

// ownerNote describes a payment event for the company.
type ownerNote struct {
	Kind           string // owner_paid, owner_charge_failed
	Amount         int64
	Card           string // saved card label, for card charges
	Reason         string // why a charge failed
	ClientNotified bool   // the client was e-mailed about the failure
	IdempotencyKey string
}

// notifyOwner e-mails the company when a payment goes through or fails
// without anyone watching (online payment, automatic card charge).
func (a *App) notifyOwner(ctx context.Context, co *store.Company, inv *store.Invoice, n ownerNote) {
	to := OwnerAddress(co)
	if !co.HasResend() || to == "" {
		return
	}
	lang := i18n.Norm(co.DefaultLang)
	link := ""
	if a.BaseURL() != "" {
		link = fmt.Sprintf("%s/c/%d/invoices/%d", a.BaseURL(), co.ID, inv.ID)
	}
	subject, html, text := mailer.Render(mailer.Content{Lang: lang, Kind: n.Kind, CompanyName: co.DisplayName(), Accent: co.AccentColor,
		ClientName: inv.ClientName, Number: inv.Number, Amount: money.Format(n.Amount, inv.Currency, lang), Card: n.Card, Message: n.Reason,
		ClientNotified: n.ClientNotified, Link: link})
	id, err := mailer.Send(ctx, a.ResendKey(co), mailer.Message{From: co.EmailFrom, To: []string{to}, Subject: subject, HTML: html, Text: text,
		IdempotencyKey: n.IdempotencyKey})
	log := store.EmailLog{CompanyID: co.ID, InvoiceID: inv.ID, Kind: n.Kind, To: to, Subject: subject, Status: "sent", ProviderID: id, HTML: html, Text: text}
	if err != nil {
		log.Status, log.Error = "failed", err.Error()
		slog.Warn("owner notification", "invoice", inv.ID, "err", err)
	}
	a.Store.LogEmail(log)
}

// logStripe records what happened on Stripe for an invoice (nil for a
// client-level event such as a saved card).
func (a *App) logStripe(co *store.Company, inv *store.Invoice, l store.StripeLog) {
	l.CompanyID = co.ID
	if inv != nil {
		l.InvoiceID, l.ClientID, l.Number = inv.ID, inv.ClientID, inv.Number
		if l.Amount != 0 && l.Currency == "" {
			l.Currency = inv.Currency
		}
	}
	a.Store.LogStripe(l)
}
