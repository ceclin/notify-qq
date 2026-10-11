package apikey

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBehaviorReadinessHonorsDatabaseWaitDeadlineAndRecovers(t *testing.T) {
	store, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	connection, err := store.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := store.CheckReadiness(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("readiness did not honor connection wait deadline: %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckReadiness(t.Context()); err != nil {
		t.Fatalf("readiness did not recover after connection release: %v", err)
	}
}
