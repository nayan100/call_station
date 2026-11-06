-- migrations/001_init.sql
-- Call Station schema

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

-- Enforces at-most-one open call per station at database level
CREATE UNIQUE INDEX IF NOT EXISTS one_open_call_per_station
    ON calls (station_id)
    WHERE status NOT IN ('arrived', 'failed');

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
