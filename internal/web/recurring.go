package web

import (
	"context"
	"strconv"
	"time"

	"github.com/flocom/invoicer/internal/money"
	"github.com/flocom/invoicer/internal/store"
)

func (s *Server) recurringList(c *Ctx) error {
	list, err := s.Store.RecurringList(c.Company.ID)
	if err != nil {
		return err
	}
	return s.render(c, 200, "recurring", s.page(c, c.t("nav.recurring"), "recurring", list))
}

type recurringFormData struct {
	Banks    []*store.BankAccount
	R        *store.Recurring
	Clients  []*store.Client
	IsNew    bool
	Invoices []*store.Invoice
	Preview  []string
	CanSend  bool
}

func (s *Server) recurringForm(c *Ctx) error {
	clients, err := s.Store.Clients(c.Company.ID, false, "")
	if err != nil {
		return err
	}
	co := c.Company
	d := &recurringFormData{Clients: clients, CanSend: co.HasResend()}
	d.Banks, _ = s.Store.BankAccounts(co.ID)
	if id := c.id("id"); id != 0 {
		r, err := s.Store.Recurring(co.ID, id)
		if err != nil {
			return err
		}
		d.R = r
		d.Invoices, _ = s.Store.RecurringInvoices(co.ID, r.ID)
	} else {
		today := s.App.Today()
		t, _ := time.Parse("2006-01-02", today)
		d.R = &store.Recurring{Currency: co.DefaultCurrency, IntervalUnit: "month", IntervalCount: 1, NextRun: today, AnchorDay: t.Day(),
			Remaining: -1, DueDays: co.PaymentTermsDays, AutoSend: co.HasResend(), Active: true, Notes: co.DefaultNotes, CardPayment: true,
			Lines: []store.Line{{Quantity: 1000, TaxBP: co.DefaultTaxBP}}}
		if cid, _ := strconv.ParseInt(c.R.URL.Query().Get("client"), 10, 64); cid != 0 {
			if cl, err := s.Store.Client(co.ID, cid); err == nil {
				d.R.ClientID = cl.ID
				if cl.Currency != "" {
					d.R.Currency = cl.Currency
				}
			}
		}
		d.IsNew = true
	}
	d.Preview = previewRuns(d.R, 4)
	title := c.t("recurring.new")
	if !d.IsNew {
		title = d.R.Name
	}
	return s.render(c, 200, "recurring_form", s.page(c, title, "recurring", d))
}

func addDays(iso string, n int) string {
	d, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return d.AddDate(0, 0, n).Format("2006-01-02")
}

func previewRuns(r *store.Recurring, n int) []string {
	var out []string
	d := r.NextRun
	left := r.Remaining
	for i := 0; i < n && d != ""; i++ {
		if (r.EndDate != "" && d > r.EndDate) || left == 0 {
			break
		}
		out = append(out, d)
		d = r.Advance(d)
		if left > 0 {
			left--
		}
	}
	return out
}

func (s *Server) recurringSave(c *Ctx) error {
	co := c.Company
	r := &store.Recurring{CompanyID: co.ID, Active: true}
	isNew := true
	if id := c.id("id"); id != 0 {
		var err error
		if r, err = s.Store.Recurring(co.ID, id); err != nil {
			return err
		}
		isNew = false
	}
	origNext := r.NextRun
	r.ClientID, _ = strconv.ParseInt(c.form("client_id"), 10, 64)
	r.Name = clip(c.form("name"), 200)
	r.Currency = c.form("currency")
	r.IntervalUnit = c.form("interval_unit")
	r.IntervalCount, _ = strconv.Atoi(c.form("interval_count"))
	r.NextRun = c.form("next_run")
	r.EndDate = c.form("end_date")
	r.DueDays, _ = strconv.Atoi(c.form("due_days"))
	r.AutoSend = c.form("auto_send") == "1"
	r.Notes = clip(c.form("notes"), 4000)
	r.BankAccountID = s.bankChoice(c)
	r.CardPayment = c.form("card_payment") == "1"
	r.AutoCharge = r.CardPayment && c.form("auto_charge") == "1"
	if v := c.form("remaining"); v == "" {
		r.Remaining = -1
	} else if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		r.Remaining = n
	}
	lines, lerr := parseLines(c)
	if lines != nil {
		r.Lines = lines
	}
	fail := func(key string) error {
		clients, _ := s.Store.Clients(co.ID, false, "")
		if len(r.Lines) == 0 {
			r.Lines = []store.Line{{Quantity: 1000, TaxBP: co.DefaultTaxBP}}
		}
		banks, _ := s.Store.BankAccounts(co.ID)
		p := s.page(c, c.t("recurring.new"), "recurring", &recurringFormData{R: r, Clients: clients, IsNew: isNew, CanSend: co.HasResend(), Banks: banks})
		p.Error = c.t(key)
		return s.render(c, 400, "recurring_form", p)
	}
	if _, err := s.Store.Client(co.ID, r.ClientID); err != nil {
		return fail("err.client_required")
	}
	if r.Name == "" {
		return fail("err.name_required")
	}
	if !money.ValidCurrency(r.Currency) {
		return fail("err.currency")
	}
	if r.IntervalUnit != "week" && r.IntervalUnit != "month" && r.IntervalUnit != "year" {
		return fail("err.interval")
	}
	if r.IntervalCount < 1 || r.IntervalCount > 36 {
		return fail("err.interval")
	}
	if !validDate(r.NextRun) || (r.EndDate != "" && !validDate(r.EndDate)) {
		return fail("err.dates")
	}
	// a start date far in the past would issue a burst of numbered invoices
	if r.NextRun != origNext && (r.NextRun < addDays(s.App.Today(), -31) || r.NextRun > addDays(s.App.Today(), 3660)) {
		return fail("err.recurring_start")
	}
	if r.DueDays < 0 || r.DueDays > 365 {
		return fail("err.dates")
	}
	if lerr != nil {
		return fail(lerr.Error())
	}
	t, _ := time.Parse("2006-01-02", r.NextRun)
	r.AnchorDay = t.Day()
	if r.Remaining == 0 {
		r.Active = false
	}
	if err := s.Store.SaveRecurring(r); err != nil {
		return err
	}
	if isNew {
		c.audit("recurring.created", r.Name)
	}
	c.ok("recurring.saved")
	// generate immediately if the first run is today or in the past
	if r.Active && r.NextRun <= s.App.Today() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			s.App.RunRecurring(ctx)
		}()
	}
	return c.redirect(c.cpath("/recurring/%d", r.ID))
}

func (s *Server) recurringToggle(c *Ctx) error {
	r, err := s.Store.Recurring(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	active := !r.Active
	if active && r.NextRun < s.App.Today() {
		// do not generate the whole backlog after a long pause
		next := r.NextRun
		for next < s.App.Today() {
			next = r.Advance(next)
		}
		r.NextRun = next
		r.Active = true
		if err := s.Store.SaveRecurring(r); err != nil {
			return err
		}
	} else if err := s.Store.SetRecurringActive(c.Company.ID, r.ID, active); err != nil {
		return err
	}
	c.audit("recurring.toggle", r.Name)
	c.ok("flash.saved")
	return c.redirect(c.cpath("/recurring/%d", r.ID))
}

func (s *Server) recurringDelete(c *Ctx) error {
	if err := s.Store.DeleteRecurring(c.Company.ID, c.id("id")); err != nil {
		return err
	}
	c.audit("recurring.deleted", itoa(c.id("id")))
	c.ok("recurring.deleted")
	return c.redirect(c.cpath("/recurring"))
}

// recurringRunNow generates the next invoice immediately by moving the next
// run date to today.
func (s *Server) recurringRunNow(c *Ctx) error {
	r, err := s.Store.Recurring(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	if !r.Active {
		c.bad("recurring.inactive")
		return c.redirect(c.cpath("/recurring/%d", r.ID))
	}
	today := s.App.Today()
	if r.NextRun > today {
		r.NextRun = today
		if err := s.Store.SaveRecurring(r); err != nil {
			return err
		}
	}
	s.App.RunRecurring(c.R.Context())
	c.audit("recurring.run_now", r.Name)
	c.ok("recurring.generated_now")
	return c.redirect(c.cpath("/recurring/%d", r.ID))
}
