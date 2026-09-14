package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/BoniLuan/relay/internal/storage"
)

type tokenAdmin interface {
	IssueClientToken(context.Context, string) (storage.ClientToken, string, error)
	ListClientTokens(context.Context, string) ([]storage.ClientToken, error)
	RevokeClientToken(context.Context, string, string) error
}

var tokenIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func runTokenCommand(ctx context.Context, db tokenAdmin, args []string, out io.Writer) error {
	if len(args) < 2 || !tokenIDPattern.MatchString(args[1]) {
		return errors.New("token commands require a lowercase client UUID")
	}
	command, client := args[0], args[1]
	if command == "revoke-client-token" {
		if len(args) != 3 || !tokenIDPattern.MatchString(args[2]) {
			return errors.New("usage: relay revoke-client-token CLIENT_ID TOKEN_ID")
		}
	} else if len(args) != 2 {
		return errors.New("usage: relay [issue-client-token|list-client-tokens] CLIENT_ID")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	switch command {
	case "issue-client-token":
		meta, token, err := db.IssueClientToken(ctx, client)
		if err != nil {
			return tokenCommandError(err)
		}
		// One-time administrative output, not structured application logging.
		if _, err = fmt.Fprintf(out, "client_id=%s\ntoken_id=%s\ntoken=%s\n", client, meta.ID, token); err != nil {
			return errors.New("token output failed; inspect metadata and revoke the undisclosed token before retrying")
		}
	case "list-client-tokens":
		tokens, err := db.ListClientTokens(ctx, client)
		if err != nil {
			return tokenCommandError(err)
		}
		if err = json.NewEncoder(out).Encode(tokens); err != nil {
			return errors.New("token metadata output failed")
		}
	case "revoke-client-token":
		if err := db.RevokeClientToken(ctx, client, args[2]); err != nil {
			return tokenCommandError(err)
		}
		if _, err := fmt.Fprintln(out, "token revoked"); err != nil {
			return errors.New("revocation output failed; retry the same command")
		}
	default:
		return errors.New("unknown token command")
	}
	return nil
}

func tokenCommandError(err error) error {
	switch {
	case errors.Is(err, storage.ErrTokenLimit):
		return errors.New("two active tokens already exist; inspect metadata and revoke an unused token")
	case errors.Is(err, storage.ErrNotFound):
		return errors.New("client or client-owned token not found")
	default:
		return errors.New("token operation failed; inspect token metadata before retrying")
	}
}
