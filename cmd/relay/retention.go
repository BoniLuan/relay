package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"github.com/BoniLuan/relay/internal/storage"
)

type retentionAdmin interface {
	PruneHistory(context.Context, string, int, bool) (storage.RetentionResult, error)
}

func runRetention(ctx context.Context, db retentionAdmin, args []string, out io.Writer) error {
	usage := errors.New("usage: relay prune-history CLIENT_ID [--limit 1..100] [--apply]")
	if len(args) < 1 || !tokenIDPattern.MatchString(args[0]) {
		return usage
	}
	flags := flag.NewFlagSet("prune-history", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	apply := flags.Bool("apply", false, "commit deletion instead of preview")
	limit := flags.Int("limit", storage.MaxRetentionBatch, "maximum events in one batch")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || *limit < 1 || *limit > storage.MaxRetentionBatch {
		return usage
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := db.PruneHistory(ctx, args[0], *limit, *apply)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return errors.New("retention client not found")
		}
		return errors.New("retention operation failed; inspect a fresh preview before retrying")
	}
	if json.NewEncoder(out).Encode(result) != nil {
		return errors.New("retention output failed; deletion may have committed, inspect a fresh preview")
	}
	return nil
}
