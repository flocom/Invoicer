package store

import (
	"context"
	"database/sql"
	"time"
)

type Recurring struct {
	ID            int64
	CompanyID     int64
	ClientID      int64
	Name          string
	Currency      string
	IntervalUnit  string // week, month, year
	IntervalCount int
	AnchorDay     int
	NextRun       string
	EndDate       string
	Remaining     int // -1 = unlimited
	DueDays       int
	AutoSend      bool
	Active        bool
	Notes         string
	LastRunAt     int64
	LastError     string
	BankAccountID int64
	CreatedAt     int64

	Lines      []Line
	ClientName string
}

// Amount returns the total (incl. tax) of one generated invoice.
func (r *Recurring) Amount() int64 {
	lines := append([]Line(nil), r.Lines...)
	_, _, t := ComputeTotals(lines)
	return t
}

// Advance computes the run date following `from`, keeping the anchor day for
// monthly/yearly schedules (a 31st becomes the last day of shorter months).
func (r *Recurring) Advance(from string) string {
	d, err := time.Parse("2006-01-02", from)
	if err != nil {
		return from
	}
	n := max(r.IntervalCount, 1)
	switch r.IntervalUnit {
	case "week":
		return d.AddDate(0, 0, 7*n).Format("2006-01-02")
	case "year":
		n *= 12
	}
	y, m := d.Year(), int(d.Month())-1+n
	y += m / 12
	m = m % 12
	first := time.Date(y, time.Month(m+1), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	day := r.AnchorDay
	if day < 1 {
		day = d.Day()
	}
	if day > last {
		day = last
	}
	return time.Date(y, time.Month(m+1), day, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

const recurringCols = `r.id, r.company_id, r.client_id, r.name, r.currency, r.interval_unit, r.interval_count, r.anchor_day, r.next_run,
	r.end_date, r.remaining, r.due_days, r.auto_send, r.active, r.notes, r.last_run_at, r.last_error, r.created_at, c.name, r.bank_account_id`

func scanRecurring(row interface{ Scan(...any) error }) (*Recurring, error) {
	r := &Recurring{}
	err := row.Scan(&r.ID, &r.CompanyID, &r.ClientID, &r.Name, &r.Currency, &r.IntervalUnit, &r.IntervalCount, &r.AnchorDay,
		&r.NextRun, &r.EndDate, &r.Remaining, &r.DueDays, &r.AutoSend, &r.Active, &r.Notes, &r.LastRunAt, &r.LastError, &r.CreatedAt, &r.ClientName, &r.BankAccountID)
	if err != nil {
		return nil, notFound(err)
	}
	return r, nil
}

func (s *Store) recurringLines(r *Recurring) error {
	rows, err := s.DB.Query(`SELECT id, position, description, quantity, unit_price, tax_bp FROM recurring_lines
		WHERE recurring_id = ? ORDER BY position`, r.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	r.Lines = nil
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.ID, &l.Position, &l.Description, &l.Quantity, &l.UnitPrice, &l.TaxBP); err != nil {
			return err
		}
		r.Lines = append(r.Lines, l)
	}
	return rows.Err()
}

func (s *Store) RecurringList(companyID int64) ([]*Recurring, error) {
	rows, err := s.DB.Query(`SELECT `+recurringCols+` FROM recurring r JOIN clients c ON c.id = r.client_id
		WHERE r.company_id = ? ORDER BY r.active DESC, r.next_run`, companyID)
	if err != nil {
		return nil, err
	}
	var out []*Recurring
	for rows.Next() {
		r, err := scanRecurring(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	for _, r := range out {
		if err := s.recurringLines(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) Recurring(companyID, id int64) (*Recurring, error) {
	r, err := scanRecurring(s.DB.QueryRow(`SELECT `+recurringCols+` FROM recurring r JOIN clients c ON c.id = r.client_id
		WHERE r.id = ? AND r.company_id = ?`, id, companyID))
	if err != nil {
		return nil, err
	}
	return r, s.recurringLines(r)
}

// DueRecurring returns active schedules whose next run is today or earlier.
func (s *Store) DueRecurring(today string) ([]*Recurring, error) {
	rows, err := s.DB.Query(`SELECT `+recurringCols+` FROM recurring r JOIN clients c ON c.id = r.client_id
		JOIN companies co ON co.id = r.company_id
		WHERE r.active = 1 AND r.next_run <= ? AND co.archived = 0`, today)
	if err != nil {
		return nil, err
	}
	var out []*Recurring
	for rows.Next() {
		r, err := scanRecurring(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	for _, r := range out {
		if err := s.recurringLines(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) SaveRecurring(r *Recurring) error {
	return withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		if r.ID == 0 {
			res, err := tx.Exec(`INSERT INTO recurring(company_id, client_id, name, currency, interval_unit, interval_count, anchor_day,
				next_run, end_date, remaining, due_days, auto_send, active, notes, bank_account_id, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				r.CompanyID, r.ClientID, r.Name, r.Currency, r.IntervalUnit, r.IntervalCount, r.AnchorDay, r.NextRun, r.EndDate,
				r.Remaining, r.DueDays, b2i(r.AutoSend), b2i(r.Active), r.Notes, r.BankAccountID, now())
			if err != nil {
				return err
			}
			r.ID, _ = res.LastInsertId()
		} else {
			_, err := tx.Exec(`UPDATE recurring SET client_id=?, name=?, currency=?, interval_unit=?, interval_count=?, anchor_day=?,
				next_run=?, end_date=?, remaining=?, due_days=?, auto_send=?, active=?, notes=?, bank_account_id=? WHERE id=? AND company_id=?`,
				r.ClientID, r.Name, r.Currency, r.IntervalUnit, r.IntervalCount, r.AnchorDay, r.NextRun, r.EndDate, r.Remaining,
				r.DueDays, b2i(r.AutoSend), b2i(r.Active), r.Notes, r.BankAccountID, r.ID, r.CompanyID)
			if err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM recurring_lines WHERE recurring_id = ?`, r.ID); err != nil {
			return err
		}
		for i, l := range r.Lines {
			if _, err := tx.Exec(`INSERT INTO recurring_lines(recurring_id, position, description, quantity, unit_price, tax_bp)
				VALUES(?,?,?,?,?,?)`, r.ID, i, l.Description, l.Quantity, l.UnitPrice, l.TaxBP); err != nil {
				return err
			}
		}
		return nil
	})
}

// AdvanceRecurring moves the schedule forward only if it still points at
// `expected` (protects against double generation).
func (s *Store) AdvanceRecurring(id int64, expected, next string, remaining int, active bool, lastErr string) (bool, error) {
	res, err := s.DB.Exec(`UPDATE recurring SET next_run = ?, remaining = ?, active = ?, last_run_at = ?, last_error = ?
		WHERE id = ? AND next_run = ?`, next, remaining, b2i(active), now(), lastErr, id, expected)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) SetRecurringError(id int64, msg string) {
	s.DB.Exec(`UPDATE recurring SET last_error = ? WHERE id = ?`, msg, id)
}

func (s *Store) SetRecurringActive(companyID, id int64, active bool) error {
	_, err := s.DB.Exec(`UPDATE recurring SET active = ? WHERE id = ? AND company_id = ?`, b2i(active), id, companyID)
	return err
}

func (s *Store) DeleteRecurring(companyID, id int64) error {
	_, err := s.DB.Exec(`DELETE FROM recurring WHERE id = ? AND company_id = ?`, id, companyID)
	return err
}

// GenerateFromRecurring atomically creates the draft invoice for the run
// `r.NextRun` and moves the schedule forward. It returns ErrNotFound if
// another worker already handled this run.
func (s *Store) GenerateFromRecurring(r *Recurring, inv *Invoice, next string, remaining int, active bool) error {
	inv.Subtotal, inv.TaxTotal, inv.Total = ComputeTotals(inv.Lines)
	return withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE recurring SET next_run = ?, remaining = ?, active = ?, last_run_at = ?, last_error = ''
			WHERE id = ? AND next_run = ? AND active = 1`, next, remaining, b2i(active), now(), r.ID, r.NextRun)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		t := now()
		ins, err := tx.Exec(`INSERT INTO invoices(company_id, client_id, status, currency, lang, issue_date, due_date, subtotal,
			tax_total, total, notes, public_token, recurring_id, reminders_enabled, bank_account_id, created_at, updated_at)
			VALUES(?,?,'draft',?,?,?,?,?,?,?,?,?,?,1,?,?,?)`, inv.CompanyID, inv.ClientID, inv.Currency, inv.Lang, inv.IssueDate,
			inv.DueDate, inv.Subtotal, inv.TaxTotal, inv.Total, inv.Notes, inv.PublicToken, r.ID, r.BankAccountID, t, t)
		if err != nil {
			return err
		}
		inv.ID, _ = ins.LastInsertId()
		return insertLines(tx, inv.ID, inv.Lines)
	})
}

func (s *Store) RecurringInvoices(companyID, recurringID int64) ([]*Invoice, error) {
	rows, err := s.DB.Query(`SELECT `+invoiceCols+` FROM invoices i JOIN clients c ON c.id = i.client_id
		WHERE i.company_id = ? AND i.recurring_id = ? ORDER BY i.id DESC LIMIT 24`, companyID, recurringID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Invoice
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}
