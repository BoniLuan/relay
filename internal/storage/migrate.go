package storage

import (
	"context"
	_ "embed"
	"fmt"
)

//go:embed migrations/001_ingestion.sql
var initialSchema string

//go:embed migrations/002_signing_secrets.sql
var signingSchema string

//go:embed migrations/003_delivery_leases.sql
var leaseSchema string

//go:embed migrations/004_delivery_attempts.sql
var attemptSchema string

const schemaVersion = 4

// Migrate is invoked explicitly by the CLI, never by API startup. The transaction
// and advisory lock make the initial migration atomic and safe to run twice.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7283519401)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY)"); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM schema_migrations").Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	migrations := []string{initialSchema, signingSchema, leaseSchema, attemptSchema}
	for version < schemaVersion {
		if _, err = tx.Exec(ctx, migrations[version]); err != nil {
			return err
		}
		version++
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
