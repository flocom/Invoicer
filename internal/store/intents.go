package store

// StripeIntent is a payment started with the card form of the payment page.
type StripeIntent struct {
	ID        string
	CompanyID int64
	InvoiceID int64
	Amount    int64
	Currency  string
	SavesCard bool
	Status    string // open, complete, canceled
	CreatedAt int64
}

const intentCols = `id, company_id, invoice_id, amount, currency, saves_card, status, created_at`

func (s *Store) SaveStripeIntent(it StripeIntent) error {
	_, err := s.DB.Exec(`INSERT INTO stripe_intents(id, company_id, invoice_id, amount, currency, saves_card, status, created_at)
		VALUES(?,?,?,?,?,?,'open',?)`, it.ID, it.CompanyID, it.InvoiceID, it.Amount, it.Currency, b2i(it.SavesCard), now())
	return err
}

func (s *Store) StripeIntent(id string) (*StripeIntent, error) {
	it, err := s.intents(`SELECT `+intentCols+` FROM stripe_intents WHERE id = ?`, id)
	if err != nil || len(it) == 0 {
		return nil, ErrNotFound
	}
	return &it[0], nil
}

func (s *Store) SetStripeIntentStatus(id, status string) {
	s.DB.Exec(`UPDATE stripe_intents SET status = ? WHERE id = ?`, status, id)
}

// OpenStripeIntents lists the payments of an invoice that may still go through.
func (s *Store) OpenStripeIntents(invoiceID int64) ([]StripeIntent, error) {
	return s.intents(`SELECT `+intentCols+` FROM stripe_intents WHERE invoice_id = ? AND status = 'open'`, invoiceID)
}

// PendingStripeIntents lists recent payments whose outcome is not known yet
// (followed up when the client did not come back to the page).
func (s *Store) PendingStripeIntents() ([]StripeIntent, error) {
	return s.intents(`SELECT `+intentCols+` FROM stripe_intents WHERE status = 'open' AND created_at > ?`, now()-3*86400)
}

func (s *Store) intents(q string, args ...any) ([]StripeIntent, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StripeIntent
	for rows.Next() {
		var it StripeIntent
		if err := rows.Scan(&it.ID, &it.CompanyID, &it.InvoiceID, &it.Amount, &it.Currency, &it.SavesCard, &it.Status, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
