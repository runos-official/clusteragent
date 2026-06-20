package datastore

import (
	"errors"
	"testing"
	"time"
)

// TestActiveDB_NotReady asserts activeDB() returns ErrNotReady when no handle is
// installed, and that a representative query function surfaces ErrNotReady
// cleanly (rather than panicking on a nil handle) when the datastore is not
// connected. It clears the handle for the duration of the test and restores it.
func TestActiveDB_NotReady(t *testing.T) {
	prev := setHandle(nil)
	t.Cleanup(func() { setHandle(prev) })

	gdb, err := activeDB()
	if gdb != nil {
		t.Fatalf("activeDB() handle = %v, want nil when not connected", gdb)
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("activeDB() err = %v, want ErrNotReady", err)
	}

	// A representative query function must surface ErrNotReady, not panic.
	if err := CreateBuildKitJob("job-x", "osid-x", "repo", "main", "sha"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("CreateBuildKitJob err = %v, want ErrNotReady", err)
	}
	if _, err := GetBuildKitJob("job-x"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("GetBuildKitJob err = %v, want ErrNotReady", err)
	}
	if _, err := GetUploadToken("tok"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("GetUploadToken err = %v, want ErrNotReady", err)
	}
	if err := DeleteExpiredUploadTokens(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("DeleteExpiredUploadTokens err = %v, want ErrNotReady", err)
	}
}

// TestActiveDB_ReadyAfterSetHandle confirms the test helper path: once a handle
// is installed directly (as setupTestDB does), activeDB() returns it with no
// error.
func TestActiveDB_ReadyAfterSetHandle(t *testing.T) {
	setupTestDB(t)
	gdb, err := activeDB()
	if err != nil {
		t.Fatalf("activeDB() after setupTestDB err = %v, want nil", err)
	}
	if gdb == nil {
		t.Fatal("activeDB() handle = nil after setupTestDB, want live handle")
	}
}

// TestInitializeClose_NoLeakNoCrash verifies Initialize() returns immediately
// without a live cluster (it must never block or fail when Postgres/the pointer
// ConfigMap is absent) and that Close() cancels the loop and returns promptly
// (no goroutine leak / no hang). The reconcile loop runs against a non-cluster
// environment, fails to resolve a target, and backs off; Close must still stop
// it quickly.
func TestInitializeClose_NoLeakNoCrash(t *testing.T) {
	// Ensure a clean slate: no loop running, no handle.
	_ = Close()
	prev := setHandle(nil)
	t.Cleanup(func() { setHandle(prev) })

	if err := Initialize(); err != nil {
		t.Fatalf("Initialize() = %v, want nil (must never fail startup)", err)
	}
	// Second Initialize is a no-op and must not start a second loop.
	if err := Initialize(); err != nil {
		t.Fatalf("Initialize() second call = %v, want nil", err)
	}

	done := make(chan error, 1)
	go func() { done <- Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close() = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close() did not return within 10s: reconcile loop leaked")
	}
}
