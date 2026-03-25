// Package report generates weekly business intelligence reports for traders
// using the mistral model via Ollama. This is the slow path — called once per
// user per report cycle, never in the real-time reply path.
//
// Pipeline:
//
//	GenerateAndSend()
//	  └── fetchTransactions()  ← GetTransactionsSince from db/sqlite.go
//	        └── buildPrompt()  ← format transaction data for mistral
//	              └── callOllama()  ← HTTP POST, goroutine+select, 60s timeout
//	                    └── saveReport()  ← SaveReport(*models.Report)
//	                          └── whatsapp.SendWhatsAppAsync()  ← fire-and-forget
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/michaelorina/datamartics/bookkeepingai/internal/db"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/models"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/whatsapp"
)

// ollamaReportTimeout is generous — mistral generating a narrative report
// is significantly slower than phi3 parsing a single message.
const ollamaReportTimeout = 60 * time.Second

// reportTypeWeekly identifies this report variant in the reports table.
const reportTypeWeekly = "weekly"

// ollamaURL reads OLLAMA_URL from the environment at call time.
// No init() — keeps the package side-effect-free.
func ollamaURL() string {
	u := os.Getenv("OLLAMA_URL")
	if u == "" {
		return "http://localhost:11434"
	}
	return u
}

// ollamaRequest is the JSON body sent to POST /api/generate.
type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// ollamaResponse is the envelope returned when stream: false.
type ollamaResponse struct {
	Response string `json:"response"`
	Error    string `json:"error,omitempty"`
}

// reportResult carries the mistral output (or an error) across a goroutine
// boundary via a buffered channel.
type reportResult struct {
	text string
	err  error
}

// GenerateAndSend is the public entry point called by the cron scheduler and
// the /admin/trigger-report handler.
//
// It:
//  1. Fetches all transactions for the user since periodStart.
//  2. Builds a structured prompt with the transaction data.
//  3. Calls mistral via Ollama (goroutine + select, 60s timeout).
//  4. Saves the report to the reports table via db.SaveReport.
//  5. Fires a WhatsApp message via SendWhatsAppAsync (non-blocking).
//
// The context governs the DB calls. The Ollama call gets its own 60s
// sub-context — mistral needs headroom that the caller's context may not have.
func GenerateAndSend(ctx context.Context, database *db.DB, phone string, periodStart, periodEnd time.Time) error {
	// ── 1. Fetch transactions ────────────────────────────────────────────────
	transactions, err := fetchTransactions(database, phone, periodStart)
	if err != nil {
		return fmt.Errorf("report: fetch transactions: %w", err)
	}

	if len(transactions) == 0 {
		// Nothing to report — send a polite nudge instead of an empty report.
		msg := "Datamartics Weekly Update\n\nHabari! No transactions were recorded this week. " +
			"Send your sales and expenses via WhatsApp so we can track your business for you. " +
			"Tutaonana!"
		whatsapp.SendWhatsAppAsync(ctx, phone, msg, func(e error) {
			log.Printf("report: nudge send failed for %s: %v", phone, e)
		})
		return nil
	}

	// ── 2. Build prompt ──────────────────────────────────────────────────────
	prompt := buildPrompt(phone, periodStart, periodEnd, transactions)

	// ── 3. Call mistral (goroutine + select, 60s timeout) ────────────────────
	reportText, err := callOllama(ctx, prompt)
	if err != nil {
		return fmt.Errorf("report: ollama call: %w", err)
	}

	// ── 4. Save report ───────────────────────────────────────────────────────
	if err := saveReport(database, phone, periodStart, periodEnd, reportText); err != nil {
		// Non-fatal: report generated successfully. Log and continue to delivery.
		log.Printf("report: warn: failed to save report for %s: %v", phone, err)
	}

	// ── 5. Deliver via WhatsApp (fire-and-forget) ────────────────────────────
	whatsapp.SendWhatsAppAsync(ctx, phone, reportText, func(e error) {
		log.Printf("report: delivery failed for %s: %v", phone, e)
	})

	return nil
}

// fetchTransactions returns all transactions for phone recorded since
// periodStart. Uses GetTransactionsSince — the method available on *db.DB.
func fetchTransactions(database *db.DB, phone string, since time.Time) ([]models.Transaction, error) {
	transactions, err := database.GetTransactionsSince(phone, since)
	if err != nil {
		return nil, fmt.Errorf("fetchTransactions: %w", err)
	}
	return transactions, nil
}

// buildPrompt constructs the mistral prompt from the transaction list.
// Deliberately structured so mistral produces a rich, human-readable report
// suitable for a non-technical trader receiving it on WhatsApp.
//
// Language: English with occasional Swahili — mirrors a Nairobi bookkeeper.
func buildPrompt(phone string, start, end time.Time, txns []models.Transaction) string {
	var totalRevenue, totalExpenses float64
	var salesLines, expenseLines strings.Builder

	for _, t := range txns {
		switch t.Type {
		case models.TypeSale:
			totalRevenue += t.Amount
			fmt.Fprintf(&salesLines, "  - %s: qty %.0f @ %s %.2f (recorded %s)\n",
				t.Item, t.Quantity, t.Currency, t.Amount,
				t.CreatedAt.Format("Mon 2 Jan, 15:04"))
		case models.TypeExpense:
			totalExpenses += t.Amount
			fmt.Fprintf(&expenseLines, "  - %s: %s %.2f (recorded %s)\n",
				t.Item, t.Currency, t.Amount,
				t.CreatedAt.Format("Mon 2 Jan, 15:04"))
		}
	}

	netProfit := totalRevenue - totalExpenses
	period := fmt.Sprintf("%s to %s",
		start.Format("2 January 2006"),
		end.Format("2 January 2006"))

	var b strings.Builder
	fmt.Fprintf(&b, `You are Datamartics, an AI bookkeeper serving small business owners in Nairobi, Kenya.
Generate a weekly business report for a trader. The report will be delivered via WhatsApp.

CONSTRAINTS:
- Maximum 400 words.
- Use plain text only. No markdown, no asterisks, no bullet symbols.
- Warm, professional tone. A Nairobi market trader should feel spoken to, not lectured.
- Include occasional Swahili words where natural (e.g. "Hongera!", "Biashara yako inaendelea vizuri").
- Begin with a friendly greeting and the period covered.
- State total revenue, total expenses, and net profit clearly in KES.
- Name the top 2-3 best-selling items by revenue.
- Flag any expense spike with a brief comment if relevant.
- Close with one concrete, actionable recommendation for the coming week.
- Do NOT invent any figures. Only use the data provided below.

REPORT PERIOD: %s
TRADER: %s

SALES TRANSACTIONS:
%s
TOTAL REVENUE: KES %.2f

EXPENSE TRANSACTIONS:
%s
TOTAL EXPENSES: KES %.2f

NET PROFIT: KES %.2f

Write the report now:`,
		period, phone,
		salesLines.String(), totalRevenue,
		expenseLines.String(), totalExpenses,
		netProfit,
	)

	return b.String()
}

// callOllama POSTs to Ollama /api/generate using mistral. The HTTP call runs
// inside a goroutine; the result travels via a buffered channel of size 1.
// A select races the channel against the context deadline — the caller never
// blocks indefinitely.
func callOllama(ctx context.Context, prompt string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, ollamaReportTimeout)
	defer cancel()

	ch := make(chan reportResult, 1) // buffered — goroutine never leaks on timeout

	go func() {
		text, err := doOllamaHTTP(callCtx, prompt)
		ch <- reportResult{text: text, err: err}
	}()

	select {
	case res := <-ch:
		return res.text, res.err
	case <-callCtx.Done():
		return "", fmt.Errorf("callOllama: mistral timed out: %w", callCtx.Err())
	}
}

// doOllamaHTTP performs the actual HTTP round-trip to Ollama.
// Separated from callOllama so it runs cleanly inside the goroutine.
func doOllamaHTTP(ctx context.Context, prompt string) (string, error) {
	reqBody := ollamaRequest{
		Model:  "mistral",
		Prompt: prompt,
		Stream: false, // single JSON envelope — required for one-shot unmarshal
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("doOllamaHTTP: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ollamaURL()+"/api/generate", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("doOllamaHTTP: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{} // context cancellation governs the deadline
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("doOllamaHTTP: http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("doOllamaHTTP: ollama returned %d: %s", resp.StatusCode, string(body))
	}

	var envelope ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return "", fmt.Errorf("doOllamaHTTP: decode response: %w", err)
	}

	if envelope.Error != "" {
		return "", fmt.Errorf("doOllamaHTTP: ollama error: %s", envelope.Error)
	}

	reportText := strings.TrimSpace(envelope.Response)
	if reportText == "" {
		return "", fmt.Errorf("doOllamaHTTP: mistral returned empty response")
	}

	return reportText, nil
}

// saveReport persists the generated report to the reports table.
// Uses *models.Report with the exact field names from types.go:
// ReportText (not Content), SentAt (not CreatedAt), Type.
func saveReport(database *db.DB, phone string, start, end time.Time, text string) error {
	r := &models.Report{
		PhoneNumber: phone,
		PeriodStart: start,
		PeriodEnd:   end,
		ReportText:  text,
		SentAt:      time.Now(),
		Type:        reportTypeWeekly,
	}
	return database.SaveReport(r)
}