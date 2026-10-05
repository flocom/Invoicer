package store

// migrations are applied in order and never edited once released. A binary
// that finds a newer schema than it knows keeps working (migrations are
// additive) so an automatic rollback stays possible.
var migrations = []string{
	// 1 — initial schema
	`
CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE users (
	id                INTEGER PRIMARY KEY,
	email             TEXT NOT NULL UNIQUE COLLATE NOCASE,
	name              TEXT NOT NULL,
	password_hash     TEXT NOT NULL,
	role              TEXT NOT NULL CHECK (role IN ('owner','admin','member')),
	lang              TEXT NOT NULL DEFAULT 'en',
	totp_secret       BLOB,
	totp_enabled      INTEGER NOT NULL DEFAULT 0,
	totp_last_counter INTEGER NOT NULL DEFAULT 0,
	recovery_codes    TEXT NOT NULL DEFAULT '',
	disabled          INTEGER NOT NULL DEFAULT 0,
	failed_logins     INTEGER NOT NULL DEFAULT 0,
	locked_until      INTEGER NOT NULL DEFAULT 0,
	created_at        INTEGER NOT NULL,
	last_login_at     INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX users_single_owner ON users(role) WHERE role = 'owner';

CREATE TABLE sessions (
	id_hash     BLOB PRIMARY KEY,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	csrf        TEXT NOT NULL,
	mfa_pending INTEGER NOT NULL DEFAULT 0,
	company_id  INTEGER NOT NULL DEFAULT 0,
	ip          TEXT NOT NULL DEFAULT '',
	user_agent  TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE invites (
	token_hash  BLOB PRIMARY KEY,
	email       TEXT NOT NULL,
	role        TEXT NOT NULL,
	company_ids TEXT NOT NULL DEFAULT '',
	created_by  INTEGER NOT NULL,
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	used_at     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE password_resets (
	token_hash BLOB PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE companies (
	id                    INTEGER PRIMARY KEY,
	public_id             TEXT NOT NULL UNIQUE,
	name                  TEXT NOT NULL,
	legal_name            TEXT NOT NULL DEFAULT '',
	address               TEXT NOT NULL DEFAULT '',
	email                 TEXT NOT NULL DEFAULT '',
	phone                 TEXT NOT NULL DEFAULT '',
	website               TEXT NOT NULL DEFAULT '',
	tax_id                TEXT NOT NULL DEFAULT '',
	registration_id       TEXT NOT NULL DEFAULT '',
	logo                  BLOB,
	accent_color          TEXT NOT NULL DEFAULT '#4338ca',
	default_currency      TEXT NOT NULL DEFAULT 'EUR',
	default_lang          TEXT NOT NULL DEFAULT 'en',
	default_tax_bp        INTEGER NOT NULL DEFAULT 0,
	invoice_prefix        TEXT NOT NULL DEFAULT 'INV-',
	payment_terms_days    INTEGER NOT NULL DEFAULT 30,
	bank_holder           TEXT NOT NULL DEFAULT '',
	bank_name             TEXT NOT NULL DEFAULT '',
	iban                  TEXT NOT NULL DEFAULT '',
	bic                   TEXT NOT NULL DEFAULT '',
	bank_extra            TEXT NOT NULL DEFAULT '',
	default_notes         TEXT NOT NULL DEFAULT '',
	footer                TEXT NOT NULL DEFAULT '',
	resend_key            BLOB,
	email_from            TEXT NOT NULL DEFAULT '',
	email_reply_to        TEXT NOT NULL DEFAULT '',
	email_bcc             TEXT NOT NULL DEFAULT '',
	stripe_key            BLOB,
	stripe_webhook_secret BLOB,
	stripe_webhook_id     TEXT NOT NULL DEFAULT '',
	stripe_account        TEXT NOT NULL DEFAULT '',
	reminders_enabled     INTEGER NOT NULL DEFAULT 1,
	reminder_days         TEXT NOT NULL DEFAULT '-3,0,7,15,30',
	archived              INTEGER NOT NULL DEFAULT 0,
	created_at            INTEGER NOT NULL
);

CREATE TABLE company_members (
	company_id INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	PRIMARY KEY (company_id, user_id)
);

CREATE TABLE clients (
	id           INTEGER PRIMARY KEY,
	company_id   INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	contact_name TEXT NOT NULL DEFAULT '',
	email        TEXT NOT NULL DEFAULT '',
	cc_emails    TEXT NOT NULL DEFAULT '',
	address      TEXT NOT NULL DEFAULT '',
	tax_id       TEXT NOT NULL DEFAULT '',
	lang         TEXT NOT NULL DEFAULT 'en',
	currency     TEXT NOT NULL DEFAULT '',
	notes        TEXT NOT NULL DEFAULT '',
	archived     INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL
);
CREATE INDEX clients_company ON clients(company_id);

CREATE TABLE number_counters (
	company_id INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
	year       INTEGER NOT NULL,
	last       INTEGER NOT NULL,
	PRIMARY KEY (company_id, year)
);

CREATE TABLE recurring (
	id                INTEGER PRIMARY KEY,
	company_id        INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
	client_id         INTEGER NOT NULL REFERENCES clients(id),
	name              TEXT NOT NULL,
	currency          TEXT NOT NULL,
	interval_unit     TEXT NOT NULL CHECK (interval_unit IN ('week','month','year')),
	interval_count    INTEGER NOT NULL DEFAULT 1,
	anchor_day        INTEGER NOT NULL DEFAULT 1,
	next_run          TEXT NOT NULL,
	end_date          TEXT NOT NULL DEFAULT '',
	remaining         INTEGER NOT NULL DEFAULT -1,
	due_days          INTEGER NOT NULL DEFAULT 30,
	auto_send         INTEGER NOT NULL DEFAULT 1,
	active            INTEGER NOT NULL DEFAULT 1,
	notes             TEXT NOT NULL DEFAULT '',
	last_run_at       INTEGER NOT NULL DEFAULT 0,
	last_error        TEXT NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL
);

CREATE TABLE recurring_lines (
	id           INTEGER PRIMARY KEY,
	recurring_id INTEGER NOT NULL REFERENCES recurring(id) ON DELETE CASCADE,
	position     INTEGER NOT NULL,
	description  TEXT NOT NULL,
	quantity     INTEGER NOT NULL,
	unit_price   INTEGER NOT NULL,
	tax_bp       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE invoices (
	id                INTEGER PRIMARY KEY,
	company_id        INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
	client_id         INTEGER NOT NULL REFERENCES clients(id),
	number            TEXT,
	status            TEXT NOT NULL CHECK (status IN ('draft','open','paid','void')),
	currency          TEXT NOT NULL,
	lang              TEXT NOT NULL,
	issue_date        TEXT NOT NULL,
	due_date          TEXT NOT NULL,
	subtotal          INTEGER NOT NULL DEFAULT 0,
	tax_total         INTEGER NOT NULL DEFAULT 0,
	total             INTEGER NOT NULL DEFAULT 0,
	amount_paid       INTEGER NOT NULL DEFAULT 0,
	notes             TEXT NOT NULL DEFAULT '',
	public_token      TEXT NOT NULL UNIQUE,
	client_snapshot   TEXT NOT NULL DEFAULT '',
	company_snapshot  TEXT NOT NULL DEFAULT '',
	recurring_id      INTEGER NOT NULL DEFAULT 0,
	reminders_enabled INTEGER NOT NULL DEFAULT 1,
	reminders_sent    TEXT NOT NULL DEFAULT '',
	sent_at           INTEGER NOT NULL DEFAULT 0,
	paid_at           INTEGER NOT NULL DEFAULT 0,
	voided_at         INTEGER NOT NULL DEFAULT 0,
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL,
	UNIQUE (company_id, number)
);
CREATE INDEX invoices_company_status ON invoices(company_id, status);

CREATE TABLE invoice_lines (
	id          INTEGER PRIMARY KEY,
	invoice_id  INTEGER NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
	position    INTEGER NOT NULL,
	description TEXT NOT NULL,
	quantity    INTEGER NOT NULL,
	unit_price  INTEGER NOT NULL,
	tax_bp      INTEGER NOT NULL DEFAULT 0,
	amount      INTEGER NOT NULL
);
CREATE INDEX invoice_lines_invoice ON invoice_lines(invoice_id);

CREATE TABLE payments (
	id                INTEGER PRIMARY KEY,
	invoice_id        INTEGER NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
	amount            INTEGER NOT NULL,
	method            TEXT NOT NULL,
	reference         TEXT NOT NULL DEFAULT '',
	paid_on           TEXT NOT NULL,
	stripe_session_id TEXT UNIQUE,
	created_by        INTEGER NOT NULL DEFAULT 0,
	created_at        INTEGER NOT NULL
);
CREATE INDEX payments_invoice ON payments(invoice_id);

CREATE TABLE stripe_sessions (
	id         TEXT PRIMARY KEY,
	invoice_id INTEGER NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
	amount     INTEGER NOT NULL,
	url        TEXT NOT NULL,
	status     TEXT NOT NULL DEFAULT 'open',
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX stripe_sessions_invoice ON stripe_sessions(invoice_id);

CREATE TABLE email_log (
	id          INTEGER PRIMARY KEY,
	company_id  INTEGER NOT NULL,
	invoice_id  INTEGER NOT NULL DEFAULT 0,
	kind        TEXT NOT NULL,
	to_addr     TEXT NOT NULL,
	subject     TEXT NOT NULL,
	status      TEXT NOT NULL,
	provider_id TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
);
CREATE INDEX email_log_invoice ON email_log(invoice_id);

CREATE TABLE audit_log (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL DEFAULT 0,
	company_id INTEGER NOT NULL DEFAULT 0,
	action     TEXT NOT NULL,
	details    TEXT NOT NULL DEFAULT '',
	ip         TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX audit_log_time ON audit_log(created_at);
`,
	// 2 — bank details in e-mails, faster line suggestions
	`
ALTER TABLE companies ADD COLUMN email_bank_details INTEGER NOT NULL DEFAULT 0;
CREATE INDEX clients_email ON clients(email COLLATE NOCASE);
`,
	// 3 — several bank accounts per company, chosen per invoice
	`
CREATE TABLE bank_accounts (
	id         INTEGER PRIMARY KEY,
	company_id INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
	label      TEXT NOT NULL DEFAULT '',
	currency   TEXT NOT NULL,
	holder     TEXT NOT NULL DEFAULT '',
	bank_name  TEXT NOT NULL DEFAULT '',
	iban       TEXT NOT NULL DEFAULT '',
	bic        TEXT NOT NULL DEFAULT '',
	extra      TEXT NOT NULL DEFAULT '',
	position   INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX bank_accounts_company ON bank_accounts(company_id);
INSERT INTO bank_accounts(company_id, currency, holder, bank_name, iban, bic, extra, created_at)
	SELECT id, default_currency, bank_holder, bank_name, iban, bic, bank_extra, created_at
	FROM companies WHERE iban != '' OR bank_extra != '';
-- 0 = automatic (account in the invoice currency), -1 = none, > 0 = that account
ALTER TABLE invoices ADD COLUMN bank_account_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE recurring ADD COLUMN bank_account_id INTEGER NOT NULL DEFAULT 0;
`,
	// 4 — online card payment (Stripe) can be turned off per invoice
	`
ALTER TABLE invoices ADD COLUMN card_payment INTEGER NOT NULL DEFAULT 1;
ALTER TABLE recurring ADD COLUMN card_payment INTEGER NOT NULL DEFAULT 1;
`,
	// 5 — clients shared by every company (company_id stays the creating one)
	`
ALTER TABLE clients ADD COLUMN shared INTEGER NOT NULL DEFAULT 0;
CREATE INDEX clients_shared ON clients(shared) WHERE shared = 1;
`,
	// 6 — saved cards charged off-session (per company: each has its own Stripe account)
	`
CREATE TABLE stripe_customers (
	company_id  INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
	client_id   INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
	customer_id TEXT NOT NULL,
	card_token  TEXT NOT NULL UNIQUE,
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (company_id, client_id)
);
CREATE TABLE client_cards (
	id             INTEGER PRIMARY KEY,
	company_id     INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
	client_id      INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
	payment_method TEXT NOT NULL,
	brand          TEXT NOT NULL DEFAULT '',
	last4          TEXT NOT NULL DEFAULT '',
	exp_month      INTEGER NOT NULL DEFAULT 0,
	exp_year       INTEGER NOT NULL DEFAULT 0,
	is_default     INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	UNIQUE (company_id, payment_method)
);
CREATE INDEX client_cards_client ON client_cards(company_id, client_id);
CREATE TABLE card_setups (
	id         TEXT PRIMARY KEY,
	company_id INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
	client_id  INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
	status     TEXT NOT NULL DEFAULT 'open',
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE card_charges (
	id             INTEGER PRIMARY KEY,
	company_id     INTEGER NOT NULL,
	invoice_id     INTEGER NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
	card_id        INTEGER NOT NULL,
	card_label     TEXT NOT NULL DEFAULT '',
	payment_intent TEXT NOT NULL DEFAULT '',
	amount         INTEGER NOT NULL,
	status         TEXT NOT NULL DEFAULT 'pending',
	error          TEXT NOT NULL DEFAULT '',
	automatic      INTEGER NOT NULL DEFAULT 0,
	created_by     INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL
);
CREATE INDEX card_charges_invoice ON card_charges(invoice_id);
ALTER TABLE recurring ADD COLUMN auto_charge INTEGER NOT NULL DEFAULT 0;
ALTER TABLE invoices ADD COLUMN auto_charge INTEGER NOT NULL DEFAULT 0;
`,
	// 7 — content of sent e-mails, to show them again
	`
ALTER TABLE email_log ADD COLUMN html TEXT NOT NULL DEFAULT '';
ALTER TABLE email_log ADD COLUMN text_body TEXT NOT NULL DEFAULT '';
ALTER TABLE email_log ADD COLUMN attachments TEXT NOT NULL DEFAULT '';
CREATE INDEX email_log_company ON email_log(company_id, id);
`,
	// 8 — number of invoices generated by each schedule (guards "generate now")
	`
ALTER TABLE recurring ADD COLUMN generated INTEGER NOT NULL DEFAULT 0;
UPDATE recurring SET generated = (SELECT COUNT(*) FROM invoices WHERE recurring_id = recurring.id);
-- product & service suggestions removed from the list (until used again)
CREATE TABLE hidden_suggestions (
	company_id  INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
	description TEXT NOT NULL,
	hidden_at   INTEGER NOT NULL,
	PRIMARY KEY (company_id, description)
);
`,
}
