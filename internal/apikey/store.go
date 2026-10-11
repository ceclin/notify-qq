package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/oklog/ulid/v2"
	"github.com/samber/oops"
	_ "modernc.org/sqlite"
)

const Filename = "notify.sqlite3"
const PairingLifetime = 15 * time.Minute

type Key struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	CreatedAt        string  `json:"created_at"`
	RevokedAt        *string `json:"revoked_at"`
	UserOpenID       string  `json:"user_openid"`
	PairingExpiresAt *string `json:"pairing_expires_at"`
}

type CreateResult struct {
	Key
	APIKey      string `json:"key"`
	PairingCode string `json:"pairing_code"`
}

type PairingResult struct {
	Key
	PairingCode string `json:"pairing_code"`
}

type Store struct {
	db         *sql.DB
	migrations MigrationState
}

// Open shares a local database with CLI commands. A single connection per process
// keeps SQLite writes serialized; WAL lets other processes read during a write.
func Open(ctx context.Context, directory string) (*Store, error) {
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, oops.In("database").Code("migration_source_failed").Wrap(err)
	}
	return openStore(ctx, directory, source)
}

func openStore(ctx context.Context, directory string, source fs.FS) (*Store, error) {
	builder := oops.FromContext(ctx).In("api_keys").Code("key_store_open_failed")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, builder.Wrapf(err, "key storage directory could not be created")
	}
	path, err := filepath.Abs(filepath.Join(directory, Filename))
	if err != nil {
		return nil, builder.Wrapf(err, "key storage path could not be resolved")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, builder.Wrapf(err, "key database could not be opened")
	}
	permissionError := file.Chmod(0600)
	closeError := file.Close()
	if err := errors.Join(permissionError, closeError); err != nil {
		return nil, builder.Wrapf(err, "key database permissions could not be secured")
	}
	location := url.URL{Scheme: "file", Path: path}
	query := location.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Set("_txlock", "immediate")
	location.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, builder.Wrapf(err, "key database driver could not be opened")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	migration, err := migrate(ctx, db, path, source)
	if err != nil {
		return nil, builder.Wrap(errors.Join(err, db.Close()))
	}
	store.migrations = migration
	return store, nil
}

func (s *Store) Close() error {
	return oops.In("api_keys").Code("key_store_close_failed").Wrapf(s.db.Close(), "key database could not close")
}

// Create returns both independent random credentials once. The recipient's
// pairing code grants binding only and cannot authenticate an HTTP request.
func (s *Store) Create(ctx context.Context, name string) (CreateResult, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 || strings.ContainsFunc(name, unicode.IsControl) {
		return CreateResult{}, oops.FromContext(ctx).In("api_keys").Code("key_name_invalid").New("key name must contain 1 to 100 bytes without control characters")
	}
	secret, err := generateSecret(ctx, "nqq_")
	if err != nil {
		return CreateResult{}, err
	}
	code, err := generateSecret(ctx, "nqp_")
	if err != nil {
		return CreateResult{}, err
	}
	now := time.Now().UTC()
	// Fixed-width UTC timestamps compare chronologically in SQLite, including at
	// the exact expiry boundary. RFC3339Nano has variable fractional precision.
	expires := now.Add(PairingLifetime).Format(time.RFC3339)
	key := Key{ID: ulid.Make().String(), Name: name, CreatedAt: now.Format(time.RFC3339), PairingExpiresAt: &expires}
	digest, pairingDigest := sha256.Sum256([]byte(secret)), sha256.Sum256([]byte(code))
	if _, err := s.db.ExecContext(ctx, `INSERT INTO api_keys(id, name, digest, created_at, pairing_digest, pairing_expires_at) VALUES(?, ?, ?, ?, ?, ?)`, key.ID, key.Name, digest[:], key.CreatedAt, pairingDigest[:], expires); err != nil {
		return CreateResult{}, oops.FromContext(ctx).In("api_keys").Code("key_create_failed").Wrapf(err, "API key could not be stored")
	}
	return CreateResult{Key: key, APIKey: secret, PairingCode: code}, nil
}

func generateSecret(ctx context.Context, prefix string) (string, error) {
	var randomBytes [32]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", oops.FromContext(ctx).In("api_keys").Code("credential_generation_failed").Wrap(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(randomBytes[:]), nil
}

type keyScanner interface{ Scan(...any) error }

func scanKey(row keyScanner) (Key, error) {
	var key Key
	err := row.Scan(&key.ID, &key.Name, &key.CreatedAt, &key.RevokedAt, &key.UserOpenID, &key.PairingExpiresAt)
	return key, err
}

func (s *Store) Authenticate(ctx context.Context, secret string) (Key, bool, error) {
	digest := sha256.Sum256([]byte(secret))
	key, err := scanKey(s.db.QueryRowContext(ctx, "SELECT id, name, created_at, revoked_at, user_openid, pairing_expires_at FROM api_keys WHERE digest = ? AND revoked_at IS NULL", digest[:]))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, oops.FromContext(ctx).In("api_keys").Code("key_lookup_failed").Wrapf(err, "API key could not be checked")
	}
	return key, true, nil
}

// IssuePairing keeps the existing recipient until a new code is consumed. Only
// the local operator can authorize a replacement by issuing that fresh code.
func (s *Store) IssuePairing(ctx context.Context, id string) (PairingResult, error) {
	code, err := generateSecret(ctx, "nqp_")
	if err != nil {
		return PairingResult{}, err
	}
	expires := time.Now().UTC().Add(PairingLifetime).Format(time.RFC3339)
	digest := sha256.Sum256([]byte(code))
	key, err := scanKey(s.db.QueryRowContext(ctx, `UPDATE api_keys SET pairing_digest = ?, pairing_expires_at = ? WHERE id = ? AND revoked_at IS NULL RETURNING id, name, created_at, revoked_at, user_openid, pairing_expires_at`, digest[:], expires, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PairingResult{}, oops.FromContext(ctx).In("api_keys").Code("key_unavailable").New("API key is missing or revoked")
	}
	if err != nil {
		return PairingResult{}, oops.FromContext(ctx).In("api_keys").Code("pairing_issue_failed").Wrapf(err, "pairing code could not be issued")
	}
	return PairingResult{Key: key, PairingCode: code}, nil
}

// A single conditional update consumes the code and changes the recipient
// atomically, even when the CLI and gateway hold separate database connections.
func (s *Store) Bind(ctx context.Context, code, userOpenID string) (Key, bool, error) {
	if userOpenID == "" {
		return Key{}, false, nil
	}
	digest := sha256.Sum256([]byte(code))
	key, err := scanKey(s.db.QueryRowContext(ctx, `UPDATE api_keys SET user_openid = ?, pairing_digest = NULL, pairing_expires_at = NULL WHERE pairing_digest = ? AND pairing_expires_at > ? AND revoked_at IS NULL RETURNING id, name, created_at, revoked_at, user_openid, pairing_expires_at`, userOpenID, digest[:], time.Now().UTC().Format(time.RFC3339)))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, oops.FromContext(ctx).In("api_keys").Code("pairing_save_failed").Wrapf(err, "recipient binding could not be stored")
	}
	return key, true, nil
}

func (s *Store) List(ctx context.Context) (keys []Key, err error) {
	builder := oops.FromContext(ctx).In("api_keys").Code("key_list_failed")
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, created_at, revoked_at, user_openid, pairing_expires_at FROM api_keys ORDER BY created_at, id")
	if err != nil {
		return nil, builder.Wrapf(err, "API keys could not be listed")
	}
	defer func() { err = builder.Wrap(errors.Join(err, rows.Close())) }()
	keys = []Key{}
	for rows.Next() {
		key, err := scanKey(rows)
		if err != nil {
			return nil, builder.Wrap(err)
		}
		keys = append(keys, key)
	}
	return keys, builder.Wrap(rows.Err())
}

func (s *Store) Revoke(ctx context.Context, id string) (bool, error) {
	builder := oops.FromContext(ctx).In("api_keys").Code("key_revoke_failed")
	result, err := s.db.ExecContext(ctx, "UPDATE api_keys SET revoked_at = ?, pairing_digest = NULL, pairing_expires_at = NULL WHERE id = ? AND revoked_at IS NULL", time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return false, builder.Wrapf(err, "API key could not be revoked")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, builder.Wrap(err)
	}
	if count == 0 {
		var exists bool
		if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM api_keys WHERE id = ?)", id).Scan(&exists); err != nil {
			return false, builder.Wrap(err)
		}
		if !exists {
			return false, oops.FromContext(ctx).In("api_keys").Code("key_not_found").New("API key ID does not exist")
		}
	}
	return count > 0, nil
}

// CheckReadiness exercises the schema needed by authentication. An empty key
// table is healthy; this read probe does not claim that future writes will work.
func (s *Store) CheckReadiness(ctx context.Context) error {
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM api_keys)").Scan(&exists)
	return oops.FromContext(ctx).In("database").Code("storage_unavailable").Wrapf(err, "authentication storage is unavailable")
}
