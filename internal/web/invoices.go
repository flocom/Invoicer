package web

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/flocom/invoicer/internal/app"
	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/money"
	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
)

// ---------- dashboard ----------

func (s *Server) dashboard(c *Ctx) error {
	today := s.App.Today()
	st, err := s.Store.Stats(c.Company.ID, today)
	if err != nil {
		return err
	}
	overdue, _, _ := s.Store.Invoices(c.Company.ID, store.InvoiceFilter{Status: "overdue", Today: today, Limit: 8})
	recent, _, _ := s.Store.Invoices(c.Company.ID, store.InvoiceFilter{Today: today, Limit: 8})
	rec, _ := s.Store.RecurringList(c.Company.ID)
	var upcoming []*store.Recurring
	for _, r := range rec {
		if r.Active && len(upcoming) < 5 {
			upcoming = append(upcoming, r)
		}
	}
	setup := map[string]bool{
		"details": c.Company.Address != "" || c.Company.TaxID != "",
		"email":   c.Company.HasResend(),
		"payment": c.Company.HasStripe() || c.Company.HasBank(),
	}
	nClients := 0
	if cl, err := s.Store.Clients(c.Company.ID, false, ""); err == nil {
		nClients = len(cl)
	}
	setup["client"] = nClients > 0
	return s.render(c, 200, "dashboard", s.page(c, c.Company.Name, "dashboard", map[string]any{
		"Stats": st, "Overdue": overdue, "Recent": recent, "Upcoming": upcoming, "Setup": setup,
		// the checklist is for brand-new companies: hide it once invoices exist
		"SetupDone": (setup["details"] && setup["email"] && setup["payment"] && setup["client"]) || len(recent) > 0,
		"Chart":     chartBars(st, c.Company.DefaultCurrency, today),
	}))
}

type bar struct {
	Label  string
	Amount int64
	Pct    int
}

// chartBars builds the 12-month "paid" histogram for the default currency.
func chartBars(st *store.Stats, cur, today string) []bar {
	months := map[string]int64{}
	for _, m := range st.Monthly {
		if m.Currency == cur {
			months[m.Month] = m.Amount
		}
	}
	t, _ := time.Parse("2006-01-02", today)
	var out []bar
	var maxV int64
	for i := 0; i < 12; i++ {
		m := time.Date(t.Year(), time.Month(i+1), 1, 0, 0, 0, 0, time.UTC)
		k := m.Format("2006-01")
		out = append(out, bar{Label: k, Amount: months[k]})
		maxV = max(maxV, months[k])
	}
	for i := range out {
		if maxV > 0 {
			out[i].Pct = int(out[i].Amount * 100 / maxV)
		}
	}
	return out
}

// ---------- list ----------

func (s *Server) invoiceList(c *Ctx) error {
	q := c.R.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 1)
	f := store.InvoiceFilter{Status: q.Get("status"), Search: strings.TrimSpace(q.Get("q")), Today: s.App.Today(), Limit: 50, Offset: (page - 1) * 50}
	list, total, err := s.Store.Invoices(c.Company.ID, f)
	if err != nil {
		return err
	}
	return s.render(c, 200, "invoices", s.page(c, c.t("nav.invoices"), "invoices", map[string]any{
		"Invoices": list, "Status": f.Status, "Q": f.Search, "Page": page, "Total": total,
		"HasNext": page*50 < total, "Statuses": []string{"", "draft", "open", "overdue", "paid", "void"},
	}))
}

// ---------- form ----------

type invoiceFormData struct {
	Invoice   *store.Invoice
	Clients   []*store.Client
	IsNew     bool
	CanSend   bool
	DefaultBP int64
}

func (s *Server) invoiceForm(c *Ctx) error {
	clients, err := s.Store.Clients(c.Company.ID, false, "")
	if err != nil {
		return err
	}
	co := c.Company
	d := &invoiceFormData{Clients: clients, CanSend: co.HasResend(), DefaultBP: co.DefaultTaxBP}
	if id := c.id("id"); id != 0 {
		inv, err := s.Store.Invoice(co.ID, id)
		if err != nil {
			return err
		}
		if inv.Status != store.StatusDraft {
			c.bad("invoice.not_editable")
			return c.redirect(c.cpath("/invoices/%d", id))
		}
		d.Invoice = inv
	} else {
		today := s.App.Today()
		due, _ := time.Parse("2006-01-02", today)
		inv := &store.Invoice{Currency: co.DefaultCurrency, Lang: co.DefaultLang, IssueDate: today,
			DueDate: due.AddDate(0, 0, co.PaymentTermsDays).Format("2006-01-02"), Notes: co.DefaultNotes, RemindersEnabled: true,
			Lines: []store.Line{{Quantity: 1000, TaxBP: co.DefaultTaxBP}}}
		if cid, _ := strconv.ParseInt(c.R.URL.Query().Get("client"), 10, 64); cid != 0 {
			if cl, err := s.Store.Client(co.ID, cid); err == nil {
				inv.ClientID = cl.ID
				inv.Lang = cl.Lang
				if cl.Currency != "" {
					inv.Currency = cl.Currency
				}
			}
		}
		d.Invoice, d.IsNew = inv, true
	}
	title := c.t("invoice.new")
	if !d.IsNew {
		title = d.Invoice.Title()
	}
	return s.render(c, 200, "invoice_form", s.page(c, title, "invoices", d))
}

// parseLines reads the repeated line inputs of invoice and recurring forms.
func parseLines(c *Ctx) ([]store.Line, error) {
	f := c.R.PostForm
	desc, qty, price, tax := f["line_desc"], f["line_qty"], f["line_price"], f["line_tax"]
	if len(desc) > 200 || len(qty) != len(desc) || len(price) != len(desc) || len(tax) != len(desc) {
		return nil, errors.New("err.lines")
	}
	var out []store.Line
	for i := range desc {
		d := strings.TrimSpace(desc[i])
		if d == "" && strings.TrimSpace(price[i]) == "" {
			continue
		}
		q, err1 := money.ParseQuantity(qty[i])
		p, err2 := money.ParseAmount(price[i])
		t, err3 := money.ParseRate(tax[i])
		if err1 != nil || err2 != nil || err3 != nil || d == "" || q == 0 || t < 0 || t > 10000 ||
			q > 1e12 || q < -1e12 || p > 1e13 || p < -1e13 {
			return nil, errors.New("err.lines")
		}
		out = append(out, store.Line{Position: len(out), Description: clip(d, 2000), Quantity: q, UnitPrice: p, TaxBP: t})
	}
	if len(out) == 0 {
		return nil, errors.New("err.no_lines")
	}
	return out, nil
}

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func (s *Server) invoiceSave(c *Ctx) error {
	co := c.Company
	inv := &store.Invoice{CompanyID: co.ID, PublicToken: security.Token(24), RemindersEnabled: true}
	isNew := true
	if id := c.id("id"); id != 0 {
		var err error
		if inv, err = s.Store.Invoice(co.ID, id); err != nil {
			return err
		}
		if inv.Status != store.StatusDraft {
			return errForbidden
		}
		isNew = false
	}
	clientID, _ := strconv.ParseInt(c.form("client_id"), 10, 64)
	inv.ClientID = clientID
	inv.Currency = c.form("currency")
	inv.Lang = i18n.Norm(c.form("lang"))
	inv.IssueDate = c.form("issue_date")
	inv.DueDate = c.form("due_date")
	inv.Notes = clip(c.form("notes"), 4000)
	inv.RemindersEnabled = c.form("reminders") == "1"
	lines, lerr := parseLines(c)
	if lines != nil {
		inv.Lines = lines
	}
	fail := func(key string) error {
		clients, _ := s.Store.Clients(co.ID, false, "")
		d := &invoiceFormData{Invoice: inv, Clients: clients, IsNew: isNew, CanSend: co.HasResend(), DefaultBP: co.DefaultTaxBP}
		if len(inv.Lines) == 0 {
			inv.Lines = []store.Line{{Quantity: 1000, TaxBP: co.DefaultTaxBP}}
		}
		p := s.page(c, c.t("invoice.new"), "invoices", d)
		p.Error = c.t(key)
		return s.render(c, 400, "invoice_form", p)
	}
	if _, err := s.Store.Client(co.ID, clientID); err != nil {
		return fail("err.client_required")
	}
	if !money.ValidCurrency(inv.Currency) {
		return fail("err.currency")
	}
	if !validDate(inv.IssueDate) || !validDate(inv.DueDate) || inv.DueDate < inv.IssueDate {
		return fail("err.dates")
	}
	if lerr != nil {
		return fail(lerr.Error())
	}
	if err := s.Store.SaveDraft(inv); err != nil {
		return err
	}
	if isNew {
		c.audit("invoice.draft_created", itoa(inv.ID))
	}
	switch c.form("action") {
	case "issue":
		return s.doIssue(c, inv.ID)
	case "send":
		return s.doSend(c, inv.ID, "")
	}
	c.ok("invoice.saved")
	return c.redirect(c.cpath("/invoices/%d", inv.ID))
}

// ---------- view & actions ----------

func (s *Server) invoiceView(c *Ctx) error {
	inv, err := s.Store.Invoice(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	cl, _ := s.Store.Client(c.Company.ID, inv.ClientID)
	payments, _ := s.Store.Payments(inv.ID)
	emails, _ := s.Store.EmailsForInvoice(inv.ID)
	buyer := inv.Buyer()
	if inv.Status == store.StatusDraft && cl != nil {
		buyer = store.Party{Name: cl.Name, ContactName: cl.ContactName, Address: cl.Address, Email: cl.Email, TaxID: cl.TaxID}
	}
	return s.render(c, 200, "invoice_view", s.page(c, inv.Title(), "invoices", map[string]any{
		"Invoice": inv, "Client": cl, "Buyer": buyer, "Payments": payments, "Emails": emails,
		"TaxGroups": store.TaxGroups(inv.Lines), "PublicURL": s.App.PublicURL(inv), "CanSend": c.Company.HasResend(),
		"HasStripe": c.Company.HasStripe(),
	}))
}

func (s *Server) loadInvoice(c *Ctx) (*store.Invoice, error) {
	return s.Store.Invoice(c.Company.ID, c.id("id"))
}

func (s *Server) invoicePDF(c *Ctx) error {
	inv, err := s.loadInvoice(c)
	if err != nil {
		return err
	}
	return s.servePDF(c, c.Company, inv, c.R.URL.Query().Get("dl") == "1")
}

func (s *Server) servePDF(c *Ctx, co *store.Company, inv *store.Invoice, download bool) error {
	b, err := s.App.PDF(co, inv)
	if err != nil {
		return err
	}
	disp := "inline"
	if download {
		disp = "attachment"
	}
	c.W.Header().Set("Content-Type", "application/pdf")
	c.W.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disp, app.FileName(inv)))
	c.W.Header().Set("Cache-Control", "private, no-store")
	c.W.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	_, err = c.W.Write(b)
	return err
}

func (s *Server) doIssue(c *Ctx, id int64) error {
	inv, err := s.Store.Issue(c.Company.ID, id)
	if err != nil {
		c.flash("err", err.Error())
		return c.redirect(c.cpath("/invoices/%d", id))
	}
	c.audit("invoice.issue", inv.Number)
	c.ok("invoice.issued", inv.Number)
	return c.redirect(c.cpath("/invoices/%d", id))
}

func (s *Server) invoiceIssue(c *Ctx) error { return s.doIssue(c, c.id("id")) }

func (s *Server) doSend(c *Ctx, id int64, message string) error {
	inv, err := s.Store.Invoice(c.Company.ID, id)
	if err != nil {
		return err
	}
	if !s.limiter.allow("send:"+itoa(c.Company.ID), 30, 10*time.Minute) {
		c.bad("err.rate_limited")
		return c.redirect(c.cpath("/invoices/%d", id))
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 45*time.Second)
	defer cancel()
	if err := s.App.SendInvoiceEmail(ctx, c.Company, inv, "invoice", message, c.User.ID, ""); err != nil {
		if errors.Is(err, app.ErrNoEmail) {
			c.bad("err.no_email_config")
		} else if errors.Is(err, app.ErrNoRecipient) {
			c.bad("err.no_recipient")
		} else {
			c.flash("err", err.Error())
		}
		return c.redirect(c.cpath("/invoices/%d", id))
	}
	inv, _ = s.Store.Invoice(c.Company.ID, id)
	c.audit("invoice.sent", inv.Number)
	c.ok("invoice.sent", inv.Number)
	return c.redirect(c.cpath("/invoices/%d", id))
}

func (s *Server) invoiceSend(c *Ctx) error {
	return s.doSend(c, c.id("id"), clip(c.form("message"), 2000))
}

func (s *Server) invoiceRemind(c *Ctx) error {
	inv, err := s.loadInvoice(c)
	if err != nil {
		return err
	}
	kind := "reminder_overdue"
	today := s.App.Today()
	if inv.DueDate > today {
		kind = "reminder_before"
	} else if inv.DueDate == today {
		kind = "reminder_due"
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 45*time.Second)
	defer cancel()
	if err := s.App.SendInvoiceEmail(ctx, c.Company, inv, kind, clip(c.form("message"), 2000), c.User.ID, ""); err != nil {
		c.flash("err", err.Error())
	} else {
		c.audit("invoice.reminder", inv.Number)
		c.ok("invoice.reminder_sent")
	}
	return c.redirect(c.cpath("/invoices/%d", inv.ID))
}

func (s *Server) invoiceMarkSent(c *Ctx) error {
	inv, err := s.loadInvoice(c)
	if err != nil {
		return err
	}
	if inv.Status == store.StatusDraft {
		if inv, err = s.Store.Issue(c.Company.ID, inv.ID); err != nil {
			c.flash("err", err.Error())
			return c.redirect(c.cpath("/invoices/%d", c.id("id")))
		}
	}
	s.Store.MarkSent(inv.ID)
	c.audit("invoice.marked_sent", inv.Number)
	c.ok("flash.saved")
	return c.redirect(c.cpath("/invoices/%d", inv.ID))
}

func (s *Server) paymentAdd(c *Ctx) error {
	inv, err := s.loadInvoice(c)
	if err != nil {
		return err
	}
	amount, err := money.ParseAmount(c.form("amount"))
	if err != nil || amount <= 0 || amount > 1e13 {
		c.bad("err.amount")
		return c.redirect(c.cpath("/invoices/%d", inv.ID))
	}
	method := c.form("method")
	valid := false
	for _, m := range store.PaymentMethods {
		valid = valid || m == method
	}
	if !valid {
		method = "other"
	}
	date := c.form("paid_on")
	if !validDate(date) {
		date = s.App.Today()
	}
	became, err := s.Store.RecordPayment(inv.ID, store.Payment{Amount: amount, Method: method, Reference: clip(c.form("reference"), 200), PaidOn: date}, c.User.ID)
	if err != nil {
		c.flash("err", err.Error())
		return c.redirect(c.cpath("/invoices/%d", inv.ID))
	}
	c.audit("payment.recorded", fmt.Sprintf("%s %s %s", inv.Number, money.Format(amount, inv.Currency, "en"), method))
	if became && c.form("receipt") == "1" {
		if inv, err := s.Store.Invoice(c.Company.ID, inv.ID); err == nil {
			ctx, cancel := context.WithTimeout(c.R.Context(), 30*time.Second)
			s.App.SendReceipt(ctx, c.Company, inv)
			cancel()
		}
	}
	c.ok("invoice.payment_recorded")
	return c.redirect(c.cpath("/invoices/%d", inv.ID))
}

func (s *Server) paymentDelete(c *Ctx) error {
	inv, err := s.loadInvoice(c)
	if err != nil {
		return err
	}
	if err := s.Store.DeletePayment(inv.ID, c.id("pid")); err != nil {
		return err
	}
	c.audit("payment.deleted", inv.Number)
	c.ok("invoice.payment_deleted")
	return c.redirect(c.cpath("/invoices/%d", inv.ID))
}

func (s *Server) invoiceVoid(c *Ctx) error {
	inv, err := s.loadInvoice(c)
	if err != nil {
		return err
	}
	if err := s.Store.Void(c.Company.ID, inv.ID); err != nil {
		c.flash("err", err.Error())
	} else {
		c.audit("invoice.void", inv.Number)
		c.ok("invoice.voided")
	}
	return c.redirect(c.cpath("/invoices/%d", inv.ID))
}

func (s *Server) invoiceDelete(c *Ctx) error {
	if err := s.Store.DeleteDraft(c.Company.ID, c.id("id")); err != nil {
		c.flash("err", err.Error())
		return c.redirect(c.cpath("/invoices/%d", c.id("id")))
	}
	c.audit("invoice.draft_deleted", itoa(c.id("id")))
	c.ok("invoice.deleted")
	return c.redirect(c.cpath("/invoices"))
}

func (s *Server) invoiceDuplicate(c *Ctx) error {
	src, err := s.loadInvoice(c)
	if err != nil {
		return err
	}
	today := s.App.Today()
	due, _ := time.Parse("2006-01-02", today)
	inv := &store.Invoice{CompanyID: c.Company.ID, ClientID: src.ClientID, Currency: src.Currency, Lang: src.Lang, IssueDate: today,
		DueDate: due.AddDate(0, 0, c.Company.PaymentTermsDays).Format("2006-01-02"), Notes: src.Notes,
		PublicToken: security.Token(24), RemindersEnabled: true}
	for _, l := range src.Lines {
		l.ID = 0
		inv.Lines = append(inv.Lines, l)
	}
	if err := s.Store.SaveDraft(inv); err != nil {
		return err
	}
	c.ok("invoice.duplicated")
	return c.redirect(c.cpath("/invoices/%d/edit", inv.ID))
}

func (s *Server) invoiceToggleReminders(c *Ctx) error {
	inv, err := s.loadInvoice(c)
	if err != nil {
		return err
	}
	if err := s.Store.SetInvoiceReminders(c.Company.ID, inv.ID, !inv.RemindersEnabled); err != nil {
		return err
	}
	c.ok("flash.saved")
	return c.redirect(c.cpath("/invoices/%d", inv.ID))
}
