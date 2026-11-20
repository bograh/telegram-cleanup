// Command telegram-cleanup runs a local dashboard for decluttering a Telegram
// account: it lists channels, groups and bots, asks an OpenAI-compatible model
// to categorize them, and lets you leave/mute/archive in bulk.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/bograh/telegram-cleanup/internal/ai"
	"github.com/bograh/telegram-cleanup/internal/store"
	"github.com/bograh/telegram-cleanup/internal/telegram"
	"github.com/bograh/telegram-cleanup/internal/web"
)

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	_ = godotenv.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dataDir := env("DATA_DIR", "data")
	st, err := store.Open(dataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	var missing []string
	appID, _ := strconv.Atoi(env("TELEGRAM_APP_ID", ""))
	appHash := env("TELEGRAM_APP_HASH", "")
	if appID == 0 {
		missing = append(missing, "TELEGRAM_APP_ID")
	}
	if appHash == "" {
		missing = append(missing, "TELEGRAM_APP_HASH")
	}
	var tg *telegram.Client
	if len(missing) == 0 {
		tg = telegram.New(ctx, appID, appHash, filepath.Join(dataDir, "session.json"))
	}

	var an *ai.Analyzer
	if model := env("AI_MODEL", ""); model != "" {
		batch, _ := strconv.Atoi(env("AI_BATCH_SIZE", "25"))
		workers, _ := strconv.Atoi(env("AI_CONCURRENCY", "3"))
		an = ai.New(ai.Config{
			BaseURL:   env("AI_BASE_URL", "https://api.openai.com/v1"),
			APIKey:    env("AI_API_KEY", ""),
			Model:     model,
			JSONMode:  env("AI_JSON_MODE", "true") != "false",
			BatchSize: batch,
			Workers:   workers,
		})
	}

	addr := env("ADDR", "127.0.0.1:8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           web.New(ctx, tg, an, st, missing).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Printf("Telegram Cleanup running at http://%s", addr)
	if an == nil {
		log.Printf("AI disabled: set AI_MODEL (plus AI_BASE_URL / AI_API_KEY) in .env to enable it")
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
