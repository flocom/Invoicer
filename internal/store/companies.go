package store

import (
	"strconv"
	"strings"
)

type Company struct {
	ID                  int64
	PublicID            string
	Name                string
	LegalName           string
	Address             string
	Email               string
	Phone               string
	Website             string
	TaxID               string
	RegistrationID      string
	Logo                []byte
	AccentColor         string
	DefaultCurrency     string
	DefaultLang         string
	DefaultTaxBP        int64
	InvoicePrefix       string
	PaymentTermsDays    int
	BankHolder          string
	BankName            string
	IBAN                string
	BIC                 string
	BankExtra           string
	DefaultNotes        string
	Footer              string
	ResendKey           []byte
	EmailFrom           string
	EmailReplyTo        string
	EmailBCC            string
	StripeKey           []byte
	StripeWebhookSecret []byte
	StripeWebhookID     string
	StripeAccount       string
	RemindersEnabled    bool
	ReminderDays        string
	Archived            bool
	CreatedAt           int64
}

func (c *Company) DisplayName() string {
	if c.LegalName != "" {
		return c.LegalName
	}
	return c.Name
}

func (c *Company) HasResend() bool { return len(c.ResendKey) > 0 && c.EmailFrom != "" }
func (c *Company) HasStripe() bool { return len(c.StripeKey) > 0 }
func (c *Company) HasBank() bool   { return c.IBAN != "" || c.BankExtra != "" }

// ReminderOffsets parses "-3,0,7" into day offsets relative to the due date.
func (c *Company) ReminderOffsets() []int {
	var out []int
	for _, p := range strings.Split(c.ReminderDays, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n >= -60 && n <= 365 {
			out = append(out, n)
		}
	}
	return out
}

const companyCols = `id, public_id, name, legal_name, address, email, phone, website, tax_id, registration_id, logo, accent_color,
	default_currency, default_lang, default_tax_bp, invoice_prefix, payment_terms_days, bank_holder, bank_name, iban, bic,
	bank_extra, default_notes, footer, resend_key, email_from, email_reply_to, email_bcc, stripe_key, stripe_webhook_secret,
	stripe_webhook_id, stripe_account, reminders_enabled, reminder_days, archived, created_at`

func scanCompany(row interface{ Scan(...any) error }) (*Company, error) {
	c := &Company{}
	err := row.Scan(&c.ID, &c.PublicID, &c.Name, &c.LegalName, &c.Address, &c.Email, &c.Phone, &c.Website, &c.TaxID,
		&c.RegistrationID, &c.Logo, &c.AccentColor, &c.DefaultCurrency, &c.DefaultLang, &c.DefaultTaxBP, &c.InvoicePrefix,
		&c.PaymentTermsDays, &c.BankHolder, &c.BankName, &c.IBAN, &c.BIC, &c.BankExtra, &c.DefaultNotes, &c.Footer,
		&c.ResendKey, &c.EmailFrom, &c.EmailReplyTo, &c.EmailBCC, &c.StripeKey, &c.StripeWebhookSecret, &c.StripeWebhookID,
		&c.StripeAccount, &c.RemindersEnabled, &c.ReminderDays, &c.Archived, &c.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return c, nil
}

func (s *Store) CreateCompany(publicID, name, currency, lang string, creator int64) (*Company, error) {
	res, err := s.DB.Exec(`INSERT INTO companies(public_id, name, default_currency, default_lang, created_at) VALUES(?,?,?,?,?)`,
		publicID, name, currency, lang, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if creator != 0 {
		s.DB.Exec(`INSERT OR IGNORE INTO company_members(company_id, user_id) VALUES(?,?)`, id, creator)
	}
	return s.Company(id)
}

func (s *Store) Company(id int64) (*Company, error) {
	return scanCompany(s.DB.QueryRow(`SELECT `+companyCols+` FROM companies WHERE id = ?`, id))
}

func (s *Store) CompanyByPublicID(pid string) (*Company, error) {
	return scanCompany(s.DB.QueryRow(`SELECT `+companyCols+` FROM companies WHERE public_id = ?`, pid))
}

// CompaniesFor returns the companies a user may access: all of them for
// owners and admins, memberships otherwise.
func (s *Store) CompaniesFor(u *User, includeArchived bool) ([]*Company, error) {
	q := `SELECT ` + companyCols + ` FROM companies WHERE 1=1`
	var args []any
	if !u.IsAdmin() {
		q += ` AND id IN (SELECT company_id FROM company_members WHERE user_id = ?)`
		args = append(args, u.ID)
	}
	if !includeArchived {
		q += ` AND archived = 0`
	}
	q += ` ORDER BY archived, name COLLATE NOCASE`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Company
	for rows.Next() {
		c, err := scanCompany(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AllCompanies() ([]*Company, error) {
	rows, err := s.DB.Query(`SELECT ` + companyCols + ` FROM companies WHERE archived = 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Company
	for rows.Next() {
		c, err := scanCompany(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CanAccessCompany(u *User, companyID int64) bool {
	if u.IsAdmin() {
		var n int
		s.DB.QueryRow(`SELECT COUNT(*) FROM companies WHERE id = ?`, companyID).Scan(&n)
		return n == 1
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM company_members WHERE company_id = ? AND user_id = ?`, companyID, u.ID).Scan(&n)
	return n == 1
}

func (s *Store) UpdateCompanyGeneral(c *Company) error {
	_, err := s.DB.Exec(`UPDATE companies SET name=?, legal_name=?, address=?, email=?, phone=?, website=?, tax_id=?,
		registration_id=?, accent_color=? WHERE id=?`,
		c.Name, c.LegalName, c.Address, c.Email, c.Phone, c.Website, c.TaxID, c.RegistrationID, c.AccentColor, c.ID)
	return err
}

func (s *Store) UpdateCompanyInvoicing(c *Company) error {
	_, err := s.DB.Exec(`UPDATE companies SET default_currency=?, default_lang=?, default_tax_bp=?, invoice_prefix=?,
		payment_terms_days=?, default_notes=?, footer=?, bank_holder=?, bank_name=?, iban=?, bic=?, bank_extra=?,
		reminders_enabled=?, reminder_days=? WHERE id=?`,
		c.DefaultCurrency, c.DefaultLang, c.DefaultTaxBP, c.InvoicePrefix, c.PaymentTermsDays, c.DefaultNotes, c.Footer,
		c.BankHolder, c.BankName, c.IBAN, c.BIC, c.BankExtra, b2i(c.RemindersEnabled), c.ReminderDays, c.ID)
	return err
}

func (s *Store) SetCompanyLogo(id int64, png []byte) error {
	_, err := s.DB.Exec(`UPDATE companies SET logo = ? WHERE id = ?`, png, id)
	return err
}

func (s *Store) UpdateCompanyEmail(id int64, key []byte, keepKey bool, from, replyTo, bcc string) error {
	if keepKey {
		_, err := s.DB.Exec(`UPDATE companies SET email_from=?, email_reply_to=?, email_bcc=? WHERE id=?`, from, replyTo, bcc, id)
		return err
	}
	_, err := s.DB.Exec(`UPDATE companies SET resend_key=?, email_from=?, email_reply_to=?, email_bcc=? WHERE id=?`, key, from, replyTo, bcc, id)
	return err
}

func (s *Store) UpdateCompanyStripe(id int64, key, webhookSecret []byte, webhookID, account string) error {
	_, err := s.DB.Exec(`UPDATE companies SET stripe_key=?, stripe_webhook_secret=?, stripe_webhook_id=?, stripe_account=? WHERE id=?`,
		key, webhookSecret, webhookID, account, id)
	return err
}

func (s *Store) SetCompanyArchived(id int64, archived bool) error {
	_, err := s.DB.Exec(`UPDATE companies SET archived = ? WHERE id = ?`, b2i(archived), id)
	return err
}

func (s *Store) CompanyMembers(companyID int64) (map[int64]bool, error) {
	rows, err := s.DB.Query(`SELECT user_id FROM company_members WHERE company_id = ?`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) UserCompanyIDs(userID int64) (map[int64]bool, error) {
	rows, err := s.DB.Query(`SELECT company_id FROM company_members WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) SetUserCompanies(userID int64, ids []int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM company_members WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO company_members(company_id, user_id) SELECT id, ? FROM companies WHERE id = ?`, userID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- clients ----------

type Client struct {
	ID          int64
	CompanyID   int64
	Name        string
	ContactName string
	Email       string
	CCEmails    string
	Address     string
	TaxID       string
	Lang        string
	Currency    string
	Notes       string
	Archived    bool
	CreatedAt   int64
	// computed
	Outstanding map[string]int64
}

// Recipients returns the main address followed by CC addresses.
func (c *Client) Recipients() []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range append([]string{c.Email}, strings.FieldsFunc(c.CCEmails, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' })...) {
		e = strings.TrimSpace(e)
		if e != "" && !seen[strings.ToLower(e)] {
			seen[strings.ToLower(e)] = true
			out = append(out, e)
		}
	}
	return out
}

const clientCols = `id, company_id, name, contact_name, email, cc_emails, address, tax_id, lang, currency, notes, archived, created_at`

func scanClient(row interface{ Scan(...any) error }) (*Client, error) {
	c := &Client{}
	err := row.Scan(&c.ID, &c.CompanyID, &c.Name, &c.ContactName, &c.Email, &c.CCEmails, &c.Address, &c.TaxID, &c.Lang,
		&c.Currency, &c.Notes, &c.Archived, &c.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return c, nil
}

func (s *Store) Clients(companyID int64, includeArchived bool, search string) ([]*Client, error) {
	q := `SELECT ` + clientCols + ` FROM clients WHERE company_id = ?`
	args := []any{companyID}
	if !includeArchived {
		q += ` AND archived = 0`
	}
	if search != "" {
		q += ` AND (name LIKE ? ESCAPE '\' OR email LIKE ? ESCAPE '\' OR contact_name LIKE ? ESCAPE '\')`
		like := "%" + escapeLike(search) + "%"
		args = append(args, like, like, like)
	}
	q += ` ORDER BY archived, name COLLATE NOCASE`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Client
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Client loads a client and checks it belongs to the company.
func (s *Store) Client(companyID, id int64) (*Client, error) {
	return scanClient(s.DB.QueryRow(`SELECT `+clientCols+` FROM clients WHERE id = ? AND company_id = ?`, id, companyID))
}

func (s *Store) SaveClient(c *Client) error {
	if c.ID == 0 {
		res, err := s.DB.Exec(`INSERT INTO clients(company_id, name, contact_name, email, cc_emails, address, tax_id, lang, currency, notes, created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, c.CompanyID, c.Name, c.ContactName, c.Email, c.CCEmails, c.Address, c.TaxID, c.Lang, c.Currency, c.Notes, now())
		if err != nil {
			return err
		}
		c.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := s.DB.Exec(`UPDATE clients SET name=?, contact_name=?, email=?, cc_emails=?, address=?, tax_id=?, lang=?, currency=?, notes=?, archived=?
		WHERE id=? AND company_id=?`, c.Name, c.ContactName, c.Email, c.CCEmails, c.Address, c.TaxID, c.Lang, c.Currency, c.Notes,
		b2i(c.Archived), c.ID, c.CompanyID)
	return err
}

func (s *Store) ClientOutstanding(companyID int64) (map[int64]map[string]int64, error) {
	rows, err := s.DB.Query(`SELECT client_id, currency, SUM(total - amount_paid) FROM invoices
		WHERE company_id = ? AND status = 'open' GROUP BY client_id, currency`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]int64{}
	for rows.Next() {
		var id, amt int64
		var cur string
		rows.Scan(&id, &cur, &amt)
		if out[id] == nil {
			out[id] = map[string]int64{}
		}
		out[id][cur] = amt
	}
	return out, rows.Err()
}

func (s *Store) DeleteClient(companyID, id int64) (bool, error) {
	var n int
	s.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM invoices WHERE client_id = ?) + (SELECT COUNT(*) FROM recurring WHERE client_id = ?)`, id, id).Scan(&n)
	if n > 0 {
		return false, nil
	}
	_, err := s.DB.Exec(`DELETE FROM clients WHERE id = ? AND company_id = ?`, id, companyID)
	return err == nil, err
}

func idsArgs(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
