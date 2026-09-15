package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BoniLuan/relay/internal/storage"
)

type fakeRetention struct {
	calls, limit int
	apply        bool
	err          error
}

func (f *fakeRetention) PruneHistory(_ context.Context, _ string, limit int, apply bool) (storage.RetentionResult, error) {
	f.calls++
	f.limit = limit
	f.apply = apply
	return storage.RetentionResult{Applied: apply, Selected: 2}, f.err
}
func TestRetentionCommandPreviewValidationAndPrivateFailures(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, args := range [][]string{nil, {"bad"}, {id, "--limit", "0"}, {id, "--limit", "101"}, {id, "--unknown"}, {id, "extra"}} {
		db := &fakeRetention{}
		var out bytes.Buffer
		if err := runRetention(context.Background(), db, args, &out); err == nil || db.calls != 0 || out.Len() != 0 {
			t.Fatal("invalid args accepted")
		}
	}
	for _, apply := range []bool{false, true} {
		args := []string{id, "--limit", "2"}
		if apply {
			args = append(args, "--apply")
		}
		db := &fakeRetention{}
		var out bytes.Buffer
		if err := runRetention(context.Background(), db, args, &out); err != nil || db.apply != apply || db.limit != 2 {
			t.Fatal("preview/apply selection incorrect")
		}
	}
	db := &fakeRetention{err: errors.New("private SQL payload")}
	var out bytes.Buffer
	if err := runRetention(context.Background(), db, []string{id, "--apply"}, &out); err == nil || strings.Contains(err.Error(), "private") || out.Len() != 0 {
		t.Fatal("failure leaked details or emitted success")
	}
}
