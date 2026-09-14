package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BoniLuan/relay/internal/storage"
)

type fakeTokenAdmin struct {
	calls int
	err   error
}

func (f *fakeTokenAdmin) IssueClientToken(context.Context, string) (storage.ClientToken, string, error) {
	f.calls++
	return storage.ClientToken{ID: "issued"}, "private-token", f.err
}
func (f *fakeTokenAdmin) ListClientTokens(context.Context, string) ([]storage.ClientToken, error) {
	f.calls++
	return []storage.ClientToken{}, f.err
}
func (f *fakeTokenAdmin) RevokeClientToken(context.Context, string, string) error {
	f.calls++
	return f.err
}

func TestTokenCommandValidationAndPrivateFailures(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, args := range [][]string{nil, {"issue-client-token"}, {"issue-client-token", "not-an-id"}, {"issue-client-token", id, "extra"}, {"revoke-client-token", id}, {"revoke-client-token", id, "bad"}} {
		db := &fakeTokenAdmin{}
		var out bytes.Buffer
		if err := runTokenCommand(context.Background(), db, args, &out); err == nil || db.calls != 0 || out.Len() != 0 {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
	for _, command := range []string{"issue-client-token", "list-client-tokens", "revoke-client-token"} {
		args := []string{command, id}
		if command == "revoke-client-token" {
			args = append(args, id)
		}
		for _, failure := range []error{errors.New("private-database-detail"), storage.ErrNotFound, storage.ErrTokenLimit} {
			db := &fakeTokenAdmin{err: failure}
			var out bytes.Buffer
			err := runTokenCommand(context.Background(), db, args, &out)
			if err == nil || db.calls != 1 || out.Len() != 0 || strings.Contains(err.Error(), "private") {
				t.Fatal("failed command disclosed sensitive data")
			}
		}
	}
	db := &fakeTokenAdmin{}
	var out bytes.Buffer
	if err := runTokenCommand(context.Background(), db, []string{"issue-client-token", id}, &out); err != nil || !strings.Contains(out.String(), "token=private-token") {
		t.Fatal("successful issuance missing intentional one-time output")
	}
	out.Reset()
	if err := runTokenCommand(context.Background(), db, []string{"list-client-tokens", id}, &out); err != nil || out.String() != "[]\n" {
		t.Fatal("metadata output invalid")
	}
}
