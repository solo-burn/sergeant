package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/slack-go/slack"

	"sergeant/internal/i18n"
	"sergeant/internal/parser"
	"sergeant/internal/shadow"
	"sergeant/internal/slackapi"
)

func main() {
	options := &slog.HandlerOptions{Level: parseLevel(os.Getenv("LOG_LEVEL"))}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, options))
	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func run(logger *slog.Logger) error {
	dbPath := getEnv("SERGEANT_DB_PATH", "./sergeant.db")
	ghostPath := getEnv("SERGEANT_LEDGER_DB_PATH", "./sergeant.ledger.db")
	ledgerOnly := len(os.Args) == 2 && os.Args[1] == "ledger"
	if len(os.Args) > 1 && !ledgerOnly {
		return errors.New("usage: sergeant [ledger]")
	}
	db, err := openDashboardDB(ghostPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	dashboard := &http.Server{
		Addr:              "0.0.0.0:8081",
		Handler:           dashboardHandler(db),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if ledgerOnly {
		return serveHTTP(logger, dashboard)
	}
	botToken := os.Getenv("SLACK_BOT_TOKEN")
	signingSecret := os.Getenv("SLACK_SIGNING_SECRET")
	if botToken == "" || signingSecret == "" {
		return errors.New("SLACK_BOT_TOKEN and SLACK_SIGNING_SECRET are required")
	}
	addr := getEnv("SERGEANT_HTTP_ADDR", ":8080")
	defaultCcy, err := parser.NormalizeCurrency(getEnv("SERGEANT_DEFAULT_CURRENCY", "PLN"))
	if err != nil {
		return fmt.Errorf("SERGEANT_DEFAULT_CURRENCY: %w", err)
	}
	translator := i18n.New(getEnv("SERGEANT_LOCALE", i18n.DefaultLocale))
	logger.Info("locale", "active", translator.Locale(), "available", i18n.Available())

	trial, err := shadow.Open(context.Background(), dbPath, ghostPath, defaultCcy, translator)
	if err != nil {
		return err
	}
	defer func() { _ = trial.Legacy.Close() }()
	replayCtx, stopReplay := context.WithCancel(context.Background())
	replayDone := make(chan struct{})
	go func() {
		defer close(replayDone)
		trial.Run(replayCtx, logger)
	}()
	defer func() {
		stopReplay()
		<-replayDone
	}()

	client := slack.New(botToken)
	authCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	auth, err := client.AuthTestContext(authCtx)
	cancel()
	if err != nil {
		return err
	}
	logger.Info("connected", "team", auth.Team, "bot_user_id", auth.UserID)

	mux := http.NewServeMux()
	handler := &slackapi.Handler{
		SigningSecret: signingSecret,
		BotUserID:     auth.UserID,
		Engine:        trial.Legacy,
		Responder:     slackapi.SlackResponder{Client: client},
		Translator:    translator,
		Logger:        logger,
	}
	mux.Handle("/slack/events", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	err = serveHTTP(logger, srv, dashboard)
	handler.Wait()
	return err
}

func serveHTTP(logger *slog.Logger, servers ...*http.Server) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			logger.Info("listening", "addr", srv.Addr)
			errCh <- srv.ListenAndServe()
		}()
	}

	var err error
	select {
	case err = <-errCh:
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
			err = errors.Join(err, shutdownErr, srv.Close())
		}
	}
	return err
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
