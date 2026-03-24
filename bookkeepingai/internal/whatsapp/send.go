// Package whatsapp handles all outbound WhatsApp messaging via Twilio.
// This is a pure transport layer — no business logic lives here.
// It is goroutine-safe and must be called from any number of concurrent goroutines.
package whatsapp

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	twilioApi "github.com/twilio/twilio-go/rest/api/v2010"

	twilio "github.com/twilio/twilio-go"
)

// defaultTimeout is applied to every Twilio API call that does not already
// carry a shorter deadline on the incoming context.
const defaultTimeout = 10 * time.Second

// client is the singleton Twilio REST client.
// Initialised exactly once via clientOnce.
var (
	client     *twilio.RestClient
	clientOnce sync.Once
	clientErr  error
)

// initClient initialises the Twilio REST client from environment variables.
// Called exactly once, lazily, on the first SendWhatsApp call.
// Returns an error if the required env vars are missing.
func initClient() error {
	clientOnce.Do(func() {
		sid := os.Getenv("TWILIO_ACCOUNT_SID")
		token := os.Getenv("TWILIO_AUTH_TOKEN")

		if sid == "" || token == "" {
			clientErr = fmt.Errorf(
				"whatsapp: TWILIO_ACCOUNT_SID or TWILIO_AUTH_TOKEN is not set",
			)
			return
		}

		client = twilio.NewRestClientWithParams(twilio.ClientParams{
			Username: sid,
			Password: token,
		})
	})

	return clientErr
}

// SendWhatsApp sends a WhatsApp message to the given recipient number.
//
// Parameters:
//   - ctx:  caller-supplied context; a defaultTimeout deadline is added if the
//     context has no deadline of its own.
//   - to:   recipient in E.164 WhatsApp format, e.g. "whatsapp:+254712345678"
//   - body: message text; max 1600 chars (Twilio hard limit)
//
// Returns an error if the Twilio API call fails or the context is cancelled.
// All errors are logged before being returned — callers may handle or ignore
// the returned error, but they should not log it a second time.
func SendWhatsApp(ctx context.Context, to, body string) error {
	// Initialise client on first call (goroutine-safe).
	if err := initClient(); err != nil {
		log.Printf("[whatsapp] client init failed: %v", err)
		return err
	}

	// Enforce a local deadline if the caller did not set one.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}

	from := os.Getenv("TWILIO_WHATSAPP_FROM")
	if from == "" {
		err := fmt.Errorf("whatsapp: TWILIO_WHATSAPP_FROM is not set")
		log.Printf("[whatsapp] %v", err)
		return err
	}

	// Construct the Twilio message parameters.
	params := &twilioApi.CreateMessageParams{}
	params.SetFrom(from)
	params.SetTo(to)
	params.SetBody(body)

	// Run the Twilio API call in a separate goroutine so we can honour
	// context cancellation: if the context expires first we return
	// immediately without leaking the goroutine (the goroutine will
	// complete in the background and its result will be discarded).
	type result struct {
		sid string
		err error
	}

	ch := make(chan result, 1)

	go func() {
		resp, err := client.Api.CreateMessage(params)
		if err != nil {
			ch <- result{err: fmt.Errorf("whatsapp: twilio API error: %w", err)}
			return
		}

		sid := ""
		if resp.Sid != nil {
			sid = *resp.Sid
		}
		ch <- result{sid: sid}
	}()

	select {
	case <-ctx.Done():
		err := fmt.Errorf("whatsapp: send to %s cancelled: %w", to, ctx.Err())
		log.Printf("[whatsapp] %v", err)
		return err

	case r := <-ch:
		if r.err != nil {
			log.Printf("[whatsapp] failed to send to %s: %v", to, r.err)
			return r.err
		}
		log.Printf("[whatsapp] sent to %s — SID: %s", to, r.sid)
		return nil
	}
}

// SendWhatsAppAsync fires SendWhatsApp in a new goroutine.
// Use this for non-critical notifications (e.g. weekly report delivery)
// where the caller does not need to wait for delivery confirmation.
//
// The errCallback function is called with any error that occurs.
// Pass nil if you do not need error handling.
func SendWhatsAppAsync(ctx context.Context, to, body string, errCallback func(error)) {
	go func() {
		if err := SendWhatsApp(ctx, to, body); err != nil {
			if errCallback != nil {
				errCallback(err)
			}
		}
	}()
}