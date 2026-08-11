package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"icloud-api/internal/testimap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	service, err := testimap.NewService(testimap.ServiceConfig{
		IMAPAddr:     environment("TEST_IMAP_ADDR", "127.0.0.1:1993"),
		ControlAddr:  environment("TEST_IMAP_CONTROL_ADDR", "127.0.0.1:8081"),
		ServerName:   environment("TEST_IMAP_SERVER_NAME", "localhost"),
		CAFile:       environment("TEST_IMAP_CA_FILE", "data/test-imap-ca.pem"),
		ControlToken: environment("TEST_IMAP_CONTROL_TOKEN", "icloud-api-test-control-token"),
		Logger:       logger,
	})
	if err != nil {
		return err
	}
	if enabled, err := environmentBool("TEST_IMAP_SEED_DEFAULT", true); err != nil {
		return err
	} else if enabled {
		if err := seedDefaultAccount(service.Backend()); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return service.Run(ctx)
}

func seedDefaultAccount(backend *testimap.Backend) error {
	account, err := backend.CreateAccount(
		environment("TEST_IMAP_DEFAULT_USERNAME", "ui-test@icloud.test"),
		environment("TEST_IMAP_DEFAULT_PASSWORD", "ui-test-password"),
		environment("TEST_IMAP_DEFAULT_FORWARD_ADDRESS", "ui-test@icloud.test"),
	)
	if err != nil {
		return fmt.Errorf("create default test IMAP account: %w", err)
	}
	alias := environment("TEST_IMAP_DEFAULT_ALIAS", "alias@icloud.test")
	message, _, err := testimap.PresetMessage("verification-code", alias, time.Now())
	if err != nil {
		return fmt.Errorf("prepare default test message: %w", err)
	}
	raw, err := testimap.RenderMessage(message, account.ForwardAddress, time.Now())
	if err != nil {
		return fmt.Errorf("render default test message: %w", err)
	}
	if _, err := backend.AddMessage(account.ID, raw, time.Now(), false); err != nil {
		return fmt.Errorf("add default test message: %w", err)
	}
	return nil
}

func environment(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func environmentBool(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s is not a boolean: %w", name, err)
	}
	return parsed, nil
}
