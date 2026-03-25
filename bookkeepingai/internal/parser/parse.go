package parser

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

	"github.com/michaelorina/datamartics/bookkeepingai/internal/models"
)

const (
	defaultParseTimeout = 5 * time.Second
	ollamaModel         = "phi3"
)

// ollamaRequest is the payload sent to the Ollama /api/generate endpoint.
type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"` // false → single JSON response object
}

// ollamaResponse is the subset of the Ollama response we care about.
type ollamaResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
	Error    string `json:"error,omitempty"`
}

// parseResult is the internal channel payload for the goroutine → select handoff.
type parseResult struct {
	tx  models.ParsedTransaction
	err error
}

// buildPrompt constructs the strict JSON-only prompt sent to phi3.
// The model must return a single JSON object and nothing else.
func buildPrompt(rawMessage string) string {
	return fmt.Sprintf(`You are a bookkeeping assistant for African small businesses.
A trader sent the following message (may be English, Swahili, or Sheng):

"%s"

Extract the transaction details and return ONLY a valid JSON object.
No explanation. No markdown. No code fences. Just the raw JSON object.

The JSON must follow this exact schema:
{
  "item":     "<string — name of the product or service sold>",
  "quantity": <number — how many units, default 1 if not stated>,
  "amount":   <number — total amount in KES, required>,
  "type":     "<string — must be exactly 'sale' or 'expense'>",
  "currency": "<string — currency code, almost always KES>"
}

Rules:
- amount is always a number (no currency symbols, no commas).
- type is always lowercase "sale" or "expense".
- If the message describes receiving money or selling something, type is "sale".
- If the message describes spending money or buying something, type is "expense".
- If quantity is not mentioned, set it to 1.
- currency is almost always "KES" unless the message explicitly states otherwise.
- If you cannot parse a valid transaction from the message, return:
  {"error": "could not parse transaction"}

Message: "%s"`, rawMessage, rawMessage)
}

// ParseTransaction sends rawMessage to the local phi3 model via Ollama
// and returns a ParsedTransaction. The caller's context is respected; if it
// has no deadline, a 5-second timeout is added automatically.
//
// The Ollama HTTP call runs inside a goroutine. A select races the result
// channel against ctx.Done() so the caller is never blocked if the context
// expires or is cancelled.
func ParseTransaction(ctx context.Context, rawMessage string) (models.ParsedTransaction, error) {
	// Enforce a deadline on the context if none exists.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultParseTimeout)
		defer cancel()
	}

	resultCh := make(chan parseResult, 1) // buffered: goroutine never leaks

	go func() {
		tx, err := callOllama(ctx, rawMessage)
		resultCh <- parseResult{tx: tx, err: err}
	}()

	select {
	case res := <-resultCh:
		return res.tx, res.err
	case <-ctx.Done():
		err := fmt.Errorf("parser: context cancelled while waiting for phi3: %w", ctx.Err())
		log.Println(err)
		return models.ParsedTransaction{}, err
	}
}

// callOllama performs the actual HTTP POST to the Ollama generate endpoint
// and unmarshals the response into a ParsedTransaction.
func callOllama(ctx context.Context, rawMessage string) (models.ParsedTransaction, error) {
	ollamaURL := strings.TrimRight(os.Getenv("OLLAMA_URL"), "/") + "/api/generate"

	payload := ollamaRequest{
		Model:  ollamaModel,
		Prompt: buildPrompt(rawMessage),
		Stream: false,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		err = fmt.Errorf("parser: failed to marshal ollama request: %w", err)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaURL, bytes.NewReader(body))
	if err != nil {
		err = fmt.Errorf("parser: failed to build http request: %w", err)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		err = fmt.Errorf("parser: ollama http call failed: %w", err)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("parser: ollama returned non-200 status: %d", resp.StatusCode)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		err = fmt.Errorf("parser: failed to read ollama response body: %w", err)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}

	var ollamaResp ollamaResponse
	if err = json.Unmarshal(rawBody, &ollamaResp); err != nil {
		err = fmt.Errorf("parser: failed to unmarshal ollama envelope: %w", err)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}

	if ollamaResp.Error != "" {
		err = fmt.Errorf("parser: ollama returned error: %s", ollamaResp.Error)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}

	return unmarshalTransaction(ollamaResp.Response, rawMessage)
}

// unmarshalTransaction takes the raw JSON string returned by phi3 and
// unmarshals it into a ParsedTransaction. It handles the "could not parse"
// sentinel that the prompt instructs the model to return on failure.
func unmarshalTransaction(jsonStr, originalMessage string) (models.ParsedTransaction, error) {
	// Defensive trim: phi3 occasionally emits leading/trailing whitespace
	// even when told not to. Strip it before attempting to parse.
	jsonStr = strings.TrimSpace(jsonStr)

	// Check for the sentinel error object the prompt asks phi3 to return.
	var errCheck struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &errCheck); err == nil && errCheck.Error != "" {
		err = fmt.Errorf("parser: phi3 could not parse message %q: %s", originalMessage, errCheck.Error)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}

	var tx models.ParsedTransaction
	if err := json.Unmarshal([]byte(jsonStr), &tx); err != nil {
		err = fmt.Errorf("parser: failed to unmarshal ParsedTransaction from phi3 output %q: %w", jsonStr, err)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}

	// Validate required fields.
	if tx.Amount <= 0 {
		err := fmt.Errorf("parser: parsed transaction has zero or negative amount for message %q", originalMessage)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}
	if tx.Type != models.TypeSale && tx.Type != models.TypeExpense {
		err := fmt.Errorf("parser: invalid transaction type %q for message %q", tx.Type, originalMessage)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}
	if tx.Item == "" {
		err := fmt.Errorf("parser: empty item name for message %q", originalMessage)
		log.Println(err)
		return models.ParsedTransaction{}, err
	}
	if tx.Quantity <= 0 {
		tx.Quantity = 1 // default per prompt spec
	}

	return tx, nil
}