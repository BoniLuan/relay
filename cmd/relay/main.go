package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/BoniLuan/relay/internal/httpapi"
	"github.com/BoniLuan/relay/internal/secrets"
	"github.com/BoniLuan/relay/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	command := "api"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command == "keyring-init" {
		if len(os.Args) != 3 {
			return errors.New("usage: relay keyring-init PATH")
		}
		if err := secrets.InitFile(os.Args[2]); err != nil {
			return err
		}
		logger.Info("keyring created; back it up separately from database data")
		return nil
	}
	databaseURL := os.Getenv("RELAY_DATABASE_URL")
	if databaseURL == "" {
		return errors.New("RELAY_DATABASE_URL is required")
	}
	db, err := storage.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	switch command {
	case "migrate":
		migrationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := db.Migrate(migrationCtx); err != nil {
			return errors.New("migration failed")
		}
		logger.Info("migrations applied")
		return nil
	case "create-client":
		if len(os.Args) != 3 {
			return errors.New("usage: relay create-client NAME")
		}
		provisionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		id, token, err := db.ProvisionClient(provisionCtx, os.Args[2])
		if err != nil {
			return errors.New("client creation failed")
		}
		// Explicit administrative output; never include the token in application logs.
		fmt.Printf("client_id=%s\ntoken=%s\n", id, token)
		return nil
	case "register-keyring":
		keyring, err := secrets.LoadFile(os.Getenv("RELAY_KEYRING_FILE"))
		if err != nil {
			return err
		}
		keyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err = db.WithKeyring(keyring).RegisterKeyring(keyCtx); err != nil {
			return errors.New("keyring registration failed")
		}
		logger.Info("keyring registered")
		return nil
	case "api":
		keyring, err := secrets.LoadFile(os.Getenv("RELAY_KEYRING_FILE"))
		if err != nil {
			return err
		}
		keyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err = db.WithKeyring(keyring).CheckKeyring(keyCtx); err != nil {
			return errors.New("registered keyring verification failed")
		}

	default:
		return errors.New("usage: relay [api|migrate|create-client NAME|keyring-init PATH|register-keyring]")
	}
	addr := os.Getenv("RELAY_HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:18081"
	}
	server := &http.Server{
		Addr: addr, Handler: httpapi.NewHandler(db, logger),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	logger.Info("relay API starting", "address", addr)
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}
