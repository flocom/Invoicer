package web

import (
	"strconv"
)

const emailsPerPage = 50

// emailList shows every e-mail the company sent, newest first.
func (s *Server) emailList(c *Ctx) error {
	page, _ := strconv.Atoi(c.R.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	list, total, err := s.Store.CompanyEmails(c.Company.ID, emailsPerPage, (page-1)*emailsPerPage)
	if err != nil {
		return err
	}
	return s.render(c, 200, "emails", s.page(c, c.t("emails.title"), "invoices", map[string]any{
		"Emails": list, "Page": page, "HasNext": page*emailsPerPage < total, "Total": total,
	}))
}

// emailView shows one sent e-mail: details, body as received and text version.
func (s *Server) emailView(c *Ctx) error {
	e, err := s.Store.Email(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	return s.render(c, 200, "email_view", s.page(c, e.Subject, "invoices", map[string]any{"Email": e}))
}

// emailHTML serves the body of a sent e-mail for the preview frame of
// emailView. The page may only be framed by this site, and runs nothing.
func (s *Server) emailHTML(c *Ctx) error {
	e, err := s.Store.Email(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	h := c.W.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data: https:; frame-ancestors 'self'; sandbox")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	_, err = c.W.Write([]byte(e.HTML))
	return err
}

const stripeLogPerPage = 100

// stripeLogList shows what happened on Stripe: payment links, payments,
// card charges, saved cards and webhooks received.
func (s *Server) stripeLogList(c *Ctx) error {
	page, _ := strconv.Atoi(c.R.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	list, total, err := s.Store.CompanyStripeLog(c.Company.ID, stripeLogPerPage, (page-1)*stripeLogPerPage)
	if err != nil {
		return err
	}
	return s.render(c, 200, "stripe_log", s.page(c, c.t("stripelog.title"), "invoices", map[string]any{
		"Entries": list, "Page": page, "HasNext": page*stripeLogPerPage < total, "Total": total, "HasStripe": c.Company.HasStripe(),
	}))
}
