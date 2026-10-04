package store

import (
	"math/big"
	"strings"
)

// Bank account choice stored on invoices and recurring schedules.
const (
	BankAuto int64 = 0  // the first account in the invoice currency
	BankNone int64 = -1 // no bank details
)

type BankAccount struct {
	ID        int64
	CompanyID int64
	Label     string
	Currency  string
	Holder    string
	BankName  string
	IBAN      string
	BIC       string
	Extra     string
	Position  int
}

// Name is a short label for selects ("Main account · EUR · …0189").
func (b *BankAccount) Name() string {
	parts := []string{}
	if b.Label != "" {
		parts = append(parts, b.Label)
	} else if b.BankName != "" {
		parts = append(parts, b.BankName)
	}
	parts = append(parts, b.Currency)
	if n := len(b.IBAN); n > 4 {
		parts = append(parts, "…"+b.IBAN[n-4:])
	}
	return strings.Join(parts, " · ")
}

const bankCols = `id, company_id, label, currency, holder, bank_name, iban, bic, extra, position`

func scanBank(row interface{ Scan(...any) error }) (*BankAccount, error) {
	b := &BankAccount{}
	if err := row.Scan(&b.ID, &b.CompanyID, &b.Label, &b.Currency, &b.Holder, &b.BankName, &b.IBAN, &b.BIC, &b.Extra, &b.Position); err != nil {
		return nil, notFound(err)
	}
	return b, nil
}

func (s *Store) BankAccounts(companyID int64) ([]*BankAccount, error) {
	rows, err := s.DB.Query(`SELECT `+bankCols+` FROM bank_accounts WHERE company_id = ? ORDER BY position, id`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BankAccount
	for rows.Next() {
		b, err := scanBank(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) BankAccount(companyID, id int64) (*BankAccount, error) {
	return scanBank(s.DB.QueryRow(`SELECT `+bankCols+` FROM bank_accounts WHERE id = ? AND company_id = ?`, id, companyID))
}

func (s *Store) SaveBankAccount(b *BankAccount) error {
	if b.ID == 0 {
		res, err := s.DB.Exec(`INSERT INTO bank_accounts(company_id, label, currency, holder, bank_name, iban, bic, extra, position, created_at)
			VALUES(?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(position), 0) + 1 FROM bank_accounts WHERE company_id = ?),?)`,
			b.CompanyID, b.Label, b.Currency, b.Holder, b.BankName, b.IBAN, b.BIC, b.Extra, b.CompanyID, now())
		if err != nil {
			return err
		}
		b.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := s.DB.Exec(`UPDATE bank_accounts SET label=?, currency=?, holder=?, bank_name=?, iban=?, bic=?, extra=?
		WHERE id=? AND company_id=?`, b.Label, b.Currency, b.Holder, b.BankName, b.IBAN, b.BIC, b.Extra, b.ID, b.CompanyID)
	return err
}

// DeleteBankAccount removes an account; invoices that pointed to it fall
// back to the automatic choice.
func (s *Store) DeleteBankAccount(companyID, id int64) error {
	res, err := s.DB.Exec(`DELETE FROM bank_accounts WHERE id = ? AND company_id = ?`, id, companyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	s.DB.Exec(`UPDATE invoices SET bank_account_id = 0 WHERE company_id = ? AND bank_account_id = ?`, companyID, id)
	s.DB.Exec(`UPDATE recurring SET bank_account_id = 0 WHERE company_id = ? AND bank_account_id = ?`, companyID, id)
	return nil
}

// ResolveBank returns the account to show for a choice and a currency, or nil.
func (s *Store) ResolveBank(companyID, choice int64, currency string) *BankAccount {
	if choice == BankNone {
		return nil
	}
	if choice > 0 {
		if b, err := s.BankAccount(companyID, choice); err == nil {
			return b
		}
	}
	b, err := scanBank(s.DB.QueryRow(`SELECT `+bankCols+` FROM bank_accounts WHERE company_id = ? AND currency = ?
		ORDER BY position, id LIMIT 1`, companyID, currency))
	if err != nil {
		return nil
	}
	return b
}

// ValidIBAN checks the ISO 13616 structure and mod-97 checksum.
func ValidIBAN(iban string) bool {
	iban = strings.ToUpper(strings.ReplaceAll(iban, " ", ""))
	if len(iban) < 15 || len(iban) > 34 {
		return false
	}
	for i, r := range iban {
		isLetter, isDigit := r >= 'A' && r <= 'Z', r >= '0' && r <= '9'
		if !isLetter && !isDigit || (i < 2 && !isLetter) || (i >= 2 && i < 4 && !isDigit) {
			return false
		}
	}
	var num strings.Builder
	for _, r := range iban[4:] + iban[:4] {
		if r >= 'A' && r <= 'Z' {
			num.WriteString(big.NewInt(int64(r - 'A' + 10)).String())
		} else {
			num.WriteRune(r)
		}
	}
	n, ok := new(big.Int).SetString(num.String(), 10)
	return ok && new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}
