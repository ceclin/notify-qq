package apikey

import (
	"testing"
	"testing/fstest"

	"github.com/ceclin/notify-qq/internal/recipient"
)

func TestBehaviorUserOnlyDatabaseMigratesToTypedRecipients(t *testing.T) {
	directory := t.TempDir()
	baseline, err := migrationFiles.ReadFile("migrations/00001_create_api_keys.sql")
	if err != nil {
		t.Fatal(err)
	}
	legacySource := fstest.MapFS{"00001_create_api_keys.sql": &fstest.MapFile{Data: baseline}}
	legacy, err := openStore(t.Context(), directory, legacySource)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := legacy.Create(t.Context(), "bound-user")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := legacy.Create(t.Context(), "pending-repair")
	if err != nil {
		t.Fatal(err)
	}
	unpaired, err := legacy.Create(t.Context(), "unpaired")
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := legacy.Create(t.Context(), "revoked")
	if err != nil {
		t.Fatal(err)
	}
	// Seed the released schema directly; current binding queries use typed targets.
	if _, err := legacy.db.ExecContext(t.Context(), "UPDATE api_keys SET user_openid = ?, pairing_digest = NULL, pairing_expires_at = NULL WHERE id = ?", "alice", bound.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.ExecContext(t.Context(), "UPDATE api_keys SET user_openid = ? WHERE id = ?", "previous-owner", pending.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Revoke(t.Context(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := upgraded.Close(); err != nil {
			t.Error(err)
		}
	}()
	if upgraded.Migrations() != (MigrationState{Version: 2, Applied: 1}) {
		t.Fatal("existing database did not apply just the recipient migration")
	}
	owner, valid, err := upgraded.Authenticate(t.Context(), bound.APIKey)
	if err != nil || !valid || owner.Recipient == nil || *owner.Recipient != (recipient.Target{Type: recipient.User, OpenID: "alice"}) {
		t.Fatal("migration lost an existing user binding or API key")
	}
	key, valid, err := upgraded.Authenticate(t.Context(), unpaired.APIKey)
	if err != nil || !valid || key.Recipient != nil {
		t.Fatal("unpaired key gained a recipient")
	}
	if _, valid, err := upgraded.Authenticate(t.Context(), revoked.APIKey); err != nil || valid {
		t.Fatal("migration revived a revoked key")
	}
	previous, valid, err := upgraded.Authenticate(t.Context(), pending.APIKey)
	if err != nil || !valid || previous.Recipient == nil || previous.Recipient.OpenID != "previous-owner" || previous.PairingExpiresAt == nil || *previous.PairingExpiresAt != *pending.PairingExpiresAt {
		t.Fatal("migration changed a pending replacement or its current recipient")
	}
	target := recipient.Target{Type: recipient.Group, OpenID: "new-group"}
	paired, changed, err := upgraded.Bind(t.Context(), pending.PairingCode, target)
	if err != nil || !changed || paired.Recipient == nil || *paired.Recipient != target {
		t.Fatal("migration lost the pending pairing credential")
	}
	var oldColumns int
	if err := upgraded.db.QueryRowContext(t.Context(), "SELECT count(*) FROM pragma_table_info('api_keys') WHERE name = 'user_openid'").Scan(&oldColumns); err != nil {
		t.Fatal(err)
	}
	if oldColumns != 0 {
		t.Fatal("old recipient column remains after migration")
	}
}
