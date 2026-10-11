package apikey

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"time"

	"github.com/gofrs/flock"
	"github.com/pressly/goose/v3"
	"github.com/samber/oops"

	"github.com/ceclin/notify-qq/internal/logging"
)

// Embedding keeps startup independent of the working directory and includes
// the same reviewed SQL in native builds and the scratch container image.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

type MigrationState struct {
	Version int64
	Applied int
}

func (s *Store) Migrations() MigrationState { return s.migrations }

func migrate(ctx context.Context, db *sql.DB, path string, source fs.FS) (state MigrationState, migrationError error) {
	builder := oops.FromContext(ctx).In("database").Code("database_migration_failed")
	// Goose's provider mutex is instance-local. A file lock also serializes the
	// pending-version check across service and CLI processes sharing this file.
	guard := flock.New(path + ".migrate.lock")
	defer func() { migrationError = builder.Wrap(errors.Join(migrationError, guard.Close())) }()
	acquired, err := guard.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return state, builder.Wrapf(err, "database migration lock could not be acquired")
	}
	if !acquired {
		return state, builder.New("database migration lock could not be acquired")
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, source, goose.WithDisableGlobalRegistry(true), goose.WithLogger(goose.NopLogger()))
	if err != nil {
		return state, builder.Wrapf(err, "database migration sources are invalid")
	}
	latest := int64(0)
	for _, migration := range provider.ListSources() {
		latest = max(latest, migration.Version)
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return state, builder.Wrapf(err, "database migration version could not be read")
	}
	if version > latest {
		return state, unsupportedSchema(ctx, version, latest)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return state, builder.Wrapf(err, "database migrations could not be applied")
	}
	state = MigrationState{Version: latest, Applied: len(results)}
	// The Store owns db. Provider.Close would close the shared connection pool.
	return state, nil
}

func unsupportedSchema(ctx context.Context, current, supported int64) error {
	return logging.WithErrorAttributes(oops.FromContext(ctx).In("database").Code("database_schema_unsupported"), slog.Int64("database_version", current), slog.Int64("supported_version", supported)).New("database schema is newer than this application")
}
