package postgres

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS sessions (
		id text PRIMARY KEY,
		owner_id text NOT NULL,
		token_hash bytea NOT NULL UNIQUE,
		created_at timestamptz NOT NULL,
		last_seen_at timestamptz NOT NULL,
		expires_at timestamptz NOT NULL,
		revoked_at timestamptz
	)`,
	`CREATE INDEX IF NOT EXISTS sessions_active_token_idx
		ON sessions (token_hash, expires_at)
		WHERE revoked_at IS NULL`,
	`CREATE TABLE IF NOT EXISTS endpoints (
		id text PRIMARY KEY,
		owner_id text NOT NULL,
		display_name text NOT NULL,
		model_id text NOT NULL,
		revision text NOT NULL,
		profile text NOT NULL,
		min_replicas integer NOT NULL,
		max_replicas integer NOT NULL,
		target_queue_depth integer NOT NULL,
		idle_timeout_seconds integer NOT NULL,
		cache_preference text NOT NULL,
		max_cold_start_fallback_seconds integer NOT NULL,
		endpoint_uid text,
		created_at timestamptz NOT NULL,
		provisioned_at timestamptz,
		deletion_requested_at timestamptz,
		deleted_at timestamptz
	)`,
	`CREATE INDEX IF NOT EXISTS endpoints_owner_created_idx
		ON endpoints (owner_id, created_at DESC)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS endpoints_uid_idx
		ON endpoints (endpoint_uid)
		WHERE endpoint_uid IS NOT NULL`,
	`CREATE TABLE IF NOT EXISTS endpoint_idempotency (
		owner_id text NOT NULL,
		operation text NOT NULL,
		idempotency_key text NOT NULL,
		request_hash text NOT NULL,
		endpoint_id text NOT NULL REFERENCES endpoints(id) ON DELETE RESTRICT,
		created_at timestamptz NOT NULL,
		PRIMARY KEY (owner_id, operation, idempotency_key)
	)`,
	`CREATE TABLE IF NOT EXISTS audit_events (
		id bigserial PRIMARY KEY,
		actor text NOT NULL,
		action text NOT NULL,
		endpoint_id text REFERENCES endpoints(id) ON DELETE RESTRICT,
		endpoint_uid text,
		request_id text NOT NULL,
		result text NOT NULL,
		details jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at timestamptz NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS audit_events_endpoint_created_idx
		ON audit_events (endpoint_id, created_at DESC)`,
	`CREATE OR REPLACE FUNCTION ember_reject_audit_mutation()
		RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			RAISE EXCEPTION 'audit_events is append-only';
		END;
		$$`,
	`DROP TRIGGER IF EXISTS audit_events_append_only ON audit_events`,
	`CREATE TRIGGER audit_events_append_only
		BEFORE UPDATE OR DELETE ON audit_events
		FOR EACH ROW EXECUTE FUNCTION ember_reject_audit_mutation()`,
}
