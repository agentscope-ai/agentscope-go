package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
)

// SQLConfig configures the SQL storage backend.
type SQLConfig struct {
	DriverName string // e.g. "sqlite3", "postgres", "mysql"
	DSN        string // data source name
	TableName  string // default: "agent_sessions"
}

// SQLStorage persists agent states in a SQL database.
// Compatible with any database/sql driver (sqlite, postgres, mysql).
type SQLStorage struct {
	db        *sql.DB
	tableName string
	driver    string
}

// Compile-time interface check.
var _ agent.StateSaver = (*SQLStorage)(nil)

// NewSQLStorage opens a database connection and creates the sessions table if
// it does not exist. The caller is responsible for importing the appropriate
// database driver (blank import pattern).
func NewSQLStorage(cfg SQLConfig) (*SQLStorage, error) {
	if cfg.DriverName == "" {
		return nil, fmt.Errorf("storage/sql: driver name is required")
	}
	if cfg.DSN == "" {
		return nil, fmt.Errorf("storage/sql: DSN is required")
	}
	if cfg.TableName == "" {
		cfg.TableName = "agent_sessions"
	}

	db, err := sql.Open(cfg.DriverName, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("storage/sql: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage/sql: ping: %w", err)
	}

	s := &SQLStorage{
		db:        db,
		tableName: cfg.TableName,
		driver:    cfg.DriverName,
	}

	if err := s.createTable(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

// Close closes the underlying database connection.
func (s *SQLStorage) Close() error {
	return s.db.Close()
}

// SaveState persists an agent state using UPSERT semantics.
func (s *SQLStorage) SaveState(ctx context.Context, sessionID string, state *agent.AgentState) error {
	if sessionID == "" {
		return fmt.Errorf("storage/sql: session ID is required")
	}
	state.SessionID = sessionID

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("storage/sql: marshal state: %w", err)
	}

	now := time.Now().UTC()
	query := s.upsertQuery()
	_, err = s.db.ExecContext(ctx, query, sessionID, string(data), state.Summary, now, now)
	if err != nil {
		return fmt.Errorf("storage/sql: save state: %w", err)
	}
	return nil
}

// LoadState retrieves an agent state by session ID.
func (s *SQLStorage) LoadState(ctx context.Context, sessionID string) (*agent.AgentState, error) {
	query := fmt.Sprintf(
		"SELECT state_json FROM %s WHERE session_id = %s",
		s.tableName, s.ph(1),
	)
	row := s.db.QueryRowContext(ctx, query, sessionID)

	var data string
	if err := row.Scan(&data); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("storage/sql: session %q not found", sessionID)
		}
		return nil, fmt.Errorf("storage/sql: load state: %w", err)
	}

	var state agent.AgentState
	if err := json.Unmarshal([]byte(data), &state); err != nil {
		return nil, fmt.Errorf("storage/sql: unmarshal state: %w", err)
	}
	return &state, nil
}

// ListSessions returns summary info for all persisted sessions.
func (s *SQLStorage) ListSessions(ctx context.Context) ([]agent.SessionInfo, error) {
	query := fmt.Sprintf(
		"SELECT session_id, summary FROM %s ORDER BY created_at ASC",
		s.tableName,
	)
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("storage/sql: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var infos []agent.SessionInfo
	for rows.Next() {
		var info agent.SessionInfo
		var summary sql.NullString
		if err := rows.Scan(&info.SessionID, &summary); err != nil {
			return nil, fmt.Errorf("storage/sql: scan session: %w", err)
		}
		if summary.Valid {
			info.Summary = summary.String
		}
		infos = append(infos, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage/sql: iterate sessions: %w", err)
	}
	return infos, nil
}

// DeleteSession removes a session by ID.
func (s *SQLStorage) DeleteSession(ctx context.Context, sessionID string) error {
	query := fmt.Sprintf(
		"DELETE FROM %s WHERE session_id = %s",
		s.tableName, s.ph(1),
	)
	res, err := s.db.ExecContext(ctx, query, sessionID)
	if err != nil {
		return fmt.Errorf("storage/sql: delete session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("storage/sql: session %q not found", sessionID)
	}
	return nil
}

// --- internal helpers ---

// createTable creates the sessions table if it does not exist.
func (s *SQLStorage) createTable() error {
	// Use TEXT for maximum portability; postgres users can cast to JSONB in
	// their own migrations if desired.
	query := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		session_id TEXT PRIMARY KEY,
		state_json TEXT NOT NULL,
		summary    TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	)`, s.tableName)
	_, err := s.db.Exec(query)
	if err != nil {
		return fmt.Errorf("storage/sql: create table: %w", err)
	}
	return nil
}

// ph returns a placeholder for positional parameter n (1-indexed).
// Postgres uses $1, $2, ...; sqlite and mysql use ?.
func (s *SQLStorage) ph(n int) string {
	if s.isPostgres() {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// isPostgres returns true if the driver is postgres.
func (s *SQLStorage) isPostgres() bool {
	return strings.Contains(s.driver, "postgres") || strings.Contains(s.driver, "pgx")
}

// upsertQuery returns the driver-appropriate UPSERT statement.
func (s *SQLStorage) upsertQuery() string {
	if s.isPostgres() {
		return fmt.Sprintf(`INSERT INTO %s (session_id, state_json, summary, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (session_id) DO UPDATE SET
				state_json = EXCLUDED.state_json,
				summary    = EXCLUDED.summary,
				updated_at = EXCLUDED.updated_at`,
			s.tableName)
	}
	// SQLite and MySQL both support this form (SQLite >= 3.24, MySQL >= 8.0.19 with aliases).
	// For broadest SQLite/MySQL compat, use INSERT OR REPLACE (loses created_at) or
	// INSERT ... ON CONFLICT for SQLite. We use SQLite-style here:
	if strings.Contains(s.driver, "mysql") {
		return fmt.Sprintf(`INSERT INTO %s (session_id, state_json, summary, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				state_json = VALUES(state_json),
				summary    = VALUES(summary),
				updated_at = VALUES(updated_at)`,
			s.tableName)
	}
	// SQLite default
	return fmt.Sprintf(`INSERT INTO %s (session_id, state_json, summary, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			state_json = excluded.state_json,
			summary    = excluded.summary,
			updated_at = excluded.updated_at`,
		s.tableName)
}
