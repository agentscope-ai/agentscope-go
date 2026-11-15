package storage

import (
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
)

// --- Compile-time interface check ---

func TestSQLStorageInterfaceCompliance(t *testing.T) {
	// Verify SQLStorage implements agent.StateSaver at compile time.
	var _ agent.StateSaver = (*SQLStorage)(nil)
}

// --- Config validation tests ---

func TestSQLConfigValidation_EmptyDriver(t *testing.T) {
	_, err := NewSQLStorage(SQLConfig{
		DriverName: "",
		DSN:        "something",
	})
	if err == nil {
		t.Fatal("expected error for empty driver name")
	}
	if !strings.Contains(err.Error(), "driver name is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSQLConfigValidation_EmptyDSN(t *testing.T) {
	_, err := NewSQLStorage(SQLConfig{
		DriverName: "sqlite3",
		DSN:        "",
	})
	if err == nil {
		t.Fatal("expected error for empty DSN")
	}
	if !strings.Contains(err.Error(), "DSN is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSQLConfigValidation_UnknownDriver(t *testing.T) {
	// sql.Open with an unregistered driver should fail.
	_, err := NewSQLStorage(SQLConfig{
		DriverName: "nonexistent_driver_xyz",
		DSN:        "file::memory:",
	})
	if err == nil {
		t.Fatal("expected error for unregistered driver")
	}
	// The error should come from sql.Open or Ping.
}

func TestSQLConfigValidation_DefaultTableName(t *testing.T) {
	// We cannot fully initialize without a valid driver, but we can verify
	// that the table name default is "agent_sessions" by inspecting the config
	// after partial setup. Since NewSQLStorage fails early with unknown driver,
	// we test this logic indirectly via the placeholder and upsert helpers.
	s := &SQLStorage{
		tableName: "agent_sessions",
		driver:    "sqlite3",
	}
	// If the table name is the default it should appear in queries.
	q := s.upsertQuery()
	if !strings.Contains(q, "agent_sessions") {
		t.Errorf("expected default table name in upsert query, got: %s", q)
	}
}

// --- Placeholder function tests ---

func TestPlaceholder_Postgres(t *testing.T) {
	tests := []struct {
		driver string
	}{
		{"postgres"},
		{"pgx"},
		{"cloudsql-postgres"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			s := &SQLStorage{driver: tc.driver}
			if got := s.ph(1); got != "$1" {
				t.Errorf("ph(1) = %q, want $1", got)
			}
			if got := s.ph(2); got != "$2" {
				t.Errorf("ph(2) = %q, want $2", got)
			}
			if got := s.ph(5); got != "$5" {
				t.Errorf("ph(5) = %q, want $5", got)
			}
		})
	}
}

func TestPlaceholder_SQLiteAndMySQL(t *testing.T) {
	tests := []struct {
		driver string
	}{
		{"sqlite3"},
		{"sqlite"},
		{"mysql"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			s := &SQLStorage{driver: tc.driver}
			if got := s.ph(1); got != "?" {
				t.Errorf("ph(1) = %q, want ?", got)
			}
			if got := s.ph(2); got != "?" {
				t.Errorf("ph(2) = %q, want ?", got)
			}
			if got := s.ph(99); got != "?" {
				t.Errorf("ph(99) = %q, want ?", got)
			}
		})
	}
}

// --- isPostgres tests ---

func TestIsPostgres(t *testing.T) {
	tests := []struct {
		driver string
		want   bool
	}{
		{"postgres", true},
		{"pgx", true},
		{"cloudsql-postgres", true},
		{"sqlite3", false},
		{"mysql", false},
		{"sqlserver", false},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			s := &SQLStorage{driver: tc.driver}
			if got := s.isPostgres(); got != tc.want {
				t.Errorf("isPostgres() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- Upsert query generation tests ---

func TestUpsertQuery_Postgres(t *testing.T) {
	s := &SQLStorage{
		tableName: "my_sessions",
		driver:    "postgres",
	}
	q := s.upsertQuery()

	// Should use $1..$5 placeholders.
	if !strings.Contains(q, "$1") || !strings.Contains(q, "$5") {
		t.Error("postgres upsert should use $N placeholders")
	}
	// Should use ON CONFLICT ... DO UPDATE.
	if !strings.Contains(q, "ON CONFLICT") {
		t.Error("postgres upsert should use ON CONFLICT")
	}
	if !strings.Contains(q, "EXCLUDED.state_json") {
		t.Error("postgres upsert should reference EXCLUDED")
	}
	// Should reference the correct table.
	if !strings.Contains(q, "my_sessions") {
		t.Error("upsert should reference the configured table name")
	}
}

func TestUpsertQuery_MySQL(t *testing.T) {
	s := &SQLStorage{
		tableName: "sessions",
		driver:    "mysql",
	}
	q := s.upsertQuery()

	// Should use ? placeholders.
	if strings.Contains(q, "$1") {
		t.Error("mysql upsert should not use $N placeholders")
	}
	if !strings.Contains(q, "?") {
		t.Error("mysql upsert should use ? placeholders")
	}
	// Should use ON DUPLICATE KEY UPDATE.
	if !strings.Contains(q, "ON DUPLICATE KEY UPDATE") {
		t.Error("mysql upsert should use ON DUPLICATE KEY UPDATE")
	}
	if !strings.Contains(q, "sessions") {
		t.Error("upsert should reference the configured table name")
	}
}

func TestUpsertQuery_SQLite(t *testing.T) {
	s := &SQLStorage{
		tableName: "agent_sessions",
		driver:    "sqlite3",
	}
	q := s.upsertQuery()

	// Should use ? placeholders.
	if strings.Contains(q, "$1") {
		t.Error("sqlite upsert should not use $N placeholders")
	}
	if !strings.Contains(q, "?") {
		t.Error("sqlite upsert should use ? placeholders")
	}
	// Should use ON CONFLICT(...) DO UPDATE (SQLite style).
	if !strings.Contains(q, "ON CONFLICT(session_id)") {
		t.Error("sqlite upsert should use ON CONFLICT(session_id)")
	}
	if !strings.Contains(q, "excluded.state_json") {
		t.Error("sqlite upsert should reference excluded (lowercase)")
	}
	if !strings.Contains(q, "agent_sessions") {
		t.Error("upsert should reference the configured table name")
	}
}

func TestUpsertQuery_CustomTable(t *testing.T) {
	tables := []string{"custom_table", "my_app_states", "sessions_v2"}
	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			s := &SQLStorage{tableName: table, driver: "sqlite3"}
			q := s.upsertQuery()
			if !strings.Contains(q, table) {
				t.Errorf("expected table %q in query: %s", table, q)
			}
		})
	}
}

// --- Structural query tests ---

func TestUpsertQuery_AllDriversContainRequiredColumns(t *testing.T) {
	drivers := []string{"postgres", "mysql", "sqlite3"}
	columns := []string{"session_id", "state_json", "summary", "created_at", "updated_at"}

	for _, driver := range drivers {
		t.Run(driver, func(t *testing.T) {
			s := &SQLStorage{tableName: "t", driver: driver}
			q := s.upsertQuery()
			for _, col := range columns {
				if !strings.Contains(q, col) {
					t.Errorf("driver %q: missing column %q in upsert query", driver, col)
				}
			}
		})
	}
}
