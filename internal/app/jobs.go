package app

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
	"github.com/flocom/invoicer/internal/stripe"
)

// RunScheduler runs the background jobs until ctx is cancelled.
func (a *App) RunScheduler(ctx context.Context) {
	type job struct {
		name  string
		every time.Duration
		delay time.Duration
		fn    func(context.Context)
		next  time.Time
	}
	jobs := []*job{
		{name: "recurring", every: 10 * time.Minute, delay: 20 * time.Second, fn: a.RunRecurring},
		{name: "stripe-reconcile", every: 10 * time.Minute, delay: 40 * time.Second, fn: a.ReconcileStripe},
		{name: "reminders", every: time.Hour, delay: 2 * time.Minute, fn: a.RunReminders},
		{name: "maintenance", every: 24 * time.Hour, delay: 5 * time.Minute, fn: a.Maintenance},
		{name: "updates", every: 6 * time.Hour, delay: 45 * time.Second, fn: func(ctx context.Context) {
			if a.Updater != nil && a.Updater.Status().Enabled {
				if err := a.Updater.Check(ctx, true); err != nil {
					slog.Warn("update check failed", "err", err)
				}
			}
		}},
	}
	for _, j := range jobs {
		j.next = time.Now().Add(j.delay)
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, j := range jobs {
				if now.Before(j.next) {
					continue
				}
				j.next = now.Add(j.every)
				func() {
					defer func() {
						if r := recover(); r != nil {
							slog.Error("job panicked", "job", j.name, "panic", r)
						}
					}()
					jctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
					defer cancel()
					j.fn(jctx)
				}()
			}
		}
	}
}

// RunRecurring generates the invoices of every schedule that is due. A
// schedule that fell behind (server down) catches up one period per run.
func (a *App) RunRecurring(ctx context.Context) {
	today := a.Today()
	for round := 0; round < 24; round++ {
		due, err := a.Store.DueRecurring(today)
		if err != nil {
			slog.Error("recurring: list", "err", err)
			return
		}
		if len(due) == 0 {
			return
		}
		for _, r := range due {
			if err := a.generateRecurring(ctx, r, today); err != nil {
				slog.Error("recurring: generate", "schedule", r.ID, "err", err)
				a.Store.SetRecurringError(r.ID, err.Error())
			}
		}
	}
}

func (a *App) generateRecurring(ctx context.Context, r *store.Recurring, today string) error {
	co, err := a.Store.Company(r.CompanyID)
	if err != nil {
		return err
	}
	cl, err := a.Store.Client(co.ID, r.ClientID)
	if err != nil {
		return err
	}
	if len(r.Lines) == 0 {
		return fmt.Errorf("schedule has no lines")
	}
	issue := r.NextRun
	if r.EndDate != "" && issue > r.EndDate {
		_, err := a.Store.AdvanceRecurring(r.ID, r.NextRun, r.NextRun, r.Remaining, false, "")
		return err
	}
	due, _ := time.Parse("2006-01-02", issue)
	lines := make([]store.Line, len(r.Lines))
	copy(lines, r.Lines)
	inv := &store.Invoice{CompanyID: co.ID, ClientID: cl.ID, Currency: r.Currency, Lang: cl.Lang, IssueDate: issue,
		DueDate: due.AddDate(0, 0, r.DueDays).Format("2006-01-02"), Notes: r.Notes, PublicToken: security.Token(24),
		RemindersEnabled: true, Lines: lines}
	remaining := r.Remaining
	if remaining > 0 {
		remaining--
	}
	next := r.Advance(r.NextRun)
	active := remaining != 0 && (r.EndDate == "" || next <= r.EndDate)
	if err := a.Store.GenerateFromRecurring(r, inv, next, remaining, active); err != nil {
		if err == store.ErrNotFound {
			return nil // already handled
		}
		return err
	}
	issued, err := a.Store.Issue(co.ID, inv.ID)
	if err != nil {
		return err
	}
	a.Store.Audit(0, co.ID, "", "recurring.generate", fmt.Sprintf("%s → %s", r.Name, issued.Number))
	if r.AutoSend {
		// only e-mail invoices generated for today, not catch-up backlog older than a week
		if issue >= addDays(today, -7) {
			if err := a.SendInvoiceEmail(ctx, co, issued, "invoice", "", 0, "recurring-"+issued.PublicToken); err != nil {
				a.Store.SetRecurringError(r.ID, "e-mail: "+err.Error())
			}
		}
	}
	return nil
}

func addDays(iso string, n int) string {
	d, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return d.AddDate(0, 0, n).Format("2006-01-02")
}

func daysBetween(from, to string) int {
	a, err1 := time.Parse("2006-01-02", from)
	b, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(b.Sub(a).Hours() / 24)
}

// RunReminders e-mails payment reminders according to each company's
// schedule (days relative to the due date). Only the most recent applicable
// step is sent, so enabling reminders never floods old clients.
func (a *App) RunReminders(ctx context.Context) {
	if a.Now().Hour() < 8 || a.Now().Hour() >= 20 {
		return // business hours in the configured timezone
	}
	today := a.Today()
	companies, err := a.Store.AllCompanies()
	if err != nil {
		return
	}
	for _, co := range companies {
		if !co.RemindersEnabled || !co.HasResend() {
			continue
		}
		offsets := co.ReminderOffsets()
		if len(offsets) == 0 {
			continue
		}
		invs, err := a.Store.OpenInvoices(co.ID)
		if err != nil {
			continue
		}
		for _, inv := range invs {
			if !inv.RemindersEnabled || inv.SentAt == 0 || inv.Due() <= 0 {
				continue
			}
			d := daysBetween(inv.DueDate, today) // negative before due date
			sent := inv.SentOffsets()
			best, found := 0, false
			for _, o := range offsets {
				if o <= d && !sent[o] && (!found || o > best) {
					best, found = o, true
				}
			}
			if !found {
				continue
			}
			for _, o := range offsets {
				if o <= d {
					sent[o] = true
				}
			}
			// skip steps that are long past (e.g. invoice sent late)
			if d-best > 3 {
				a.Store.SetRemindersSent(inv.ID, sent)
				continue
			}
			// never remind on the day the invoice was sent
			if time.Unix(inv.SentAt, 0).In(a.Location()).Format("2006-01-02") == today {
				continue
			}
			kind := "reminder_overdue"
			if best < 0 {
				kind = "reminder_before"
			} else if best == 0 {
				kind = "reminder_due"
			}
			err := a.SendInvoiceEmail(ctx, co, inv, kind, "", 0, "reminder-"+inv.PublicToken+"-"+strconv.Itoa(best))
			if err != nil {
				slog.Warn("reminder failed", "invoice", inv.ID, "err", err)
				continue
			}
			a.Store.SetRemindersSent(inv.ID, sent)
		}
	}
}

// ReconcileStripe polls recent Checkout Sessions so payments are detected
// even without webhooks (local installs, or a missed delivery).
func (a *App) ReconcileStripe(ctx context.Context) {
	sessions, err := a.Store.PendingStripeSessions()
	if err != nil {
		return
	}
	for _, ss := range sessions {
		inv, err := a.Store.InvoiceByID(ss.InvoiceID)
		if err != nil {
			continue
		}
		co, err := a.Store.Company(inv.CompanyID)
		if err != nil || !co.HasStripe() {
			continue
		}
		s, err := stripe.GetCheckout(ctx, a.StripeKey(co), ss.ID)
		if err != nil {
			continue
		}
		if err := a.ApplyStripeSession(ctx, co, s); err != nil {
			slog.Warn("stripe reconcile", "session", ss.ID, "err", err)
		}
	}
}

// Maintenance purges expired data and keeps 14 daily database backups.
func (a *App) Maintenance(ctx context.Context) {
	a.Store.PurgeExpired()
	if _, err := a.Store.Backup(a.Cfg.Path("backups"), "daily", 14); err != nil {
		slog.Error("daily backup failed", "err", err)
	}
}
