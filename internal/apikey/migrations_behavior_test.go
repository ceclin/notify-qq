package apikey

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gofrs/flock"
)

func migrationSource(t *testing.T, extra string) fs.FS {
	t.Helper()
	initial, err := migrationFiles.ReadFile("migrations/00001_create_api_keys.sql")
	if err != nil {
		t.Fatal(err)
	}
	source := fstest.MapFS{"00001_create_api_keys.sql": &fstest.MapFile{Data: initial}}
	if extra != "" {
		source["00002_upgrade.sql"] = &fstest.MapFile{Data: []byte(extra)}
	}
	return source
}

func TestBehaviorMigrationUpgradesPreserveBindingsAndCredentials(t *testing.T) {
	directory := t.TempDir()
	initial, err := Open(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	created, err := initial.Create(t.Context(), "backup")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := initial.Bind(t.Context(), created.PairingCode, "alice"); err != nil {
		t.Fatal(err)
	}
	revoked, err := initial.Create(t.Context(), "revoked")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initial.Revoke(t.Context(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := initial.Create(t.Context(), "pending")
	if err != nil {
		t.Fatal(err)
	}
	if initial.Migrations() != (MigrationState{Version: 1, Applied: 1}) {
		t.Fatal("fresh database did not apply baseline once")
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	source := migrationSource(t, "-- +goose Up\nALTER TABLE api_keys ADD COLUMN note TEXT NOT NULL DEFAULT '';\n-- +goose Down\nALTER TABLE api_keys DROP COLUMN note;")
	upgraded, err := openStore(t.Context(), directory, source)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Migrations() != (MigrationState{Version: 2, Applied: 1}) {
		t.Fatal("existing database did not apply only the pending upgrade")
	}
	owner, valid, err := upgraded.Authenticate(t.Context(), created.APIKey)
	if err != nil || !valid || owner.ID != created.ID || owner.UserOpenID != "alice" {
		t.Fatal("upgrade lost credential or binding")
	}
	if _, valid, err := upgraded.Authenticate(t.Context(), revoked.APIKey); err != nil || valid {
		t.Fatal("upgrade revived a revoked key")
	}
	if _, changed, err := upgraded.Bind(t.Context(), pending.PairingCode, "bob"); err != nil || !changed {
		t.Fatal("upgrade lost pending pairing code")
	}
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(t.Context(), directory, source)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Migrations() != (MigrationState{Version: 2, Applied: 0}) {
		t.Fatal("reopen replayed applied migrations")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if outdated, err := Open(t.Context(), directory); err == nil {
		_ = outdated.Close()
		t.Fatal("older code accepted a newer migration version")
	}
}

func TestBehaviorMigrationFailureRollsBackSchemaDataAndVersion(t *testing.T) {
	directory := t.TempDir()
	initial, err := Open(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	created, err := initial.Create(t.Context(), "backup")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := initial.Bind(t.Context(), created.PairingCode, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	failing := migrationSource(t, "-- +goose Up\nALTER TABLE api_keys ADD COLUMN temporary_probe TEXT;\nUPDATE api_keys SET user_openid='incorrect';\nSELECT missing_column FROM api_keys;\n-- +goose Down\nSELECT 1;")
	if opened, err := openStore(t.Context(), directory, failing); err == nil {
		_ = opened.Close()
		t.Fatal("invalid migration reported success")
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, Filename))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}()
	var columns, version int
	if err := database.QueryRowContext(t.Context(), "SELECT count(*) FROM pragma_table_info('api_keys') WHERE name='temporary_probe'").Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(t.Context(), "SELECT MAX(version_id) FROM goose_db_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || version != 1 {
		t.Fatal("failed migration committed schema or version")
	}
	repaired := migrationSource(t, "-- +goose Up\nALTER TABLE api_keys ADD COLUMN temporary_probe TEXT;\n-- +goose Down\nALTER TABLE api_keys DROP COLUMN temporary_probe;")
	reopened, err := openStore(t.Context(), directory, repaired)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	owner, valid, err := reopened.Authenticate(t.Context(), created.APIKey)
	if err != nil || !valid || owner.UserOpenID != "alice" || reopened.Migrations().Version != 2 {
		t.Fatal("rollback lost data or prevented a corrected retry")
	}
}

func TestBehaviorMigrationLockCancellationAndConcurrentOpening(t *testing.T) {
	directory := t.TempDir()
	guard := flock.New(filepath.Join(directory, Filename) + ".migrate.lock")
	if err := guard.Lock(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	opened, err := Open(ctx, directory)
	if err == nil {
		_ = opened.Close()
		t.Fatal("contended migration ignored deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("contended migration did not honor context: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		state MigrationState
		err   error
	}
	results := make(chan outcome, 4)
	for range 4 {
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			store, err := Open(ctx, directory)
			result := outcome{err: err}
			if err == nil {
				result.state = store.Migrations()
				result.err = store.Close()
			}
			results <- result
		}()
	}
	outcomes := make([]outcome, 0, 4)
	for range 4 {
		outcomes = append(outcomes, <-results)
	}
	applied := 0
	for _, result := range outcomes {
		if result.err != nil {
			t.Fatal(result.err)
		}
		applied += result.state.Applied
	}
	if applied != 1 {
		t.Fatalf("baseline applications=%d, want one", applied)
	}
}

func TestBehaviorMigrationRejectsUntrackedSchemaWithoutChangingCredentials(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	created, createError := store.Create(t.Context(), "existing")
	if err := errors.Join(createError, store.Close()); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, Filename))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}()
	// Existing business tables without migration history must never be silently
	// adopted as a freshly initialized database.
	if _, err := database.ExecContext(t.Context(), "DROP TABLE goose_db_version"); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(t.Context(), directory); err == nil {
		_ = opened.Close()
		t.Fatal("untracked schema was adopted")
	}
	var rows, version int
	if err := database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM api_keys WHERE id = ?", created.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(t.Context(), "SELECT MAX(version_id) FROM goose_db_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || version != 0 {
		t.Fatal("rejected initialization changed credentials or applied the baseline")
	}
}
