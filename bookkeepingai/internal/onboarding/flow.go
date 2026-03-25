// Package onboarding drives the WhatsApp onboarding state machine for
// bookkeepingai. Every inbound message from a non-ACTIVE user is routed here.
// No LLM calls are made in this file — all replies are static template strings.
//
// State transitions:
//
//	NEW / not found → AWAITING_CONSENT → AWAITING_LANGUAGE →
//	AWAITING_BUSINESS → AWAITING_PLAN → ACTIVE
package onboarding

import (
	"fmt"
	"strings"

	"github.com/michaelorina/datamartics/bookkeepingai/internal/db"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/models"
)

// HandleOnboarding is the single exported entry point for the onboarding flow.
// It reads the user's current state from the DB, advances the state machine,
// persists the new state, and returns the reply message to send back.
//
// ACTIVE users must never be routed here — the webhook handler guards that.
// If one arrives, we return an empty string and a descriptive error.
func HandleOnboarding(phone, msg string, database *db.DB) (string, error) {
	user, err := getOrCreateUser(phone, database)
	if err != nil {
		return "", fmt.Errorf("onboarding: get/create user %s: %w", phone, err)
	}

	switch user.State {
	case models.StateNew:
		return handleNew(phone, database)
	case models.StateAwaitingConsent:
		return handleAwaitingConsent(phone, msg, database)
	case models.StateAwaitingLanguage:
		return handleAwaitingLanguage(phone, msg, database)
	case models.StateAwaitingBusiness:
		return handleAwaitingBusiness(phone, msg, database)
	case models.StateAwaitingPlan:
		return handleAwaitingPlan(phone, msg, user, database)
	case models.StateActive:
		return "", fmt.Errorf("onboarding: ACTIVE user %s routed to onboarding — bug in webhook handler", phone)
	default:
		return "", fmt.Errorf("onboarding: unknown state %q for user %s", user.State, phone)
	}
}

// ---------------------------------------------------------------------------
// State handlers — one function per state, single responsibility.
// Pattern: validate input → update DB → return reply string.
// ---------------------------------------------------------------------------

// handleNew fires for brand-new users. Any first message triggers the welcome.
func handleNew(phone string, database *db.DB) (string, error) {
	if err := database.UpdateUserState(phone, models.StateAwaitingConsent); err != nil {
		return "", fmt.Errorf("handleNew: %w", err)
	}
	return msgWelcome, nil
}

// handleAwaitingConsent expects "yes" / "ndio". Any other input re-prompts.
func handleAwaitingConsent(phone, msg string, database *db.DB) (string, error) {
	norm := normalise(msg)
	if norm != "yes" && norm != "ndio" {
		return msgConsentRetry, nil
	}
	if err := database.UpdateUserState(phone, models.StateAwaitingLanguage); err != nil {
		return "", fmt.Errorf("handleAwaitingConsent: %w", err)
	}
	return msgLanguagePrompt, nil
}

// handleAwaitingLanguage expects "1" / "english" / "2" / "swahili".
func handleAwaitingLanguage(phone, msg string, database *db.DB) (string, error) {
	norm := normalise(msg)

	var lang, reply string
	switch norm {
	case "1", "english":
		lang = models.LangEnglish
		reply = msgBusinessPromptEN
	case "2", "swahili":
		lang = models.LangSwahili
		reply = msgBusinessPromptSW
	default:
		return msgLanguageRetry, nil
	}

	if err := database.UpdateUserLanguage(phone, lang); err != nil {
		return "", fmt.Errorf("handleAwaitingLanguage: set language: %w", err)
	}
	if err := database.UpdateUserState(phone, models.StateAwaitingBusiness); err != nil {
		return "", fmt.Errorf("handleAwaitingLanguage: set state: %w", err)
	}
	return reply, nil
}

// handleAwaitingBusiness accepts any non-empty text as the business name/type.
func handleAwaitingBusiness(phone, msg string, database *db.DB) (string, error) {
	trimmed := strings.TrimSpace(msg)
	if trimmed == "" {
		return msgBusinessRetry, nil
	}
	if err := database.UpdateUserBusiness(phone, trimmed); err != nil {
		return "", fmt.Errorf("handleAwaitingBusiness: set business: %w", err)
	}
	if err := database.UpdateUserState(phone, models.StateAwaitingPlan); err != nil {
		return "", fmt.Errorf("handleAwaitingBusiness: set state: %w", err)
	}
	return msgPlanPromptEN, nil
}

// handleAwaitingPlan expects "1" / "weekly" / "2" / "daily".
// user is passed in so we can personalise the activation message.
func handleAwaitingPlan(phone, msg string, user *models.User, database *db.DB) (string, error) {
	norm := normalise(msg)

	var plan string
	switch norm {
	case "1", "weekly":
		plan = models.PlanWeekly
	case "2", "daily":
		plan = models.PlanDaily
	default:
		return msgPlanRetry, nil
	}

	if err := database.UpdateUserPlan(phone, plan); err != nil {
		return "", fmt.Errorf("handleAwaitingPlan: set plan: %w", err)
	}
	if err := database.UpdateUserState(phone, models.StateActive); err != nil {
		return "", fmt.Errorf("handleAwaitingPlan: set state: %w", err)
	}

	return buildWelcomeActive(user.BusinessType, plan, user.LanguagePref), nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// getOrCreateUser fetches the user from the DB. If not found (sqlx returns
// sql.ErrNoRows wrapped as "sql: no rows in result set"), it creates a new
// record and returns it. Any other error surfaces to the caller.
func getOrCreateUser(phone string, database *db.DB) (*models.User, error) {
	user, err := database.GetUser(phone)
	if err == nil {
		return user, nil
	}
	if !isNotFound(err) {
		return nil, fmt.Errorf("getOrCreateUser: GetUser: %w", err)
	}
	newUser, err := database.CreateUser(phone)
	if err != nil {
		return nil, fmt.Errorf("getOrCreateUser: CreateUser: %w", err)
	}
	return newUser, nil
}

// isNotFound reports whether err is sql.ErrNoRows from database/sql.
// We match the string to avoid importing database/sql here.
func isNotFound(err error) bool {
	return err != nil && err.Error() == "sql: no rows in result set"
}

// normalise lowercases and trims whitespace so "YES", " Ndio ", "1 " all match.
func normalise(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// buildWelcomeActive constructs the activation confirmation.
func buildWelcomeActive(businessType, plan, lang string) string {
	planLabel := strings.ToLower(plan) // "WEEKLY" → "weekly"
	if lang == models.LangSwahili {
		return fmt.Sprintf(msgActiveTemplateSW, businessType, planLabel)
	}
	return fmt.Sprintf(msgActiveTemplateEN, businessType, planLabel)
}

// ---------------------------------------------------------------------------
// Template strings — all bot copy lives here, zero LLM calls.
// ---------------------------------------------------------------------------

const msgWelcome = `🌟 *Karibu Datamartics!* 🌟

I'm your AI bookkeeper — I help Nairobi traders track sales and expenses right here on WhatsApp.

✅ No app to download
✅ Works in English, Swahili, or Sheng
✅ Weekly profit & loss report straight to your phone

To get started, I need your consent to store your business records on this platform.

Do you agree? Reply *YES* or *NDIO* to continue.`

const msgConsentRetry = `Samahani, sikuelewa. 🙏

Please reply *YES* to agree and continue, or *NDIO* if you prefer Swahili.`

const msgLanguagePrompt = `Asante! ✅

Which language would you like to use?

Reply:
*1* — English
*2* — Swahili`

const msgLanguageRetry = `Tafadhali chagua:

*1* — English
*2* — Swahili`

const msgBusinessPromptEN = `Great choice! 👍

What is your business name or type?
_(e.g. "Mama Pima Chips", "Hardware shop", "Mitumba stall")_`

const msgBusinessPromptSW = `Sawa kabisa! 👍

Biashara yako inaitwa nini, au ni aina gani?
_(mfano: "Chips za Mama Pima", "Duka la vifaa", "Stendi ya mitumba")_`

const msgBusinessRetry = `Tafadhali andika jina au aina ya biashara yako. 🙏`

const msgPlanPromptEN = `Almost done! 📊

How often would you like your profit & loss report?

Reply:
*1* — Weekly _(every Monday morning)_
*2* — Daily _(every morning)_`

const msgPlanRetry = `Tafadhali jibu *1* (kila wiki) au *2* (kila siku).`

// msgActiveTemplateEN and msgActiveTemplateSW take (businessType, planLabel).
const msgActiveTemplateEN = `🎉 *You're all set, %s!*

Your *%s* report is scheduled. Now start tracking — just send me a message like:

  _"Sold 3 sukuma @ 20 each"_
  _"Spent 500 on tomatoes"_
  _"Sold 10 mandazi, got 150"_

I understand English, Swahili, and Sheng. Send any sale or expense and I'll record it instantly. 📒

Type *REPORT* any time to get your summary now.`

const msgActiveTemplateSW = `🎉 *Umewekwa vizuri, %s!*

Ripoti yako ya *%s* imepangwa. Sasa anza kufuatilia — tuma tu ujumbe kama:

  _"Niliuza sukuma 3 @ shilingi 20 kila moja"_
  _"Nilitumia 500 kwa nyanya"_
  _"Niliuza mandazi 10, nilipata 150"_

Ninaelewa Kiingereza, Kiswahili, na Sheng. Tuma mauzo au gharama yoyote nami nitairekodia papo hapo. 📒

Andika *RIPOTI* wakati wowote upate muhtasari wako sasa.`