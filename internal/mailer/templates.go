package mailer

import (
	"bytes"
	"html/template"
	"regexp"
	"strings"

	"github.com/flocom/invoicer/internal/i18n"
)

// Content describes a transactional e-mail before rendering.
type Content struct {
	Lang        string
	Kind        string // invoice, reminder_before, reminder_due, reminder_overdue, receipt, test
	CompanyName string
	Accent      string
	ClientName  string
	Number      string
	Amount      string // formatted amount due (or paid, for receipts)
	DueDate     string // formatted
	DaysLate    int
	Link        string
	Message     string // optional custom message from the sender
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
{{if .Link}}<tr><td style="padding:24px 32px 8px 32px" align="center">
<a href="{{.Link}}" style="display:inline-block;background:{{.Accent}};color:#ffffff;text-decoration:none;font-weight:600;font-size:15px;padding:13px 26px;border-radius:8px">{{.Button}}</a>
</td></tr>{{end}}
<tr><td style="padding:24px 32px 32px 32px;font-size:13px;color:#6b7080;line-height:1.6">{{.Closing}}<br>{{.CompanyName}}</td></tr>
</table>
</td></tr></table></body></html>`))

type view struct {
	Content
	Subject, Heading, AmountLabel, DueLabel, Button, Closing string
	Paragraphs                                               []string
}

// Render returns subject, HTML and plain-text bodies.
func Render(c Content) (subject, html, text string) {
	l := i18n.Norm(c.Lang)
	c.Lang = l
	if !accentRe.MatchString(c.Accent) {
		c.Accent = "#4338ca"
	}
	t := func(k string, a ...any) string { return i18n.T(l, k, a...) }
	v := view{Content: c, AmountLabel: t("mail.amount_due"), DueLabel: t("mail.due_date"), Button: t("mail.view_pay"),
		Closing: t("mail.closing")}
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
	if c.Link != "" {
		tb.WriteString(v.Button + ": " + c.Link + "\n\n")
	}
	tb.WriteString(v.Closing + "\n" + c.CompanyName + "\n")
	return v.Subject, buf.String(), tb.String()
}
