package web

import (
	"fmt"
	"net/mail"
	"strconv"
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

	// the same customer in the user's other companies, with their invoices
	related, _ := s.Store.RelatedClients(c.User, cl)
	type otherInvoice struct {
		CompanyID   int64
		CompanyName string
		Invoice     *store.Invoice
	}
	var others []otherInvoice
	in := map[int64]bool{c.Company.ID: true}
	for _, r := range related {
		in[r.CompanyID] = true
		list, _, _ := s.Store.Invoices(r.CompanyID, store.InvoiceFilter{ClientID: r.ClientID, Limit: 100, Today: s.App.Today()})
		for _, inv := range list {
			if inv.Status != store.StatusDraft {
				others = append(others, otherInvoice{r.CompanyID, r.CompanyName, inv})
			}
		}
	}
	var targets []*store.Company
	if cos, err := s.Store.CompaniesFor(c.User, false); err == nil {
		for _, co := range cos {
			if !in[co.ID] {
				targets = append(targets, co)
			}
		}
	}
	return s.render(c, 200, "client_view", s.page(c, cl.Name, "clients", map[string]any{"Client": cl, "Invoices": invs,
		"Related": related, "Others": others, "CopyTargets": targets}))
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

// readClientForm fills cl from the posted client fields.
func readClientForm(c *Ctx, cl *store.Client) {
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
}

// validateClient returns the message key of the first invalid field, or "".
func validateClient(cl *store.Client) string {
	if cl.Name == "" {
		return "err.name_required"
	}
	if !validEmails(cl.Email) || strings.ContainsAny(cl.Email, ",; ") {
		return "err.email_invalid"
	}
	if !validEmails(cl.CCEmails) {
		return "err.email_invalid"
	}
	return ""
}

// clientQuickCreate creates a client from the invoice and recurring editors
// without leaving the page.
func (s *Server) clientQuickCreate(c *Ctx) error {
	cl := &store.Client{CompanyID: c.Company.ID}
	readClientForm(c, cl)
	if key := validateClient(cl); key != "" {
		return c.json(400, map[string]string{"error": c.t(key)})
	}
	if err := s.Store.SaveClient(cl); err != nil {
		return err
	}
	c.audit("client.created", cl.Name)
	return c.json(200, map[string]any{"id": cl.ID, "name": cl.Name, "lang": cl.Lang, "currency": cl.Currency})
}

func (s *Server) clientSave(c *Ctx) error {
	cl := &store.Client{CompanyID: c.Company.ID}
	if id := c.id("id"); id != 0 {
		var err error
		if cl, err = s.Store.Client(c.Company.ID, id); err != nil {
			return err
		}
	}
	readClientForm(c, cl)
	cl.Archived = c.form("archived") == "1"
	next := safeNext(c.form("next"))
	if key := validateClient(cl); key != "" {
		p := s.page(c, c.t("client.new"), "clients", map[string]any{"Client": cl, "Next": next})
		p.Error = c.t(key)
		return s.render(c, 400, "client_form", p)
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

// clientCopy duplicates the client into another company the user can access.
func (s *Server) clientCopy(c *Ctx) error {
	cl, err := s.Store.Client(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	target, _ := strconv.ParseInt(c.form("company"), 10, 64)
	if target == c.Company.ID || !s.Store.CanAccessCompany(c.User, target) {
		return errForbidden
	}
	related, _ := s.Store.RelatedClients(c.User, cl)
	for _, r := range related {
		if r.CompanyID == target {
			c.ok("client.already_there", r.CompanyName)
			return c.redirect(fmt.Sprintf("/c/%d/clients/%d", r.CompanyID, r.ClientID))
		}
	}
	cp, err := s.Store.CopyClient(cl, target)
	if err != nil {
		return err
	}
	co, _ := s.Store.Company(target)
	c.audit("client.copied", fmt.Sprintf("%s → %s", cl.Name, co.Name))
	c.ok("client.copied", co.Name)
	return c.redirect(fmt.Sprintf("/c/%d/clients/%d", target, cp.ID))
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
