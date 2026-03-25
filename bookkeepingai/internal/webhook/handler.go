// Package webhook provides the Gin HTTP handler for inbound Twilio WhatsApp
// messages. It is the single entry point for every message the bot receives.
//
// Routing logic:
//   - ACTIVE users     → parser pipeline (phi3 → SQLite → template reply)
//   - non-ACTIVE users → onboarding state machine
//   - "REPORT"/"RIPOTI" keyword → immediate report for any ACTIVE user
//
// The handler is intentionally thin. All business logic lives in parser,
// onboarding, and report packages.
package webhook

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/db"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/models"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/onboarding"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/parser"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/report"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/whatsapp"
)

// Handler holds dependencies. Constructed once in main.go, reused across
// all requests — goroutine-safe (db.DB uses sqlx connection pool internally).
type Handler struct {
	db *db.DB
}

// New returns a Handler wired to the given DB.
func New(database *db.DB) *Handler {
	return &Handler{db: database}
}

// InboundMessage handles POST /webhook/inbound — registered in Twilio sandbox.
// Twilio sends form-encoded fields; we read From and Body.
//
// We respond HTTP 200 immediately and process async via a goroutine.
// This prevents Twilio from retrying when phi3/mistral are slow.
func (h *Handler) InboundMessage(c *gin.Context) {
	from := strings.TrimSpace(c.PostForm("From")) // "whatsapp:+254712345678"
	body := strings.TrimSpace(c.PostForm("Body"))

	if from == "" {
		log.Printf("webhook: missing From field — ignoring")
		c.Status(http.StatusOK)
		return
	}
	if body == "" {
		log.Printf("webhook: empty Body from %s — ignoring", from)
		c.Status(http.StatusOK)
		return
	}

	c.Status(http.StatusOK)
	go h.processMessage(from, body)
}

// TriggerReport handles POST /admin/trigger-report.
// Fires an immediate report for any phone number — used during demos.
//
// Request JSON: { "phone": "whatsapp:+254712345678" }
func (h *Handler) TriggerReport(c *gin.Context) {
	var req struct {
		Phone string `json:"phone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone field required"})
		return
	}

	now := time.Now()
	periodStart := now.AddDate(0, 0, -7)
	periodEnd := now

	ctx := context.Background()
	go func() {
		if err := report.GenerateAndSend(ctx, h.db, req.Phone, periodStart, periodEnd); err != nil {
			log.Printf("TriggerReport: GenerateAndSend %s: %v", req.Phone, err)
		}
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"status":  "report generation started",
		"phone":   req.Phone,
		"since":   periodStart.Format(time.RFC3339),
		"message": "WhatsApp report will arrive in ~30-60 seconds",
	})
}

// ---------------------------------------------------------------------------
// Internal processing — always runs in a goroutine spawned by InboundMessage.
// ---------------------------------------------------------------------------

func (h *Handler) processMessage(from, body string) {
	// REPORT keyword: intercept before state-based routing.
	if isReportKeyword(body) {
		user, err := h.db.GetUser(from)
		if err != nil && !isNotFound(err) {
			log.Printf("processMessage: GetUser %s: %v", from, err)
			sendReply(from, msgSystemError)
			return
		}
		if user != nil && user.State == models.StateActive {
			now := time.Now()
			ctx := context.Background()
			go func() {
				if err := report.GenerateAndSend(ctx, h.db, from, now.AddDate(0, 0, -7), now); err != nil {
					log.Printf("processMessage: report for %s: %v", from, err)
				}
			}()
			sendReply(from, "📊 Ninaunda ripoti yako... Itafika WhatsApp hivi karibuni!")
			return
		}
		// Not active — fall through to onboarding.
		h.runOnboarding(from, body)
		return
	}

	// Normal routing: check state.
	user, err := h.db.GetUser(from)
	if err != nil && !isNotFound(err) {
		log.Printf("processMessage: GetUser %s: %v", from, err)
		sendReply(from, msgSystemError)
		return
	}

	if user != nil && user.State == models.StateActive {
		h.runParser(from, body)
	} else {
		h.runOnboarding(from, body)
	}
}

// runParser calls phi3 to parse the message into a ParsedTransaction,
// converts it to a models.Transaction, saves it, and sends the confirmation.
func (h *Handler) runParser(from, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	parsed, err := parser.ParseTransaction(ctx, body)
	if err != nil {
		log.Printf("runParser: ParseTransaction %s: %v", from, err)
		sendReply(from, "Samahani, sikuweza kuelewa ujumbe huo. Jaribu tena! 🙏")
		return
	}

	// Convert ParsedTransaction → models.Transaction for storage.
	tx := parsedToTransaction(from, body, parsed)

	if err := h.db.SaveTransaction(tx); err != nil {
		log.Printf("runParser: SaveTransaction %s: %v", from, err)
		sendReply(from, "Imerekodiwa lakini DB ilikuwa na tatizo. Wasiliana nasi. 🙏")
		return
	}

	sendReply(from, buildTransactionConfirmation(tx))
}

// runOnboarding delegates to the onboarding state machine.
func (h *Handler) runOnboarding(from, body string) {
	reply, err := onboarding.HandleOnboarding(from, body, h.db)
	if err != nil {
		log.Printf("runOnboarding: %s: %v", from, err)
		sendReply(from, msgSystemError)
		return
	}
	if reply != "" {
		sendReply(from, reply)
	}
}

// ---------------------------------------------------------------------------
// Conversion helper
// ---------------------------------------------------------------------------

// parsedToTransaction maps a phi3 ParsedTransaction to a models.Transaction
// ready for SaveTransaction. PhoneNumber and RawText come from the webhook;
// all other fields come from the parser output.
func parsedToTransaction(phone, rawText string, p models.ParsedTransaction) *models.Transaction {
	currency := p.Currency
	if currency == "" {
		currency = "KES"
	}
	return &models.Transaction{
		PhoneNumber: phone,
		RawText:     rawText,
		Item:        p.Item,
		Quantity:    p.Quantity,
		Amount:      p.Amount,
		Type:        p.Type,
		Currency:    currency,
	}
}

// ---------------------------------------------------------------------------
// WhatsApp send helper
// ---------------------------------------------------------------------------

// sendReply sends a WhatsApp message async. The context is owned by the
// errCallback so it outlives this helper — defer-cancel here would kill the
// goroutine inside SendWhatsAppAsync before Twilio received the request.
func sendReply(to, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	whatsapp.SendWhatsAppAsync(ctx, to, body, func(err error) {
		defer cancel()
		if err != nil {
			log.Printf("sendReply to %s: %v", to, err)
		}
	})
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

func isReportKeyword(s string) bool {
	norm := strings.ToUpper(strings.TrimSpace(s))
	return norm == "REPORT" || norm == "RIPOTI"
}

// isNotFound reports whether err is sql.ErrNoRows.
func isNotFound(err error) bool {
	return err != nil && err.Error() == "sql: no rows in result set"
}

// buildTransactionConfirmation returns the instant template reply after a
// successful parse+save. No LLM call — template strings only.
func buildTransactionConfirmation(tx *models.Transaction) string {
	if tx.Type == models.TypeSale {
		return fmt.Sprintf("✅ *Mauzo yamerekodiwa!*\n_%s_ — KES %.2f", tx.Item, tx.Amount)
	}
	return fmt.Sprintf("✅ *Gharama imerekodiwa!*\n_%s_ — KES %.2f", tx.Item, tx.Amount)
}

const msgSystemError = "Samahani, hitilafu ya mfumo. Jaribu tena baadaye. 🙏"