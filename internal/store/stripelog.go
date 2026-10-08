package store

import "log/slog"

// StripeLog is one thing that happened on Stripe for a company: a payment
// link created or expired, a payment, a card charge, a saved card, a webhook.
type StripeLog struct {
	ID        int64
	CompanyID int64
	InvoiceID int64
	ClientID  int64
	Number    string // invoice number, kept when the invoice is deleted
	Event     string // e.g. checkout.created, payment.received, webhook
	Level     string // info, ok, warn, error
	Ref       string // Stripe object id (cs_…, pi_…, evt_…)
	Amount    int64
	Currency  string
	Detail    string
	CreatedAt int64
}

// LogStripe records an entry; a failure only shows in the server logs.
func (s *Store) LogStripe(l StripeLog) {
	if l.Level == "" {
		l.Level = "info"
	}
	if _, err := s.DB.Exec(`INSERT INTO stripe_log(company_id, invoice_id, client_id, number, event, level, ref, amount, currency, detail, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, l.CompanyID, l.InvoiceID, l.ClientID, l.Number, l.Event, l.Level, l.Ref, l.Amount, l.Currency,
		truncate(l.Detail, 1000), now()); err != nil {
		slog.Warn("stripe log", "err", err)
	}
}

const stripeLogCols = `id, company_id, invoice_id, client_id, number, event, level, ref, amount, currency, detail, created_at`

// InvoiceStripeLog lists the entries of an invoice, oldest first.
func (s *Store) InvoiceStripeLog(companyID, invoiceID int64) ([]StripeLog, error) {
	return s.stripeLog(`SELECT `+stripeLogCols+` FROM stripe_log WHERE company_id = ? AND invoice_id = ? ORDER BY id LIMIT 200`,
		companyID, invoiceID)
}

// CompanyStripeLog lists a page of the company's entries, newest first.
func (s *Store) CompanyStripeLog(companyID int64, limit, offset int) ([]StripeLog, int, error) {
	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM stripe_log WHERE company_id = ?`, companyID).Scan(&total); err != nil {
		return nil, 0, err
	}
	list, err := s.stripeLog(`SELECT `+stripeLogCols+` FROM stripe_log WHERE company_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`,
		companyID, limit, offset)
	return list, total, err
}

func (s *Store) stripeLog(q string, args ...any) ([]StripeLog, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StripeLog
	for rows.Next() {
		var l StripeLog
		if err := rows.Scan(&l.ID, &l.CompanyID, &l.InvoiceID, &l.ClientID, &l.Number, &l.Event, &l.Level, &l.Ref, &l.Amount, &l.Currency,
			&l.Detail, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s[:n])
	if len(r) > 0 && r[len(r)-1] == '�' {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}
