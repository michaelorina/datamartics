package models

import "time"

type User struct {
	PhoneNumber  string    `db:"phone_number"`
	State        string    `db:"state"`
	LanguagePref string    `db:"language_pref"`
	BusinessType string    `db:"business_type"`
	Plan         string    `db:"plan"`
	ConsentAt    time.Time `db:"consent_at"`
	TrialEndsAt  time.Time `db:"trial_ends_at"`
	IsPaying     bool      `db:"is_paying"`
	ReferralCode string    `db:"referral_code"`
	ReferredBy   string    `db:"referred_by"`
	FreeMonths   int       `db:"free_months"`
	CreatedAt    time.Time `db:"created_at"`
}

type Transaction struct {
	ID          int64     `db:"id"`
	PhoneNumber string    `db:"phone_number"`
	RawText     string    `db:"raw_text"`
	Item        string    `db:"item"`
	Quantity    float64   `db:"quantity"`
	Amount      float64   `db:"amount"`
	Type        string    `db:"type"`
	Currency    string    `db:"currency"`
	CreatedAt   time.Time `db:"created_at"`
}

type Report struct {
	ID          int64     `db:"id"`
	PhoneNumber string    `db:"phone_number"`
	PeriodStart time.Time `db:"period_start"`
	PeriodEnd   time.Time `db:"period_end"`
	ReportText  string    `db:"report_text"`
	SentAt      time.Time `db:"sent_at"`
	Type        string    `db:"type"`
}

type ParsedTransaction struct {
	Item     string  `json:"item"`
	Quantity float64 `json:"quantity"`
	Amount   float64 `json:"amount"`
	Type     string  `json:"type"`
	Currency string  `json:"currency"`
}

const (
	StateNew              = "NEW"
	StateAwaitingConsent  = "AWAITING_CONSENT"
	StateAwaitingLanguage = "AWAITING_LANGUAGE"
	StateAwaitingBusiness = "AWAITING_BUSINESS"
	StateAwaitingPlan     = "AWAITING_PLAN"
	StateActive           = "ACTIVE"
	StateStopped          = "STOPPED"
)

const (
	PlanWeekly = "WEEKLY"
	PlanDaily  = "DAILY"
)

const (
	LangEnglish = "english"
	LangSwahili = "swahili"
	LangMix     = "mix"
)

const (
	TypeSale    = "sale"
	TypeExpense = "expense"
	TypeStock   = "stock"
	TypeRefund  = "refund"
	TypeUnknown = "unknown"
)