package datastore

import (
	"strings"
	"testing"
)

// TestMigratedTablesArePrefixed is the regression guard for the bug where the
// models' explicit TableName() returned bare names, overriding the
// NamingStrategy TablePrefix, so AutoMigrate created unprefixed physical tables
// (buildkit_jobs instead of cluster_agent_buildkit_jobs) in the shared runos DB.
// It migrates the real model set and asserts every physical table, and every
// model's TableName(), carries the cluster_agent_ prefix.
func TestMigratedTablesArePrefixed(t *testing.T) {
	setupTestDB(t)
	gdb := testDB(t)

	var names []string
	if err := gdb.Raw(
		"SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'",
	).Scan(&names).Error; err != nil {
		t.Fatalf("list migrated tables: %v", err)
	}
	if len(names) != len(allModels()) {
		t.Fatalf("expected %d migrated tables, got %d: %v", len(allModels()), len(names), names)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, tablePrefix) {
			t.Errorf("physical table %q is missing the %q prefix", n, tablePrefix)
		}
	}

	// Each model's TableName() must already be prefixed (an explicit TableName
	// overrides the NamingStrategy, so the prefix has to live in the method).
	for _, m := range allModels() {
		namer, ok := m.(interface{ TableName() string })
		if !ok {
			t.Errorf("model %T does not implement TableName()", m)
			continue
		}
		if tn := namer.TableName(); !strings.HasPrefix(tn, tablePrefix) {
			t.Errorf("model %T TableName() %q is missing the %q prefix", m, tn, tablePrefix)
		}
	}
}
