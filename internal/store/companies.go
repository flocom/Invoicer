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
	EmailBankDetails    bool
	BankCount           int // number of bank accounts
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
func (c *Company) HasBank() bool   { return c.BankCount > 0 }

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
	stripe_webhook_id, stripe_account, reminders_enabled, reminder_days, archived, created_at, email_bank_details,
	(SELECT COUNT(*) FROM bank_accounts b WHERE b.company_id = companies.id)`

func scanCompany(row interface{ Scan(...any) error }) (*Company, error) {
	c := &Company{}
	err := row.Scan(&c.ID, &c.PublicID, &c.Name, &c.LegalName, &c.Address, &c.Email, &c.Phone, &c.Website, &c.TaxID,
		&c.RegistrationID, &c.Logo, &c.AccentColor, &c.DefaultCurrency, &c.DefaultLang, &c.DefaultTaxBP, &c.InvoicePrefix,
		&c.PaymentTermsDays, &c.BankHolder, &c.BankName, &c.IBAN, &c.BIC, &c.BankExtra, &c.DefaultNotes, &c.Footer,
		&c.ResendKey, &c.EmailFrom, &c.EmailReplyTo, &c.EmailBCC, &c.StripeKey, &c.StripeWebhookSecret, &c.StripeWebhookID,
		&c.StripeAccount, &c.RemindersEnabled, &c.ReminderDays, &c.Archived, &c.CreatedAt, &c.EmailBankDetails, &c.BankCount)
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
		payment_terms_days=?, default_notes=?, footer=?, reminders_enabled=?, reminder_days=? WHERE id=?`,
		c.DefaultCurrency, c.DefaultLang, c.DefaultTaxBP, c.InvoicePrefix, c.PaymentTermsDays, c.DefaultNotes, c.Footer,
		b2i(c.RemindersEnabled), c.ReminderDays, c.ID)
	return err
}

func (s *Store) SetEmailBankDetails(companyID int64, on bool) error {
	_, err := s.DB.Exec(`UPDATE companies SET email_bank_details = ? WHERE id = ?`, b2i(on), companyID)
	return err
}

func (s *Store) SetCompanyAccent(id int64, color string) error {
	_, err := s.DB.Exec(`UPDATE companies SET accent_color = ? WHERE id = ?`, color, id)
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
	Shared      bool // visible and usable in every company
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

const clientCols = `id, company_id, name, contact_name, email, cc_emails, address, tax_id, lang, currency, notes, archived, shared, created_at`

func scanClient(row interface{ Scan(...any) error }) (*Client, error) {
	c := &Client{}
	err := row.Scan(&c.ID, &c.CompanyID, &c.Name, &c.ContactName, &c.Email, &c.CCEmails, &c.Address, &c.TaxID, &c.Lang,
		&c.Currency, &c.Notes, &c.Archived, &c.Shared, &c.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return c, nil
}

func (s *Store) Clients(companyID int64, includeArchived bool, search string) ([]*Client, error) {
	q := `SELECT ` + clientCols + ` FROM clients WHERE (company_id = ? OR shared = 1)`
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

// Client loads a client and checks it belongs to the company or is shared.
func (s *Store) Client(companyID, id int64) (*Client, error) {
	return scanClient(s.DB.QueryRow(`SELECT `+clientCols+` FROM clients WHERE id = ? AND (company_id = ? OR shared = 1)`, id, companyID))
}

func (s *Store) SaveClient(c *Client) error {
	if c.ID == 0 {
		res, err := s.DB.Exec(`INSERT INTO clients(company_id, name, contact_name, email, cc_emails, address, tax_id, lang, currency, notes, shared, created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, c.CompanyID, c.Name, c.ContactName, c.Email, c.CCEmails, c.Address, c.TaxID, c.Lang, c.Currency, c.Notes,
			b2i(c.Shared), now())
		if err != nil {
			return err
		}
		c.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := s.DB.Exec(`UPDATE clients SET name=?, contact_name=?, email=?, cc_emails=?, address=?, tax_id=?, lang=?, currency=?, notes=?, archived=?,
		shared=? WHERE id=? AND company_id=?`, c.Name, c.ContactName, c.Email, c.CCEmails, c.Address, c.TaxID, c.Lang, c.Currency, c.Notes,
		b2i(c.Archived), b2i(c.Shared), c.ID, c.CompanyID)
	return err
}

// UnshareClient makes a shared client private to companyID again. It refuses
// (false) while another company still has invoices or recurring invoices for it.
func (s *Store) UnshareClient(id, companyID int64) (bool, error) {
	res, err := s.DB.Exec(`UPDATE clients SET shared = 0, company_id = ? WHERE id = ? AND shared = 1
		AND NOT EXISTS (SELECT 1 FROM invoices WHERE client_id = ? AND company_id != ?)
		AND NOT EXISTS (SELECT 1 FROM recurring WHERE client_id = ? AND company_id != ?)`, companyID, id, id, companyID, id, companyID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
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
	_, err := s.DB.Exec(`DELETE FROM clients WHERE id = ? AND (company_id = ? OR shared = 1)`, id, companyID)
	return err == nil, err
}

func idsArgs(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// ---------- client copies across companies ----------

// RelatedClient is the same customer (same e-mail, or same name when there is
// no e-mail) in another company the user can access. A shared client is
// related to itself in every other company.
type RelatedClient struct {
	CompanyID   int64
	CompanyName string
	ClientID    int64
}

func (s *Store) RelatedClients(u *User, cl *Client, currentCompany int64) ([]RelatedClient, error) {
	cos, err := s.CompaniesFor(u, false)
	if err != nil {
		return nil, err
	}
	var out []RelatedClient
	for _, co := range cos {
		if co.ID == currentCompany {
			continue
		}
		if cl.Shared {
			out = append(out, RelatedClient{CompanyID: co.ID, CompanyName: co.Name, ClientID: cl.ID})
			continue
		}
		var id int64
		var err error
		if strings.TrimSpace(cl.Email) != "" {
			err = s.DB.QueryRow(`SELECT id FROM clients WHERE company_id = ? AND email = ? COLLATE NOCASE ORDER BY archived, id LIMIT 1`,
				co.ID, strings.TrimSpace(cl.Email)).Scan(&id)
		} else {
			err = s.DB.QueryRow(`SELECT id FROM clients WHERE company_id = ? AND email = '' AND name = ? COLLATE NOCASE ORDER BY archived, id LIMIT 1`,
				co.ID, cl.Name).Scan(&id)
		}
		if err == nil {
			out = append(out, RelatedClient{CompanyID: co.ID, CompanyName: co.Name, ClientID: id})
		}
	}
	return out, nil
}

// CopyClient duplicates a client into another company and returns the copy.
func (s *Store) CopyClient(src *Client, targetCompany int64) (*Client, error) {
	c := *src
	c.ID = 0
	c.CompanyID = targetCompany
	c.Archived = false
	c.Shared = false
	if err := s.SaveClient(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ---------- line suggestions ----------

// LineSuggestion is a previously invoiced product or service.
type LineSuggestion struct {
	Description string `json:"description"`
	UnitPrice   int64  `json:"-"`
	TaxBP       int64  `json:"-"`
	Currency    string `json:"currency"`
	Price       string `json:"price"`
	Tax         string `json:"tax"`
}

// LineSuggestions returns distinct descriptions matching q (most recent use
// first) with the last price and tax rate used.
func suggestionKey(description string) string { return strings.ToLower(strings.TrimSpace(description)) }

// HideLineSuggestion removes a product or service from the suggestions until
// it is invoiced again.
func (s *Store) HideLineSuggestion(companyID int64, description string) error {
	k := suggestionKey(description)
	if k == "" {
		return nil
	}
	_, err := s.DB.Exec(`INSERT INTO hidden_suggestions(company_id, description, hidden_at) VALUES(?,?,?)
		ON CONFLICT(company_id, description) DO UPDATE SET hidden_at = excluded.hidden_at`, companyID, k, now())
	return err
}

func (s *Store) LineSuggestions(companyID int64, q string, limit int) ([]LineSuggestion, error) {
	like := "%" + escapeLike(strings.TrimSpace(q)) + "%"
	hidden := map[string]int64{}
	if hr, err := s.DB.Query(`SELECT description, hidden_at FROM hidden_suggestions WHERE company_id = ?`, companyID); err == nil {
		for hr.Next() {
			var k string
			var at int64
			if hr.Scan(&k, &at) == nil {
				hidden[k] = at
			}
		}
		hr.Close()
	}
	rows, err := s.DB.Query(`SELECT description, unit_price, tax_bp, currency, t FROM (
			SELECT l.description, l.unit_price, l.tax_bp, i.currency, i.created_at AS t, l.id AS lid
			FROM invoice_lines l JOIN invoices i ON i.id = l.invoice_id
			WHERE i.company_id = ? AND l.description LIKE ? ESCAPE '\'
			UNION ALL
			SELECT l.description, l.unit_price, l.tax_bp, r.currency, r.created_at AS t, l.id AS lid
			FROM recurring_lines l JOIN recurring r ON r.id = l.recurring_id
			WHERE r.company_id = ? AND l.description LIKE ? ESCAPE '\'
		) ORDER BY t DESC, lid DESC LIMIT 300`, companyID, like, companyID, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []LineSuggestion
	for rows.Next() {
		var l LineSuggestion
		var used int64
		if err := rows.Scan(&l.Description, &l.UnitPrice, &l.TaxBP, &l.Currency, &used); err != nil {
			return nil, err
		}
		k := suggestionKey(l.Description)
		if seen[k] || k == "" {
			continue
		}
		seen[k] = true
		if at, ok := hidden[k]; ok && used <= at {
			continue // removed from the list and not used since
		}
		out = append(out, l)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}
