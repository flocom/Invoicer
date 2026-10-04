package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/flocom/invoicer/internal/money"
)

const (
	StatusDraft = "draft"
	StatusOpen  = "open"
	StatusPaid  = "paid"
	StatusVoid  = "void"
)

type Line struct {
	ID          int64
	Position    int
	Description string
	Quantity    int64 // thousandths
	UnitPrice   int64 // cents
	TaxBP       int64 // basis points
	Amount      int64 // cents, excl. tax
}

type TaxGroup struct {
	RateBP int64
	Base   int64
	Tax    int64
}

// Party is the frozen identity of the seller or the buyer at issue time.
type Party struct {
	Name           string `json:"name"`
	ContactName    string `json:"contact,omitempty"`
	Address        string `json:"address,omitempty"`
	Email          string `json:"email,omitempty"`
	Phone          string `json:"phone,omitempty"`
	Website        string `json:"website,omitempty"`
	TaxID          string `json:"tax_id,omitempty"`
	RegistrationID string `json:"registration_id,omitempty"`
}

type Invoice struct {
	ID               int64
	CompanyID        int64
	ClientID         int64
	Number           string
	Status           string
	Currency         string
	Lang             string
	IssueDate        string
	DueDate          string
	Subtotal         int64
	TaxTotal         int64
	Total            int64
	AmountPaid       int64
	Notes            string
	PublicToken      string
	ClientSnapshot   string
	CompanySnapshot  string
	RecurringID      int64
	RemindersEnabled bool
	RemindersSent    string
	BankAccountID    int64 // BankAuto, BankNone or an account id
	SentAt           int64
	PaidAt           int64
	VoidedAt         int64
	CreatedAt        int64
	UpdatedAt        int64

	Lines      []Line
	ClientName string
}

func (i *Invoice) Due() int64 { return i.Total - i.AmountPaid }

func (i *Invoice) IsOverdue(today string) bool {
	return i.Status == StatusOpen && i.DueDate < today
}

// DisplayStatus folds "overdue" into the status for the UI.
func (i *Invoice) DisplayStatus(today string) string {
	if i.IsOverdue(today) {
		return "overdue"
	}
	if i.Status == StatusOpen && i.AmountPaid > 0 {
		return "partial"
	}
	return i.Status
}

func (i *Invoice) Title() string {
	if i.Number != "" {
		return i.Number
	}
	return fmt.Sprintf("Draft #%d", i.ID)
}

func (i *Invoice) Buyer() Party {
	var p Party
	json.Unmarshal([]byte(i.ClientSnapshot), &p)
	return p
}

func (i *Invoice) Seller() Party {
	var p Party
	json.Unmarshal([]byte(i.CompanySnapshot), &p)
	return p
}

func (i *Invoice) SentOffsets() map[int]bool {
	out := map[int]bool{}
	for _, p := range strings.Split(i.RemindersSent, ",") {
		if n, err := strconv.Atoi(p); err == nil {
			out[n] = true
		}
	}
	return out
}

// TaxGroups sums bases and taxes per rate (taxes are rounded per rate, the
// usual European practice).
func TaxGroups(lines []Line) []TaxGroup {
	m := map[int64]*TaxGroup{}
	for _, l := range lines {
		g := m[l.TaxBP]
		if g == nil {
			g = &TaxGroup{RateBP: l.TaxBP}
			m[l.TaxBP] = g
		}
		g.Base += l.Amount
	}
	var out []TaxGroup
	for _, g := range m {
		g.Tax = money.Tax(g.Base, g.RateBP)
		out = append(out, *g)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].RateBP < out[b].RateBP })
	return out
}

// ComputeTotals fills line amounts and returns subtotal, tax and total.
func ComputeTotals(lines []Line) (sub, tax, total int64) {
	for i := range lines {
		lines[i].Amount = money.LineAmount(lines[i].Quantity, lines[i].UnitPrice)
		sub += lines[i].Amount
	}
	for _, g := range TaxGroups(lines) {
		tax += g.Tax
	}
	return sub, tax, sub + tax
}

const invoiceCols = `i.id, i.company_id, i.client_id, COALESCE(i.number, ''), i.status, i.currency, i.lang, i.issue_date, i.due_date,
	i.subtotal, i.tax_total, i.total, i.amount_paid, i.notes, i.public_token, i.client_snapshot, i.company_snapshot,
	i.recurring_id, i.reminders_enabled, i.reminders_sent, i.sent_at, i.paid_at, i.voided_at, i.created_at, i.updated_at, c.name,
	i.bank_account_id`

func scanInvoice(row interface{ Scan(...any) error }) (*Invoice, error) {
	i := &Invoice{}
	err := row.Scan(&i.ID, &i.CompanyID, &i.ClientID, &i.Number, &i.Status, &i.Currency, &i.Lang, &i.IssueDate, &i.DueDate,
		&i.Subtotal, &i.TaxTotal, &i.Total, &i.AmountPaid, &i.Notes, &i.PublicToken, &i.ClientSnapshot, &i.CompanySnapshot,
		&i.RecurringID, &i.RemindersEnabled, &i.RemindersSent, &i.SentAt, &i.PaidAt, &i.VoidedAt, &i.CreatedAt, &i.UpdatedAt, &i.ClientName,
		&i.BankAccountID)
	if err != nil {
		return nil, notFound(err)
	}
	return i, nil
}

func (s *Store) loadLines(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, inv *Invoice) error {
	rows, err := q.Query(`SELECT id, position, description, quantity, unit_price, tax_bp, amount FROM invoice_lines
		WHERE invoice_id = ? ORDER BY position`, inv.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	inv.Lines = nil
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.ID, &l.Position, &l.Description, &l.Quantity, &l.UnitPrice, &l.TaxBP, &l.Amount); err != nil {
			return err
		}
		inv.Lines = append(inv.Lines, l)
	}
	return rows.Err()
}

// Invoice loads an invoice scoped to its company, with lines.
func (s *Store) Invoice(companyID, id int64) (*Invoice, error) {
	inv, err := scanInvoice(s.DB.QueryRow(`SELECT `+invoiceCols+` FROM invoices i JOIN clients c ON c.id = i.client_id
		WHERE i.id = ? AND i.company_id = ?`, id, companyID))
	if err != nil {
		return nil, err
	}
	return inv, s.loadLines(s.DB, inv)
}

func (s *Store) InvoiceByToken(token string) (*Invoice, error) {
	if len(token) < 20 {
		return nil, ErrNotFound
	}
	inv, err := scanInvoice(s.DB.QueryRow(`SELECT `+invoiceCols+` FROM invoices i JOIN clients c ON c.id = i.client_id
		WHERE i.public_token = ? AND i.status != 'draft'`, token))
	if err != nil {
		return nil, err
	}
	return inv, s.loadLines(s.DB, inv)
}

type InvoiceFilter struct {
	Status   string // draft, open, paid, void, overdue, unpaid or ""
	ClientID int64
	Search   string
	Today    string
	Limit    int
	Offset   int
}

func (s *Store) Invoices(companyID int64, f InvoiceFilter) ([]*Invoice, int, error) {
	where := ` WHERE i.company_id = ?`
	args := []any{companyID}
	switch f.Status {
	case StatusDraft, StatusOpen, StatusPaid, StatusVoid:
		where += ` AND i.status = ?`
		args = append(args, f.Status)
	case "overdue":
		where += ` AND i.status = 'open' AND i.due_date < ?`
		args = append(args, f.Today)
	}
	if f.ClientID != 0 {
		where += ` AND i.client_id = ?`
		args = append(args, f.ClientID)
	}
	if f.Search != "" {
		like := "%" + escapeLike(f.Search) + "%"
		where += ` AND (i.number LIKE ? ESCAPE '\' OR c.name LIKE ? ESCAPE '\' OR i.notes LIKE ? ESCAPE '\')`
		args = append(args, like, like, like)
	}
	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM invoices i JOIN clients c ON c.id = i.client_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	q := `SELECT ` + invoiceCols + ` FROM invoices i JOIN clients c ON c.id = i.client_id` + where +
		` ORDER BY CASE i.status WHEN 'draft' THEN 0 ELSE 1 END, i.issue_date DESC, i.id DESC LIMIT ? OFFSET ?`
	rows, err := s.DB.Query(q, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Invoice
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, inv)
	}
	return out, total, rows.Err()
}

func insertLines(tx *sql.Tx, invoiceID int64, lines []Line) error {
	if _, err := tx.Exec(`DELETE FROM invoice_lines WHERE invoice_id = ?`, invoiceID); err != nil {
		return err
	}
	for i, l := range lines {
		if _, err := tx.Exec(`INSERT INTO invoice_lines(invoice_id, position, description, quantity, unit_price, tax_bp, amount)
			VALUES(?,?,?,?,?,?,?)`, invoiceID, i, l.Description, l.Quantity, l.UnitPrice, l.TaxBP, l.Amount); err != nil {
			return err
		}
	}
	return nil
}

// SaveDraft creates or updates a draft invoice and its lines.
func (s *Store) SaveDraft(inv *Invoice) error {
	inv.Subtotal, inv.TaxTotal, inv.Total = ComputeTotals(inv.Lines)
	return withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		t := now()
		if inv.ID == 0 {
			res, err := tx.Exec(`INSERT INTO invoices(company_id, client_id, status, currency, lang, issue_date, due_date, subtotal,
				tax_total, total, notes, public_token, recurring_id, reminders_enabled, bank_account_id, created_at, updated_at)
				VALUES(?,?,'draft',?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, inv.CompanyID, inv.ClientID, inv.Currency, inv.Lang, inv.IssueDate,
				inv.DueDate, inv.Subtotal, inv.TaxTotal, inv.Total, inv.Notes, inv.PublicToken, inv.RecurringID, b2i(inv.RemindersEnabled),
				inv.BankAccountID, t, t)
			if err != nil {
				return err
			}
			inv.ID, _ = res.LastInsertId()
			inv.Status = StatusDraft
		} else {
			res, err := tx.Exec(`UPDATE invoices SET client_id=?, currency=?, lang=?, issue_date=?, due_date=?, subtotal=?, tax_total=?,
				total=?, notes=?, reminders_enabled=?, bank_account_id=?, updated_at=? WHERE id=? AND company_id=? AND status='draft'`,
				inv.ClientID, inv.Currency, inv.Lang, inv.IssueDate, inv.DueDate, inv.Subtotal, inv.TaxTotal, inv.Total, inv.Notes,
				b2i(inv.RemindersEnabled), inv.BankAccountID, t, inv.ID, inv.CompanyID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("only drafts can be edited")
			}
		}
		return insertLines(tx, inv.ID, inv.Lines)
	})
}

// Issue finalises a draft: assigns the next sequential number for the year of
// the issue date and freezes buyer/seller identities.
func (s *Store) Issue(companyID, id int64) (*Invoice, error) {
	err := withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		var status, issueDate string
		var clientID int64
		if err := tx.QueryRow(`SELECT status, issue_date, client_id FROM invoices WHERE id = ? AND company_id = ?`, id, companyID).
			Scan(&status, &issueDate, &clientID); err != nil {
			return notFound(err)
		}
		if status != StatusDraft {
			return errors.New("invoice already issued")
		}
		var nLines int
		tx.QueryRow(`SELECT COUNT(*) FROM invoice_lines WHERE invoice_id = ?`, id).Scan(&nLines)
		if nLines == 0 {
			return errors.New("an invoice needs at least one line")
		}
		year, _ := strconv.Atoi(issueDate[:4])
		var prefix string
		var co Company
		var cl Client
		if err := tx.QueryRow(`SELECT invoice_prefix, name, legal_name, address, email, phone, website, tax_id, registration_id
			FROM companies WHERE id = ?`, companyID).Scan(&prefix, &co.Name, &co.LegalName, &co.Address, &co.Email, &co.Phone,
			&co.Website, &co.TaxID, &co.RegistrationID); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT name, contact_name, address, email, tax_id FROM clients WHERE id = ?`, clientID).
			Scan(&cl.Name, &cl.ContactName, &cl.Address, &cl.Email, &cl.TaxID); err != nil {
			return err
		}
		var seq int64
		if err := tx.QueryRow(`INSERT INTO number_counters(company_id, year, last) VALUES(?, ?, 1)
			ON CONFLICT(company_id, year) DO UPDATE SET last = last + 1 RETURNING last`, companyID, year).Scan(&seq); err != nil {
			return err
		}
		number := fmt.Sprintf("%s%d-%04d", prefix, year, seq)
		seller, _ := json.Marshal(Party{Name: co.DisplayName(), Address: co.Address, Email: co.Email, Phone: co.Phone,
			Website: co.Website, TaxID: co.TaxID, RegistrationID: co.RegistrationID})
		buyer, _ := json.Marshal(Party{Name: cl.Name, ContactName: cl.ContactName, Address: cl.Address, Email: cl.Email, TaxID: cl.TaxID})
		_, err := tx.Exec(`UPDATE invoices SET number = ?, status = 'open', company_snapshot = ?, client_snapshot = ?, updated_at = ?
			WHERE id = ?`, number, string(seller), string(buyer), now(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Invoice(companyID, id)
}

func (s *Store) MarkSent(id int64) {
	s.DB.Exec(`UPDATE invoices SET sent_at = ?, updated_at = ? WHERE id = ?`, now(), now(), id)
}

func (s *Store) SetRemindersSent(id int64, sent map[int]bool) {
	var parts []string
	for k := range sent {
		parts = append(parts, strconv.Itoa(k))
	}
	sort.Strings(parts)
	s.DB.Exec(`UPDATE invoices SET reminders_sent = ? WHERE id = ?`, strings.Join(parts, ","), id)
}

func (s *Store) SetInvoiceReminders(companyID, id int64, enabled bool) error {
	_, err := s.DB.Exec(`UPDATE invoices SET reminders_enabled = ? WHERE id = ? AND company_id = ?`, b2i(enabled), id, companyID)
	return err
}

// SetInvoiceBank changes the bank account shown on a draft or unpaid invoice
// (payment instructions, not part of the frozen legal content).
func (s *Store) SetInvoiceBank(companyID, id, choice int64) error {
	_, err := s.DB.Exec(`UPDATE invoices SET bank_account_id = ?, updated_at = ? WHERE id = ? AND company_id = ? AND status IN ('draft','open')`,
		choice, now(), id, companyID)
	return err
}

func (s *Store) Void(companyID, id int64) error {
	res, err := s.DB.Exec(`UPDATE invoices SET status = 'void', voided_at = ?, updated_at = ? WHERE id = ? AND company_id = ? AND status = 'open'`,
		now(), now(), id, companyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("only open invoices can be voided")
	}
	return nil
}

// DeleteInvoice removes an invoice of any status with its lines, payments and
// checkout sessions. Deleting issued invoices leaves a gap in the numbering;
// the web layer restricts it to administrators and warns about the law.
func (s *Store) DeleteInvoice(companyID, id int64) (*Invoice, error) {
	inv, err := s.Invoice(companyID, id)
	if err != nil {
		return nil, err
	}
	_, err = s.DB.Exec(`DELETE FROM invoices WHERE id = ? AND company_id = ?`, id, companyID)
	return inv, err
}

func (s *Store) DeleteDraft(companyID, id int64) error {
	res, err := s.DB.Exec(`DELETE FROM invoices WHERE id = ? AND company_id = ? AND status = 'draft'`, id, companyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("only drafts can be deleted")
	}
	return nil
}

// ---------- payments ----------

type Payment struct {
	ID              int64
	InvoiceID       int64
	Amount          int64
	Method          string
	Reference       string
	PaidOn          string
	StripeSessionID string
	CreatedAt       int64
}

var PaymentMethods = []string{"bank_transfer", "stripe", "card", "cash", "check", "other"}

// RecordPayment adds a payment and updates the invoice status. It returns
// (true, nil) when the invoice became fully paid with this payment. A
// duplicate Stripe session is silently ignored (idempotent webhooks).
func (s *Store) RecordPayment(invoiceID int64, p Payment, by int64) (bool, error) {
	becamePaid := false
	err := withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		var status string
		var total, paid int64
		if err := tx.QueryRow(`SELECT status, total, amount_paid FROM invoices WHERE id = ?`, invoiceID).Scan(&status, &total, &paid); err != nil {
			return notFound(err)
		}
		if status != StatusOpen && status != StatusPaid {
			return errors.New("payments can only be recorded on issued invoices")
		}
		var sid any
		if p.StripeSessionID != "" {
			sid = p.StripeSessionID
			var n int
			tx.QueryRow(`SELECT COUNT(*) FROM payments WHERE stripe_session_id = ?`, sid).Scan(&n)
			if n > 0 {
				return nil
			}
		}
		if _, err := tx.Exec(`INSERT INTO payments(invoice_id, amount, method, reference, paid_on, stripe_session_id, created_by, created_at)
			VALUES(?,?,?,?,?,?,?,?)`, invoiceID, p.Amount, p.Method, p.Reference, p.PaidOn, sid, by, now()); err != nil {
			return err
		}
		paid += p.Amount
		newStatus, paidAt := StatusOpen, int64(0)
		if paid >= total {
			newStatus, paidAt = StatusPaid, now()
			becamePaid = status != StatusPaid
		}
		_, err := tx.Exec(`UPDATE invoices SET amount_paid = ?, status = ?, paid_at = CASE WHEN ? = 'paid' THEN COALESCE(NULLIF(paid_at, 0), ?) ELSE 0 END,
			updated_at = ? WHERE id = ?`, paid, newStatus, newStatus, paidAt, now(), invoiceID)
		return err
	})
	return becamePaid, err
}

func (s *Store) DeletePayment(invoiceID, paymentID int64) error {
	return withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM payments WHERE id = ? AND invoice_id = ?`, paymentID, invoiceID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		_, err = tx.Exec(`UPDATE invoices SET amount_paid = (SELECT COALESCE(SUM(amount), 0) FROM payments WHERE invoice_id = ?),
			updated_at = ? WHERE id = ?`, invoiceID, now(), invoiceID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE invoices SET status = CASE WHEN amount_paid >= total THEN 'paid' ELSE 'open' END,
			paid_at = CASE WHEN amount_paid >= total THEN paid_at ELSE 0 END WHERE id = ? AND status IN ('open','paid')`, invoiceID)
		return err
	})
}

func (s *Store) Payments(invoiceID int64) ([]Payment, error) {
	rows, err := s.DB.Query(`SELECT id, invoice_id, amount, method, reference, paid_on, COALESCE(stripe_session_id, ''), created_at
		FROM payments WHERE invoice_id = ? ORDER BY paid_on, id`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		var p Payment
		if err := rows.Scan(&p.ID, &p.InvoiceID, &p.Amount, &p.Method, &p.Reference, &p.PaidOn, &p.StripeSessionID, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------- Stripe checkout sessions ----------

type StripeSession struct {
	ID        string
	InvoiceID int64
	Amount    int64
	URL       string
	Status    string
	CreatedAt int64
	ExpiresAt int64
}

func (s *Store) SaveStripeSession(ss StripeSession) error {
	_, err := s.DB.Exec(`INSERT INTO stripe_sessions(id, invoice_id, amount, url, status, created_at, expires_at) VALUES(?,?,?,?,?,?,?)`,
		ss.ID, ss.InvoiceID, ss.Amount, ss.URL, "open", now(), ss.ExpiresAt)
	return err
}

// ReusableStripeSession returns a still-valid open session for this amount.
func (s *Store) ReusableStripeSession(invoiceID, amount int64) (*StripeSession, error) {
	var ss StripeSession
	err := s.DB.QueryRow(`SELECT id, invoice_id, amount, url, status, created_at, expires_at FROM stripe_sessions
		WHERE invoice_id = ? AND amount = ? AND status = 'open' AND expires_at > ? ORDER BY created_at DESC LIMIT 1`,
		invoiceID, amount, now()+1800).Scan(&ss.ID, &ss.InvoiceID, &ss.Amount, &ss.URL, &ss.Status, &ss.CreatedAt, &ss.ExpiresAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &ss, nil
}

func (s *Store) StripeSession(id string) (*StripeSession, error) {
	var ss StripeSession
	err := s.DB.QueryRow(`SELECT id, invoice_id, amount, url, status, created_at, expires_at FROM stripe_sessions WHERE id = ?`, id).
		Scan(&ss.ID, &ss.InvoiceID, &ss.Amount, &ss.URL, &ss.Status, &ss.CreatedAt, &ss.ExpiresAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &ss, nil
}

func (s *Store) SetStripeSessionStatus(id, status string) {
	s.DB.Exec(`UPDATE stripe_sessions SET status = ? WHERE id = ?`, status, id)
}

// OpenStripeSessions lists the sessions of an invoice that may still be paid.
func (s *Store) OpenStripeSessions(invoiceID int64) ([]StripeSession, error) {
	rows, err := s.DB.Query(`SELECT id, invoice_id, amount, url, status, created_at, expires_at FROM stripe_sessions
		WHERE invoice_id = ? AND status = 'open' AND expires_at > ?`, invoiceID, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StripeSession
	for rows.Next() {
		var ss StripeSession
		if err := rows.Scan(&ss.ID, &ss.InvoiceID, &ss.Amount, &ss.URL, &ss.Status, &ss.CreatedAt, &ss.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// PendingStripeSessions lists recent open sessions so payments can be
// reconciled even if a webhook was missed.
func (s *Store) PendingStripeSessions() ([]StripeSession, error) {
	rows, err := s.DB.Query(`SELECT ss.id, ss.invoice_id, ss.amount, ss.url, ss.status, ss.created_at, ss.expires_at
		FROM stripe_sessions ss JOIN invoices i ON i.id = ss.invoice_id
		WHERE ss.status = 'open' AND ss.created_at > ? AND i.status = 'open'`, now()-3*86400)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StripeSession
	for rows.Next() {
		var ss StripeSession
		if err := rows.Scan(&ss.ID, &ss.InvoiceID, &ss.Amount, &ss.URL, &ss.Status, &ss.CreatedAt, &ss.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// InvoiceByID loads an invoice without company scoping (internal jobs only).
func (s *Store) InvoiceByID(id int64) (*Invoice, error) {
	inv, err := scanInvoice(s.DB.QueryRow(`SELECT `+invoiceCols+` FROM invoices i JOIN clients c ON c.id = i.client_id WHERE i.id = ?`, id))
	if err != nil {
		return nil, err
	}
	return inv, s.loadLines(s.DB, inv)
}

// OpenInvoices returns every open invoice of a company (reminder job).
func (s *Store) OpenInvoices(companyID int64) ([]*Invoice, error) {
	rows, err := s.DB.Query(`SELECT `+invoiceCols+` FROM invoices i JOIN clients c ON c.id = i.client_id
		WHERE i.company_id = ? AND i.status = 'open'`, companyID)
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

// ---------- dashboard ----------

type Stats struct {
	Outstanding map[string]int64
	Overdue     map[string]int64
	PaidMonth   map[string]int64
	PaidYear    map[string]int64
	Drafts      int
	OpenCount   int
	OverdueN    int
	Monthly     []MonthTotal
}

type MonthTotal struct {
	Month    string
	Currency string
	Amount   int64
}

func (s *Store) Stats(companyID int64, today string) (*Stats, error) {
	st := &Stats{Outstanding: map[string]int64{}, Overdue: map[string]int64{}, PaidMonth: map[string]int64{}, PaidYear: map[string]int64{}}
	rows, err := s.DB.Query(`SELECT currency, due_date < ?, SUM(total - amount_paid), COUNT(*) FROM invoices
		WHERE company_id = ? AND status = 'open' GROUP BY currency, due_date < ?`, today, companyID, today)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cur string
		var overdue bool
		var amt int64
		var n int
		rows.Scan(&cur, &overdue, &amt, &n)
		st.Outstanding[cur] += amt
		st.OpenCount += n
		if overdue {
			st.Overdue[cur] += amt
			st.OverdueN += n
		}
	}
	rows.Close()
	s.DB.QueryRow(`SELECT COUNT(*) FROM invoices WHERE company_id = ? AND status = 'draft'`, companyID).Scan(&st.Drafts)

	rows, err = s.DB.Query(`SELECT i.currency, substr(p.paid_on, 1, 7), SUM(p.amount) FROM payments p JOIN invoices i ON i.id = p.invoice_id
		WHERE i.company_id = ? AND p.paid_on >= ? GROUP BY i.currency, substr(p.paid_on, 1, 7) ORDER BY 2`, companyID, today[:4]+"-01-01")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m MonthTotal
		rows.Scan(&m.Currency, &m.Month, &m.Amount)
		st.Monthly = append(st.Monthly, m)
		st.PaidYear[m.Currency] += m.Amount
		if m.Month == today[:7] {
			st.PaidMonth[m.Currency] += m.Amount
		}
	}
	rows.Close()
	return st, nil
}
