package db

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/models"
	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	conn *sqlx.DB
}

func New(path string) (*DB, error) {
	conn, err := sqlx.Connect("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to sqlite: %w", err)
	}
	d := &DB{conn: conn}
	if err := d.init(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *DB) init() error {
	schema := `
	CREATE TABLE IF NOT EXISTS users (
		phone_number  TEXT PRIMARY KEY,
		state         TEXT NOT NULL DEFAULT 'NEW',
		language_pref TEXT NOT NULL DEFAULT '',
		business_type TEXT NOT NULL DEFAULT '',
		plan          TEXT NOT NULL DEFAULT '',
		consent_at    DATETIME,
		trial_ends_at DATETIME,
		is_paying     BOOLEAN NOT NULL DEFAULT 0,
		referral_code TEXT NOT NULL DEFAULT '',
		referred_by   TEXT NOT NULL DEFAULT '',
		free_months   INTEGER NOT NULL DEFAULT 0,
		created_at    DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS transactions (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		phone_number TEXT NOT NULL,
		raw_text     TEXT NOT NULL,
		item         TEXT NOT NULL DEFAULT '',
		quantity     REAL NOT NULL DEFAULT 1,
		amount       REAL NOT NULL DEFAULT 0,
		type         TEXT NOT NULL DEFAULT 'sale',
		currency     TEXT NOT NULL DEFAULT 'KES',
		created_at   DATETIME NOT NULL,
		FOREIGN KEY (phone_number) REFERENCES users(phone_number)
	);

	CREATE TABLE IF NOT EXISTS reports (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		phone_number TEXT NOT NULL,
		period_start DATETIME NOT NULL,
		period_end   DATETIME NOT NULL,
		report_text  TEXT NOT NULL,
		sent_at      DATETIME NOT NULL,
		type         TEXT NOT NULL DEFAULT 'weekly',
		FOREIGN KEY (phone_number) REFERENCES users(phone_number)
	);`

	_, err := d.conn.Exec(schema)
	return err
}

func (d *DB) GetUser(phone string) (*models.User, error) {
	var u models.User
	err := d.conn.Get(&u, "SELECT * FROM users WHERE phone_number = ?", phone)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (d *DB) CreateUser(phone string) (*models.User, error) {
	now := time.Now()
	trialEnds := now.AddDate(0, 1, 0)
	u := &models.User{
		PhoneNumber:  phone,
		State:        models.StateNew,
		ConsentAt:    now,
		TrialEndsAt:  trialEnds,
		CreatedAt:    now,
	}
	_, err := d.conn.Exec(`
		INSERT INTO users (phone_number, state, language_pref, business_type, plan,
		consent_at, trial_ends_at, is_paying, referral_code, referred_by, free_months, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.PhoneNumber, u.State, "", "", "",
		u.ConsentAt, u.TrialEndsAt, false, "", "", 0, u.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (d *DB) UpdateUserState(phone, state string) error {
	_, err := d.conn.Exec("UPDATE users SET state = ? WHERE phone_number = ?", state, phone)
	return err
}

func (d *DB) UpdateUserLanguage(phone, lang string) error {
	_, err := d.conn.Exec("UPDATE users SET language_pref = ? WHERE phone_number = ?", lang, phone)
	return err
}

func (d *DB) UpdateUserBusiness(phone, business string) error {
	_, err := d.conn.Exec("UPDATE users SET business_type = ? WHERE phone_number = ?", business, phone)
	return err
}

func (d *DB) UpdateUserPlan(phone, plan string) error {
	_, err := d.conn.Exec("UPDATE users SET plan = ? WHERE phone_number = ?", plan, phone)
	return err
}

func (d *DB) SaveTransaction(t *models.Transaction) error {
	_, err := d.conn.Exec(`
		INSERT INTO transactions (phone_number, raw_text, item, quantity, amount, type, currency, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.PhoneNumber, t.RawText, t.Item, t.Quantity, t.Amount, t.Type, t.Currency, time.Now(),
	)
	return err
}

func (d *DB) GetTransactionsSince(phone string, since time.Time) ([]models.Transaction, error) {
	var txns []models.Transaction
	err := d.conn.Select(&txns,
		"SELECT * FROM transactions WHERE phone_number = ? AND created_at >= ? ORDER BY created_at ASC",
		phone, since,
	)
	return txns, err
}

func (d *DB) GetAllActiveUsers() ([]models.User, error) {
	var users []models.User
	err := d.conn.Select(&users, "SELECT * FROM users WHERE state = ?", models.StateActive)
	return users, err
}

func (d *DB) SaveReport(r *models.Report) error {
	_, err := d.conn.Exec(`
		INSERT INTO reports (phone_number, period_start, period_end, report_text, sent_at, type)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.PhoneNumber, r.PeriodStart, r.PeriodEnd, r.ReportText, r.SentAt, r.Type,
	)
	return err
}

func (d *DB) Close() error {
    return d.conn.Close()
}