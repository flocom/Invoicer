package mailer

import (
	"bytes"
	"html/template"
	"regexp"
	"strings"

	"github.com/flocom/invoicer/internal/brand"
	"github.com/flocom/invoicer/internal/i18n"
)

// Content describes a transactional e-mail before rendering.
type Content struct {
	Lang        string
	Kind        string // invoice, reminder_before, reminder_due, reminder_overdue, receipt, charge_failed, card_update, test
	CompanyName string
	Accent      string
	ClientName  string
	Number      string
	Amount      string // formatted amount due (or paid, for receipts)
	DueDate     string // formatted
	DaysLate    int
	Link        string
	Link2       string // secondary link: pay online by card (Stripe Checkout)
	Card        string // saved card label (charge_failed, owner notifications)
	// owner notifications: ClientName is the payer, Message the failure reason
	ClientNotified bool
	Message        string // optional custom message from the sender
	Bank           [][2]string
}

var accentRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

var tpl = template.Must(template.New("mail").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>{{.Subject}}</title></head>
<body style="margin:0;padding:0;background:#f4f4f7;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#1f2330">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f4f4f7;padding:32px 12px"><tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px;background:#ffffff;border-radius:12px;overflow:hidden">
<tr><td style="background:{{.Accent}};height:6px;line-height:6px;font-size:0">&nbsp;</td></tr>
<tr><td style="padding:32px 32px 8px 32px">
<p style="margin:0 0 4px 0;font-size:13px;color:#6b7080;text-transform:uppercase;letter-spacing:.06em">{{.CompanyName}}</p>
<h1 style="margin:0 0 20px 0;font-size:22px;line-height:1.3">{{.Heading}}</h1>
{{range .Paragraphs}}<p style="margin:0 0 14px 0;font-size:15px;line-height:1.6">{{.}}</p>{{end}}
{{if .Message}}<div style="margin:18px 0;padding:14px 16px;background:#f6f6f9;border-radius:8px;font-size:15px;line-height:1.6;white-space:pre-line">{{.Message}}</div>{{end}}
</td></tr>
{{if .Amount}}<tr><td style="padding:0 32px">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="border:1px solid #e6e7ec;border-radius:10px">
<tr><td style="padding:16px 18px;font-size:14px;color:#6b7080">{{.AmountLabel}}</td><td align="right" style="padding:16px 18px;font-size:20px;font-weight:700">{{.Amount}}</td></tr>
{{if .DueDate}}<tr><td style="padding:0 18px 16px 18px;font-size:14px;color:#6b7080">{{.DueLabel}}</td><td align="right" style="padding:0 18px 16px 18px;font-size:14px">{{.DueDate}}</td></tr>{{end}}
</table></td></tr>{{end}}
{{if .Bank}}<tr><td style="padding:16px 32px 0 32px">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f6f6f9;border-radius:10px">
<tr><td colspan="2" style="padding:14px 18px 6px 18px;font-size:14px;font-weight:700">{{.BankTitle}}</td></tr>
{{range .Bank}}<tr><td style="padding:3px 18px;font-size:13px;color:#6b7080;white-space:nowrap;vertical-align:top">{{index . 0}}</td><td style="padding:3px 18px 3px 0;font-size:13px;font-family:Menlo,Consolas,monospace">{{index . 1}}</td></tr>{{end}}
<tr><td colspan="2" style="height:10px;line-height:10px;font-size:0">&nbsp;</td></tr>
</table></td></tr>{{end}}
{{if .Link}}<tr><td style="padding:24px 32px 8px 32px" align="center">
<a href="{{.Link}}" style="display:inline-block;background:{{.Accent}};color:#ffffff;text-decoration:none;font-weight:600;font-size:15px;padding:13px 26px;border-radius:8px">{{.Button}}</a>
{{if .Link2}}<p style="margin:16px 0 0 0;font-size:14px"><a href="{{.Link2}}" style="color:{{.Accent}};font-weight:600">{{.Button2}}</a></p>{{end}}
</td></tr>{{end}}
<tr><td style="padding:24px 32px 32px 32px;font-size:13px;color:#6b7080;line-height:1.6">{{.Closing}}<br>{{.CompanyName}}</td></tr>
</table>
</td></tr></table></body></html>`))

type view struct {
	Content
	Subject, Heading, AmountLabel, DueLabel, Button, Button2, Closing, BankTitle string
	Paragraphs                                                                   []string
}

// Render returns subject, HTML and plain-text bodies.
func Render(c Content) (subject, html, text string) {
	l := i18n.Norm(c.Lang)
	c.Lang = l
	if !accentRe.MatchString(c.Accent) {
		c.Accent = brand.Default
	}
	c.Accent = brand.Readable(c.Accent) // white button text stays legible
	t := func(k string, a ...any) string { return i18n.T(l, k, a...) }
	v := view{Content: c, AmountLabel: t("mail.amount_due"), DueLabel: t("mail.due_date"), Button: t("mail.view_pay"),
		Button2: t("mail.pay_by_card"), Closing: t("mail.closing"), BankTitle: t("pdf.bank_transfer")}
	greet := t("mail.hello")
	if c.ClientName != "" {
		greet = t("mail.hello_name", c.ClientName)
	}
	switch c.Kind {
	case "invoice":
		v.Subject = t("mail.invoice.subject", c.Number, c.CompanyName)
		v.Heading = t("mail.invoice.heading", c.Number)
		v.Paragraphs = []string{greet, t("mail.invoice.body", c.CompanyName)}
	case "reminder_before":
		v.Subject = t("mail.reminder_before.subject", c.Number)
		v.Heading = t("mail.reminder_before.heading")
		v.Paragraphs = []string{greet, t("mail.reminder_before.body", c.Number, c.DueDate)}
	case "reminder_due":
		v.Subject = t("mail.reminder_due.subject", c.Number)
		v.Heading = t("mail.reminder_due.heading")
		v.Paragraphs = []string{greet, t("mail.reminder_due.body", c.Number)}
	case "reminder_overdue":
		v.Subject = t("mail.reminder_overdue.subject", c.Number)
		v.Heading = t("mail.reminder_overdue.heading")
		v.Paragraphs = []string{greet, t("mail.reminder_overdue.body", c.Number, c.DaysLate), t("mail.reminder_overdue.body2")}
	case "receipt":
		v.Subject = t("mail.receipt.subject", c.Number)
		v.Heading = t("mail.receipt.heading")
		v.Paragraphs = []string{greet, t("mail.receipt.body", c.Number)}
		v.AmountLabel = t("mail.amount_paid")
		v.DueDate = ""
		v.Button = t("mail.view_invoice")
		v.Link2 = "" // already paid
	case "charge_failed":
		v.Subject = t("mail.charge_failed.subject", c.Number)
		v.Heading = t("mail.charge_failed.heading")
		v.Paragraphs = []string{greet, t("mail.charge_failed.body", c.Number, c.Card), t("mail.charge_failed.body2")}
		v.Button = t("mail.update_card")
		v.Button2 = t("mail.pay_other_card")
	case "owner_paid":
		v.Subject = t("mail.owner_paid.subject", c.Number, c.ClientName)
		v.Heading = t("mail.owner_paid.heading")
		v.Paragraphs = []string{t("mail.hello"), t("mail.owner_paid.body", c.ClientName, c.Number)}
		if c.Card != "" {
			v.Paragraphs[1] = t("mail.owner_paid.card", c.Card, c.Number, c.ClientName)
		}
		v.AmountLabel = t("mail.amount_paid")
		v.DueDate = ""
		v.Button = t("mail.view_invoice")
		v.Closing = t("mail.owner_closing")
	case "owner_charge_failed":
		v.Subject = t("mail.owner_failed.subject", c.Number, c.ClientName)
		v.Heading = t("mail.owner_failed.heading")
		v.Paragraphs = []string{t("mail.hello"), t("mail.owner_failed.body", c.Number, c.ClientName, c.Card)}
		if c.ClientNotified {
			v.Paragraphs = append(v.Paragraphs, t("mail.owner_failed.notified"))
		} else {
			v.Paragraphs = append(v.Paragraphs, t("mail.owner_failed.not_notified"))
		}
		v.DueDate = ""
		v.Button = t("mail.view_invoice")
		v.Closing = t("mail.owner_closing")
	case "card_update":
		v.Subject = t("mail.card_update.subject", c.CompanyName)
		v.Heading = t("mail.card_update.heading")
		v.Paragraphs = []string{greet, t("mail.card_update.body", c.CompanyName)}
		v.Button = t("mail.update_card")
	default: // test
		v.Subject = t("mail.test.subject", c.CompanyName)
		v.Heading = t("mail.test.heading")
		v.Paragraphs = []string{t("mail.test.body")}
	}
	var buf bytes.Buffer
	tpl.Execute(&buf, v)

	var tb strings.Builder
	tb.WriteString(v.Heading + "\n\n")
	for _, p := range v.Paragraphs {
		tb.WriteString(p + "\n\n")
	}
	if c.Message != "" {
		tb.WriteString(c.Message + "\n\n")
	}
	if c.Amount != "" {
		tb.WriteString(v.AmountLabel + ": " + c.Amount + "\n")
		if v.DueDate != "" {
			tb.WriteString(v.DueLabel + ": " + v.DueDate + "\n")
		}
		tb.WriteString("\n")
	}
	if len(c.Bank) > 0 {
		tb.WriteString(v.BankTitle + "\n")
		for _, b := range c.Bank {
			if b[0] != "" {
				tb.WriteString(b[0] + ": ")
			}
			tb.WriteString(b[1] + "\n")
		}
		tb.WriteString("\n")
	}
	if c.Link != "" {
		tb.WriteString(v.Button + ": " + c.Link + "\n\n")
	}
	if v.Link2 != "" {
		tb.WriteString(v.Button2 + ": " + v.Link2 + "\n\n")
	}
	tb.WriteString(v.Closing + "\n" + c.CompanyName + "\n")
	return v.Subject, buf.String(), tb.String()
}
