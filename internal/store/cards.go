package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ---------- Stripe customers ----------

// StripeCustomer links a client to its customer on the company's Stripe
// account. CardToken is the secret of the public "update my card" page.
type StripeCustomer struct {
	CompanyID  int64
	ClientID   int64
	CustomerID string
	CardToken  string
}

func (s *Store) StripeCustomer(companyID, clientID int64) (*StripeCustomer, error) {
	sc := &StripeCustomer{}
	err := s.DB.QueryRow(`SELECT company_id, client_id, customer_id, card_token FROM stripe_customers WHERE company_id = ? AND client_id = ?`,
		companyID, clientID).Scan(&sc.CompanyID, &sc.ClientID, &sc.CustomerID, &sc.CardToken)
	if err != nil {
		return nil, notFound(err)
	}
	return sc, nil
}

func (s *Store) StripeCustomerByToken(token string) (*StripeCustomer, error) {
	if len(token) < 20 || len(token) > 64 {
		return nil, ErrNotFound
	}
	sc := &StripeCustomer{}
	err := s.DB.QueryRow(`SELECT company_id, client_id, customer_id, card_token FROM stripe_customers WHERE card_token = ?`, token).
		Scan(&sc.CompanyID, &sc.ClientID, &sc.CustomerID, &sc.CardToken)
	if err != nil {
		return nil, notFound(err)
	}
	return sc, nil
}

// SaveStripeCustomer stores the link unless one already exists, and returns
// the one in the database (a concurrent request may have created it first).
func (s *Store) SaveStripeCustomer(sc StripeCustomer) (*StripeCustomer, error) {
	_, err := s.DB.Exec(`INSERT INTO stripe_customers(company_id, client_id, customer_id, card_token, created_at) VALUES(?,?,?,?,?)
		ON CONFLICT(company_id, client_id) DO NOTHING`, sc.CompanyID, sc.ClientID, sc.CustomerID, sc.CardToken, now())
	if err != nil {
		return nil, err
	}
	return s.StripeCustomer(sc.CompanyID, sc.ClientID)
}

// ---------- saved cards ----------

type SavedCard struct {
	ID            int64
	CompanyID     int64
	ClientID      int64
	PaymentMethod string
	Brand         string
	Last4         string
	ExpMonth      int
	ExpYear       int
	Default       bool
	CreatedAt     int64
}

// Label is a short human description, e.g. "Visa •••• 4242 (04/2028)".
func (c *SavedCard) Label() string {
	brand := c.Brand
	if brand == "" {
		brand = "card"
	}
	return fmt.Sprintf("%s •••• %s (%02d/%d)", strings.ToUpper(brand[:1])+brand[1:], c.Last4, c.ExpMonth, c.ExpYear)
}

// Expired reports whether the card expired before the given ISO date.
func (c *SavedCard) Expired(today string) bool {
	return fmt.Sprintf("%04d-%02d", c.ExpYear, c.ExpMonth) < today[:min(7, len(today))]
}

const cardCols = `id, company_id, client_id, payment_method, brand, last4, exp_month, exp_year, is_default, created_at`

func scanCard(row interface{ Scan(...any) error }) (*SavedCard, error) {
	c := &SavedCard{}
	if err := row.Scan(&c.ID, &c.CompanyID, &c.ClientID, &c.PaymentMethod, &c.Brand, &c.Last4, &c.ExpMonth, &c.ExpYear, &c.Default,
		&c.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	return c, nil
}

// SaveCard records a card saved on Stripe and makes it the client's default
// (the most recently given card is the one charged automatically).
func (s *Store) SaveCard(c *SavedCard) error {
	return withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE client_cards SET is_default = 0 WHERE company_id = ? AND client_id = ?`, c.CompanyID, c.ClientID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO client_cards(company_id, client_id, payment_method, brand, last4, exp_month, exp_year, is_default, created_at)
			VALUES(?,?,?,?,?,?,?,1,?) ON CONFLICT(company_id, payment_method) DO UPDATE SET client_id = excluded.client_id,
			brand = excluded.brand, last4 = excluded.last4, exp_month = excluded.exp_month, exp_year = excluded.exp_year, is_default = 1`,
			c.CompanyID, c.ClientID, c.PaymentMethod, c.Brand, c.Last4, c.ExpMonth, c.ExpYear, now())
		return err
	})
}

func (s *Store) Cards(companyID, clientID int64) ([]*SavedCard, error) {
	rows, err := s.DB.Query(`SELECT `+cardCols+` FROM client_cards WHERE company_id = ? AND client_id = ? ORDER BY is_default DESC, id DESC`,
		companyID, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SavedCard
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Card(companyID, clientID, id int64) (*SavedCard, error) {
	return scanCard(s.DB.QueryRow(`SELECT `+cardCols+` FROM client_cards WHERE id = ? AND company_id = ? AND client_id = ?`, id, companyID, clientID))
}

func (s *Store) DefaultCard(companyID, clientID int64) (*SavedCard, error) {
	return scanCard(s.DB.QueryRow(`SELECT `+cardCols+` FROM client_cards WHERE company_id = ? AND client_id = ? AND is_default = 1`,
		companyID, clientID))
}

func (s *Store) SetDefaultCard(companyID, clientID, id int64) error {
	_, err := s.DB.Exec(`UPDATE client_cards SET is_default = (id = ?) WHERE company_id = ? AND client_id = ?
		AND EXISTS (SELECT 1 FROM client_cards WHERE id = ? AND company_id = ? AND client_id = ?)`, id, companyID, clientID, id, companyID, clientID)
	return err
}

// DeleteCard forgets a card; the newest remaining one becomes the default.
func (s *Store) DeleteCard(companyID, clientID, id int64) error {
	return withTx(context.Background(), s.DB, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM client_cards WHERE id = ? AND company_id = ? AND client_id = ?`, id, companyID, clientID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE client_cards SET is_default = 1 WHERE id = (SELECT id FROM client_cards WHERE company_id = ? AND client_id = ?
			ORDER BY id DESC LIMIT 1) AND NOT EXISTS (SELECT 1 FROM client_cards WHERE company_id = ? AND client_id = ? AND is_default = 1)`,
			companyID, clientID, companyID, clientID)
		return err
	})
}

// ---------- card setup sessions ("update my card" page) ----------

type CardSetup struct {
	ID        string
	CompanyID int64
	ClientID  int64
	Status    string
}

func (s *Store) SaveCardSetup(id string, companyID, clientID, expiresAt int64) error {
	_, err := s.DB.Exec(`INSERT INTO card_setups(id, company_id, client_id, created_at, expires_at) VALUES(?,?,?,?,?)`,
		id, companyID, clientID, now(), expiresAt)
	return err
}

func (s *Store) CardSetup(id string) (*CardSetup, error) {
	cs := &CardSetup{}
	err := s.DB.QueryRow(`SELECT id, company_id, client_id, status FROM card_setups WHERE id = ?`, id).Scan(&cs.ID, &cs.CompanyID, &cs.ClientID, &cs.Status)
	if err != nil {
		return nil, notFound(err)
	}
	return cs, nil
}

func (s *Store) SetCardSetupStatus(id, status string) {
	s.DB.Exec(`UPDATE card_setups SET status = ? WHERE id = ?`, status, id)
}

// PendingCardSetups lists sessions that may still complete (for polling
// when webhooks are not delivered).
func (s *Store) PendingCardSetups() ([]CardSetup, error) {
	rows, err := s.DB.Query(`SELECT id, company_id, client_id, status FROM card_setups WHERE status = 'open' AND expires_at > ?`, now()-3600)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CardSetup
	for rows.Next() {
		var cs CardSetup
		if err := rows.Scan(&cs.ID, &cs.CompanyID, &cs.ClientID, &cs.Status); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// ---------- off-session charges ----------

// CardCharge is one attempt to charge a saved card for an invoice.
type CardCharge struct {
	ID            int64
	CompanyID     int64
	InvoiceID     int64
	CardID        int64
	CardLabel     string
	PaymentIntent string
	Amount        int64
	Status        string // pending, succeeded, processing, failed
	Error         string
	Automatic     bool
	CreatedBy     int64
	CreatedAt     int64
}

const chargeCols = `id, company_id, invoice_id, card_id, card_label, payment_intent, amount, status, error, automatic, created_by, created_at`

func scanCharge(row interface{ Scan(...any) error }) (*CardCharge, error) {
	c := &CardCharge{}
	if err := row.Scan(&c.ID, &c.CompanyID, &c.InvoiceID, &c.CardID, &c.CardLabel, &c.PaymentIntent, &c.Amount, &c.Status, &c.Error,
		&c.Automatic, &c.CreatedBy, &c.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	return c, nil
}

func (s *Store) CreateCharge(c *CardCharge) error {
	res, err := s.DB.Exec(`INSERT INTO card_charges(company_id, invoice_id, card_id, card_label, amount, status, automatic, created_by, created_at)
		VALUES(?,?,?,?,?,'pending',?,?,?)`, c.CompanyID, c.InvoiceID, c.CardID, c.CardLabel, c.Amount, b2i(c.Automatic), c.CreatedBy, now())
	if err != nil {
		return err
	}
	c.ID, _ = res.LastInsertId()
	c.Status = "pending"
	return nil
}

func (s *Store) UpdateCharge(id int64, paymentIntent, status, errMsg string) error {
	_, err := s.DB.Exec(`UPDATE card_charges SET payment_intent = CASE WHEN ? != '' THEN ? ELSE payment_intent END, status = ?, error = ? WHERE id = ?`,
		paymentIntent, paymentIntent, status, errMsg, id)
	return err
}

func (s *Store) InvoiceCharges(invoiceID int64) ([]*CardCharge, error) {
	return s.charges(`SELECT `+chargeCols+` FROM card_charges WHERE invoice_id = ? ORDER BY id DESC`, invoiceID)
}

// ProcessingCharges lists recent charges whose outcome Stripe has not given yet.
func (s *Store) ProcessingCharges() ([]*CardCharge, error) {
	return s.charges(`SELECT `+chargeCols+` FROM card_charges WHERE status = 'processing' AND payment_intent != '' AND created_at > ?`,
		now()-14*86400)
}

func (s *Store) charges(q string, args ...any) ([]*CardCharge, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CardCharge
	for rows.Next() {
		c, err := scanCharge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
