package store

// PayPalOrder is an order created for an invoice's PayPal payment page.
type PayPalOrder struct {
	ID        string
	CompanyID int64
	InvoiceID int64
	Amount    int64
	Currency  string
	Status    string // created, complete, failed
	CreatedAt int64
}

func (s *Store) SavePayPalOrder(o PayPalOrder) error {
	_, err := s.DB.Exec(`INSERT INTO paypal_orders(id, company_id, invoice_id, amount, currency, status, created_at) VALUES(?,?,?,?,?,'created',?)`,
		o.ID, o.CompanyID, o.InvoiceID, o.Amount, o.Currency, now())
	return err
}

func (s *Store) PayPalOrder(id string) (*PayPalOrder, error) {
	var o PayPalOrder
	err := s.DB.QueryRow(`SELECT id, company_id, invoice_id, amount, currency, status, created_at FROM paypal_orders WHERE id = ?`, id).
		Scan(&o.ID, &o.CompanyID, &o.InvoiceID, &o.Amount, &o.Currency, &o.Status, &o.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &o, nil
}

func (s *Store) SetPayPalOrderStatus(id, status string) {
	s.DB.Exec(`UPDATE paypal_orders SET status = ? WHERE id = ?`, status, id)
}

// OpenPayPalOrders lists recent orders that may still be paid (followed up
// when the client did not come back from PayPal).
func (s *Store) OpenPayPalOrders() ([]PayPalOrder, error) {
	rows, err := s.DB.Query(`SELECT o.id, o.company_id, o.invoice_id, o.amount, o.currency, o.status, o.created_at FROM paypal_orders o
		JOIN invoices i ON i.id = o.invoice_id WHERE o.status = 'created' AND o.created_at > ? AND i.status = 'open'`, now()-3*86400)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PayPalOrder
	for rows.Next() {
		var o PayPalOrder
		if err := rows.Scan(&o.ID, &o.CompanyID, &o.InvoiceID, &o.Amount, &o.Currency, &o.Status, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
