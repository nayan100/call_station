// Package callservice implements the core call lifecycle management.
package callservice

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
)

// CallStatus represents a state in the call lifecycle.
type CallStatus string

const (
	StatusPending     CallStatus = "pending"
	StatusDispatching CallStatus = "dispatching"
	StatusEnRoute     CallStatus = "en_route"
	StatusArrived     CallStatus = "arrived"
	StatusFailed      CallStatus = "failed"
)

// IsTerminal returns true if the status is a terminal state.
func (s CallStatus) IsTerminal() bool {
	return s == StatusArrived || s == StatusFailed
}

// Call represents a station's robot call record.
type Call struct {
	ID          string
	StationID   string
	Destination string
	RequestID   string
	MissionID   sql.NullString
	RobotID     sql.NullString
	Status      CallStatus
	RetryCount  int
	RetryAfter  sql.NullTime
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CallEvent is an immutable timestamped record of a state transition.
type CallEvent struct {
	ID     int64      `json:"id"`
	CallID string     `json:"callId"`
	Status CallStatus `json:"status"`
	Note   string     `json:"note"`
	TS     time.Time  `json:"ts"`
}

// Store handles all PostgreSQL interactions for the call service.
type Store struct {
	db *sql.DB
}

// NewStore opens a PostgreSQL connection and runs migrations.
func NewStore(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	if err := runMigrations(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// runMigrations applies the schema DDL idempotently.
func runMigrations(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE EXTENSION IF NOT EXISTS "pgcrypto";

		CREATE TABLE IF NOT EXISTS calls (
			id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			station_id   TEXT        NOT NULL,
			destination  TEXT        NOT NULL,
			request_id   UUID        NOT NULL UNIQUE,
			mission_id   TEXT,
			robot_id     TEXT,
			status       TEXT        NOT NULL
			             CHECK (status IN (
			               'pending','dispatching','en_route','arrived','failed'
			             )),
			retry_count  INT         NOT NULL DEFAULT 0,
			retry_after  TIMESTAMPTZ,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
		);

		CREATE TABLE IF NOT EXISTS call_events (
			id       BIGSERIAL   PRIMARY KEY,
			call_id  UUID        NOT NULL REFERENCES calls(id),
			status   TEXT        NOT NULL,
			note     TEXT,
			ts       TIMESTAMPTZ NOT NULL DEFAULT now()
		);

		CREATE INDEX IF NOT EXISTS idx_call_events_call_id ON call_events(call_id);
		CREATE INDEX IF NOT EXISTS idx_calls_station_id ON calls(station_id);
		CREATE INDEX IF NOT EXISTS idx_calls_status ON calls(status);
	`)
	if err != nil {
		return err
	}

	// Apply partial unique index separately (DDL inside CREATE TABLE IF NOT EXISTS doesn't support it)
	_, err = db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS one_open_call_per_station
		    ON calls (station_id)
		    WHERE status NOT IN ('arrived', 'failed');

		-- Reset retry_after for in-flight calls on boot so they resume immediately
		UPDATE calls
		SET retry_after = now()
		WHERE status IN ('pending', 'dispatching');
	`)
	return err
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// GetOpenCall returns the open (non-terminal) call for a station, or nil if none.
func (s *Store) GetOpenCall(ctx context.Context, stationID string) (*Call, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, station_id, destination, request_id, mission_id, robot_id,
		       status, retry_count, retry_after, created_at, updated_at
		FROM calls
		WHERE station_id = $1 AND status NOT IN ('arrived','failed')
		LIMIT 1
	`, stationID)
	return scanCall(row)
}

// GetCall returns a call by its ID.
func (s *Store) GetCall(ctx context.Context, callID string) (*Call, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, station_id, destination, request_id, mission_id, robot_id,
		       status, retry_count, retry_after, created_at, updated_at
		FROM calls WHERE id = $1
	`, callID)
	return scanCall(row)
}

// GetLatestCallForStation returns the most recent call for a station (open or terminal).
func (s *Store) GetLatestCallForStation(ctx context.Context, stationID string) (*Call, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, station_id, destination, request_id, mission_id, robot_id,
		       status, retry_count, retry_after, created_at, updated_at
		FROM calls
		WHERE station_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, stationID)
	return scanCall(row)
}

// CreateCall inserts a new call record and its first event.
// Returns error if a unique constraint violation occurs (open call already exists).
func (s *Store) CreateCall(ctx context.Context, stationID, destination, requestID string) (*Call, error) {
	var call Call
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO calls (station_id, destination, request_id, status)
		VALUES ($1, $2, $3, 'pending')
		RETURNING id, station_id, destination, request_id, mission_id, robot_id,
		          status, retry_count, retry_after, created_at, updated_at
	`, stationID, destination, requestID).Scan(
		&call.ID, &call.StationID, &call.Destination, &call.RequestID,
		&call.MissionID, &call.RobotID,
		&call.Status, &call.RetryCount, &call.RetryAfter,
		&call.CreatedAt, &call.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	// Record initial event
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO call_events (call_id, status, note) VALUES ($1, 'pending', 'call created')
	`, call.ID)

	return &call, nil
}

// UpdateCallStatus sets a new status and optionally mission/robot IDs.
func (s *Store) UpdateCallStatus(ctx context.Context, callID string, status CallStatus, missionID, robotID, note string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE calls
		SET status = $1,
		    mission_id = COALESCE(NULLIF($2,''), mission_id),
		    robot_id   = COALESCE(NULLIF($3,''), robot_id),
		    updated_at = now()
		WHERE id = $4
	`, status, missionID, robotID, callID)
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO call_events (call_id, status, note) VALUES ($1, $2, $3)
	`, callID, status, note)
	return nil
}

// IncrementRetry bumps retry_count and sets retry_after.
func (s *Store) IncrementRetry(ctx context.Context, callID string, retryAfter time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE calls
		SET retry_count = retry_count + 1,
		    retry_after  = $1,
		    updated_at   = now()
		WHERE id = $2
	`, retryAfter, callID)
	return err
}

// GetActiveDispatchingCalls returns all calls that need dispatch retrying right now.
func (s *Store) GetActiveDispatchingCalls(ctx context.Context) ([]*Call, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, station_id, destination, request_id, mission_id, robot_id,
		       status, retry_count, retry_after, created_at, updated_at
		FROM calls
		WHERE status IN ('pending','dispatching')
		  AND (retry_after IS NULL OR retry_after <= now())
		ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCalls(rows)
}

// GetActiveMissionCalls returns all calls that have a missionId and are in en_route status.
func (s *Store) GetActiveMissionCalls(ctx context.Context) ([]*Call, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, station_id, destination, request_id, mission_id, robot_id,
		       status, retry_count, retry_after, created_at, updated_at
		FROM calls
		WHERE status = 'en_route' AND mission_id IS NOT NULL
		ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCalls(rows)
}

// ListAllCalls returns all calls ordered by creation time descending (for call log).
func (s *Store) ListAllCalls(ctx context.Context) ([]*Call, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, station_id, destination, request_id, mission_id, robot_id,
		       status, retry_count, retry_after, created_at, updated_at
		FROM calls
		ORDER BY created_at DESC
		LIMIT 500
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCalls(rows)
}

// ListCallEvents returns all events for a call ordered by time ascending.
func (s *Store) ListCallEvents(ctx context.Context, callID string) ([]*CallEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, call_id, status, COALESCE(note,''), ts
		FROM call_events
		WHERE call_id = $1
		ORDER BY ts
	`, callID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*CallEvent
	for rows.Next() {
		var e CallEvent
		if err := rows.Scan(&e.ID, &e.CallID, &e.Status, &e.Note, &e.TS); err != nil {
			return nil, err
		}
		events = append(events, &e)
	}
	return events, rows.Err()
}

// TimeoutStaleCalls marks dispatching calls that have exceeded the timeout as failed.
func (s *Store) TimeoutStaleCalls(ctx context.Context, dispatchTimeout, enRouteTimeout time.Duration) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE calls
		SET status = 'failed', updated_at = now()
		WHERE status IN ('pending','dispatching')
		  AND created_at < now() - $1::interval
	`, fmt.Sprintf("%d seconds", int(dispatchTimeout.Seconds())))
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE calls
		SET status = 'failed', updated_at = now()
		WHERE status = 'en_route'
		  AND created_at < now() - $1::interval
	`, fmt.Sprintf("%d seconds", int(enRouteTimeout.Seconds())))
	return err
}

func scanCall(row *sql.Row) (*Call, error) {
	var c Call
	err := row.Scan(
		&c.ID, &c.StationID, &c.Destination, &c.RequestID,
		&c.MissionID, &c.RobotID,
		&c.Status, &c.RetryCount, &c.RetryAfter,
		&c.CreatedAt, &c.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &c, err
}

func scanCalls(rows *sql.Rows) ([]*Call, error) {
	var calls []*Call
	for rows.Next() {
		var c Call
		err := rows.Scan(
			&c.ID, &c.StationID, &c.Destination, &c.RequestID,
			&c.MissionID, &c.RobotID,
			&c.Status, &c.RetryCount, &c.RetryAfter,
			&c.CreatedAt, &c.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		calls = append(calls, &c)
	}
	return calls, rows.Err()
}
