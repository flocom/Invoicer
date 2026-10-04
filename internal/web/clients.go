package web

import (
	"net/mail"
	"strings"

	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/money"
	"github.com/flocom/invoicer/internal/store"
)

func (s *Server) clientList(c *Ctx) error {
	q := strings.TrimSpace(c.R.URL.Query().Get("q"))
	archived := c.R.URL.Query().Get("archived") == "1"
	list, err := s.Store.Clients(c.Company.ID, archived, q)
	if err != nil {
		return err
	}
	out, _ := s.Store.ClientOutstanding(c.Company.ID)
	for _, cl := range list {
		cl.Outstanding = out[cl.ID]
	}
	return s.render(c, 200, "clients", s.page(c, c.t("nav.clients"), "clients",
		map[string]any{"Clients": list, "Q": q, "Archived": archived}))
}

func (s *Server) clientView(c *Ctx) error {
	cl, err := s.Store.Client(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	invs, _, err := s.Store.Invoices(c.Company.ID, store.InvoiceFilter{ClientID: cl.ID, Limit: 200, Today: s.App.Today()})
	if err != nil {
		return err
	}
	out, _ := s.Store.ClientOutstanding(c.Company.ID)
	cl.Outstanding = out[cl.ID]
	return s.render(c, 200, "client_view", s.page(c, cl.Name, "clients", map[string]any{"Client": cl, "Invoices": invs}))
}

func (s *Server) clientForm(c *Ctx) error {
	cl := &store.Client{CompanyID: c.Company.ID, Lang: c.Company.DefaultLang}
	title := c.t("client.new")
	if id := c.id("id"); id != 0 {
		var err error
		if cl, err = s.Store.Client(c.Company.ID, id); err != nil {
			return err
		}
		title = cl.Name
	}
	return s.render(c, 200, "client_form", s.page(c, title, "clients", map[string]any{"Client": cl, "Next": safeNext(c.R.URL.Query().Get("next"))}))
}

func validEmails(list string) bool {
	for _, e := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
		if a, err := mail.ParseAddress(e); err != nil || a.Address != e {
			return false
		}
	}
	return true
}

func (s *Server) clientSave(c *Ctx) error {
	cl := &store.Client{CompanyID: c.Company.ID}
	if id := c.id("id"); id != 0 {
		var err error
		if cl, err = s.Store.Client(c.Company.ID, id); err != nil {
			return err
		}
	}
	cl.Name = clip(c.form("name"), 200)
	cl.ContactName = clip(c.form("contact_name"), 200)
	cl.Email = clip(c.form("email"), 200)
	cl.CCEmails = clip(c.form("cc_emails"), 600)
	cl.Address = clip(c.form("address"), 1000)
	cl.TaxID = clip(c.form("tax_id"), 60)
	cl.Lang = i18n.Norm(c.form("lang"))
	cl.Currency = c.form("currency")
	if !money.ValidCurrency(cl.Currency) {
		cl.Currency = ""
	}
	cl.Notes = clip(c.form("notes"), 2000)
	cl.Archived = c.form("archived") == "1"
	next := safeNext(c.form("next"))
	fail := func(key string) error {
		p := s.page(c, c.t("client.new"), "clients", map[string]any{"Client": cl, "Next": next})
		p.Error = c.t(key)
		return s.render(c, 400, "client_form", p)
	}
	if cl.Name == "" {
		return fail("err.name_required")
	}
	if !validEmails(cl.Email) || strings.ContainsAny(cl.Email, ",; ") {
		return fail("err.email_invalid")
	}
	if !validEmails(cl.CCEmails) {
		return fail("err.email_invalid")
	}
	isNew := cl.ID == 0
	if err := s.Store.SaveClient(cl); err != nil {
		return err
	}
	if isNew {
		c.audit("client.created", cl.Name)
	}
	c.ok("flash.saved")
	if next != "/" && strings.HasPrefix(next, c.cpath("/")) {
		sep := "?"
		if strings.Contains(next, "?") {
			sep = "&"
		}
		return c.redirect(next + sep + "client=" + itoa(cl.ID))
	}
	return c.redirect(c.cpath("/clients/%d", cl.ID))
}

func (s *Server) clientDelete(c *Ctx) error {
	ok, err := s.Store.DeleteClient(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	if !ok {
		c.bad("client.cannot_delete")
		return c.redirect(c.cpath("/clients/%d", c.id("id")))
	}
	c.audit("client.deleted", itoa(c.id("id")))
	c.ok("client.deleted")
	return c.redirect(c.cpath("/clients"))
}
