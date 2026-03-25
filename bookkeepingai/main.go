// bookkeepingai is the WhatsApp AI bookkeeper for Datamartics.
// It starts a Gin HTTP server, registers the Twilio inbound webhook, and
// schedules weekly and daily report runs via robfig/cron.
//
// All configuration is loaded from .env via godotenv. Exposes:
//
//	POST /webhook/inbound       — Twilio posts every inbound WhatsApp message
//	POST /admin/trigger-report  — demo shortcut: fire a report immediately
//	GET  /health                — liveness check for ngrok verification
//
// Run:
//
//	go run ./bookkeepingai/main.go
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/db"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/models"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/report"
	"github.com/michaelorina/datamartics/bookkeepingai/internal/webhook"
	"github.com/robfig/cron/v3"
)

func main() {
	// -------------------------------------------------------------------------
	// 1. Load .env
	// -------------------------------------------------------------------------
	if err := godotenv.Load(".env"); err != nil {
		log.Printf("main: .env not loaded (%v) — continuing with shell env", err)
	}
	port := getEnv("PORT", "8080")

	// -------------------------------------------------------------------------
	// 2. Initialise SQLite
	// -------------------------------------------------------------------------
	database, err := db.New("bookkeepingai.db")
	if err != nil {
		log.Fatalf("main: db.New: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			log.Printf("main: db.Close: %v", err)
		}
	}()

	// -------------------------------------------------------------------------
	// 3. Cron scheduler
	//    Weekly: Mondays 04:00 UTC  = 07:00 EAT
	//    Daily:  every day 04:00 UTC = 07:00 EAT
	// -------------------------------------------------------------------------
	c := cron.New()

	_, err = c.AddFunc("0 4 * * 1", func() {
		log.Println("cron: weekly reports starting")
		runScheduledReports(database, models.PlanWeekly)
	})
	if err != nil {
		log.Fatalf("main: cron weekly: %v", err)
	}

	_, err = c.AddFunc("0 4 * * *", func() {
		log.Println("cron: daily reports starting")
		runScheduledReports(database, models.PlanDaily)
	})
	if err != nil {
		log.Fatalf("main: cron daily: %v", err)
	}

	c.Start()
	defer c.Stop()
	log.Println("main: cron started — weekly Mon 07:00 EAT, daily 07:00 EAT")

	// -------------------------------------------------------------------------
	// 4. Gin HTTP server
	// -------------------------------------------------------------------------
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	h := webhook.New(database)
	router.POST("/webhook/inbound", h.InboundMessage)
	router.POST("/admin/trigger-report", h.TriggerReport)
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "bookkeepingai"})
	})

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: router,
	}

	go func() {
		log.Printf("main: bookkeepingai listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("main: ListenAndServe: %v", err)
		}
	}()

	// -------------------------------------------------------------------------
	// 5. Graceful shutdown
	// -------------------------------------------------------------------------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("main: shutdown signal received")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("main: forced shutdown: %v", err)
	}
	log.Println("main: stopped cleanly")
}

// runScheduledReports fetches all ACTIVE users, filters by plan, and fires
// one goroutine per user. One slow mistral call never blocks the others.
func runScheduledReports(database *db.DB, planType string) {
	users, err := database.GetAllActiveUsers()
	if err != nil {
		log.Printf("runScheduledReports(%s): GetAllActiveUsers: %v", planType, err)
		return
	}

	now := time.Now()
	periodEnd := now
	periodStart := reportPeriodStart(planType, now)

	count := 0
	for _, u := range users {
		if u.Plan != planType {
			continue
		}
		u := u // capture loop variable
		go func() {
			ctx := context.Background()
			if err := report.GenerateAndSend(ctx, database, u.PhoneNumber, periodStart, periodEnd); err != nil {
				log.Printf("runScheduledReports: GenerateAndSend %s: %v", u.PhoneNumber, err)
			}
		}()
		count++
	}
	log.Printf("runScheduledReports(%s): fired %d reports", planType, count)
}

// reportPeriodStart returns the window start for the given plan type.
// Weekly → 7 days ago, Daily → 24 hours ago.
func reportPeriodStart(planType string, now time.Time) time.Time {
	if planType == models.PlanWeekly {
		return now.AddDate(0, 0, -7)
	}
	return now.Add(-24 * time.Hour)
}

// getEnv returns the env var value or the fallback if unset.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}