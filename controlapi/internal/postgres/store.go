package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/RuokeZhang/ember/controlapi/internal/controlapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const endpointCreateOperation = "endpoint.create"

type Store struct {
	pool *pgxpool.Pool
}

type rowScanner interface {
	Scan(...any) error
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	config.MaxConns = 8
	config.MinConns = 1
	config.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}
	store := &Store{pool: pool}
	if err := store.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping Postgres: %w", err)
	}
	return nil
}

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range schemaStatements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("apply schema migration: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}

func (s *Store) CreateSession(ctx context.Context, session controlapi.Session) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (
		id, owner_id, token_hash, created_at, last_seen_at, expires_at, revoked_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		session.ID,
		session.OwnerID,
		session.TokenHash,
		session.CreatedAt,
		session.LastSeen,
		session.ExpiresAt,
		session.RevokedAt,
	)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash []byte, now time.Time) (controlapi.Session, error) {
	var session controlapi.Session
	err := s.pool.QueryRow(ctx, `SELECT
		id, owner_id, token_hash, created_at, last_seen_at, expires_at, revoked_at
		FROM sessions
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > $2`,
		tokenHash,
		now,
	).Scan(
		&session.ID,
		&session.OwnerID,
		&session.TokenHash,
		&session.CreatedAt,
		&session.LastSeen,
		&session.ExpiresAt,
		&session.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return controlapi.Session{}, controlapi.ErrSessionNotFound
	}
	if err != nil {
		return controlapi.Session{}, fmt.Errorf("select session: %w", err)
	}
	return session, nil
}

func (s *Store) TouchSession(ctx context.Context, sessionID string, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions
		SET last_seen_at = $2
		WHERE id = $1 AND revoked_at IS NULL AND expires_at > $2`,
		sessionID,
		now,
	)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return controlapi.ErrSessionNotFound
	}
	return nil
}

func (s *Store) RevokeSession(ctx context.Context, sessionID string, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions
		SET revoked_at = COALESCE(revoked_at, $2)
		WHERE id = $1`,
		sessionID,
		now,
	)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return controlapi.ErrSessionNotFound
	}
	return nil
}

func (s *Store) ReserveEndpoint(
	ctx context.Context,
	ownerID string,
	idempotencyKey string,
	requestHash string,
	endpoint controlapi.EndpointRecord,
) (controlapi.EndpointRecord, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return controlapi.EndpointRecord{}, false, fmt.Errorf("begin endpoint reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO endpoints (
		id, owner_id, display_name, model_id, revision, profile,
		min_replicas, max_replicas, target_queue_depth, idle_timeout_seconds,
		cache_preference, max_cold_start_fallback_seconds, endpoint_uid,
		created_at, provisioned_at, deletion_requested_at, deleted_at
	) VALUES (
		$1, $2, $3, $4, $5, $6,
		$7, $8, $9, $10,
		$11, $12, $13,
		$14, $15, $16, $17
	)`,
		endpoint.ID,
		ownerID,
		endpoint.DisplayName,
		endpoint.ModelID,
		endpoint.Revision,
		endpoint.Profile,
		endpoint.MinReplicas,
		endpoint.MaxReplicas,
		endpoint.TargetQueueDepth,
		endpoint.IdleTimeoutSeconds,
		endpoint.CachePreference,
		endpoint.MaxColdStartFallbackSeconds,
		nullableString(endpoint.EndpointUID),
		endpoint.CreatedAt,
		endpoint.ProvisionedAt,
		endpoint.DeletionRequestedAt,
		endpoint.DeletedAt,
	); err != nil {
		return controlapi.EndpointRecord{}, false, fmt.Errorf("insert endpoint reservation: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO endpoint_idempotency (
		owner_id, operation, idempotency_key, request_hash, endpoint_id, created_at
	) VALUES ($1, $2, $3, $4, $5, $6)
	ON CONFLICT (owner_id, operation, idempotency_key) DO NOTHING`,
		ownerID,
		endpointCreateOperation,
		idempotencyKey,
		requestHash,
		endpoint.ID,
		endpoint.CreatedAt,
	)
	if err != nil {
		return controlapi.EndpointRecord{}, false, fmt.Errorf("insert idempotency reservation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if err := tx.Rollback(ctx); err != nil {
			return controlapi.EndpointRecord{}, false, fmt.Errorf("rollback replay reservation: %w", err)
		}
		return s.loadIdempotentEndpoint(ctx, ownerID, idempotencyKey, requestHash)
	}
	if err := tx.Commit(ctx); err != nil {
		return controlapi.EndpointRecord{}, false, fmt.Errorf("commit endpoint reservation: %w", err)
	}
	endpoint.OwnerID = ownerID
	return endpoint, false, nil
}

func (s *Store) loadIdempotentEndpoint(ctx context.Context, ownerID, idempotencyKey, requestHash string) (controlapi.EndpointRecord, bool, error) {
	var existingHash string
	row := s.pool.QueryRow(ctx, `SELECT
		i.request_hash,
		e.id, e.owner_id, e.display_name, e.model_id, e.revision, e.profile,
		e.min_replicas, e.max_replicas, e.target_queue_depth, e.idle_timeout_seconds,
		e.cache_preference, e.max_cold_start_fallback_seconds, e.endpoint_uid,
		e.created_at, e.provisioned_at, e.deletion_requested_at, e.deleted_at
		FROM endpoint_idempotency i
		JOIN endpoints e ON e.id = i.endpoint_id
		WHERE i.owner_id = $1 AND i.operation = $2 AND i.idempotency_key = $3`,
		ownerID,
		endpointCreateOperation,
		idempotencyKey,
	)
	record, err := scanEndpointWithPrefix(row, &existingHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return controlapi.EndpointRecord{}, false, fmt.Errorf("idempotency reservation disappeared")
	}
	if err != nil {
		return controlapi.EndpointRecord{}, false, fmt.Errorf("load idempotency reservation: %w", err)
	}
	if existingHash != requestHash {
		return controlapi.EndpointRecord{}, false, controlapi.ErrIdempotencyConflict
	}
	return record, true, nil
}

func (s *Store) MarkEndpointProvisioned(ctx context.Context, ownerID, endpointID, endpointUID string, now time.Time) (controlapi.EndpointRecord, error) {
	row := s.pool.QueryRow(ctx, `UPDATE endpoints
		SET endpoint_uid = COALESCE(endpoint_uid, $3),
			provisioned_at = COALESCE(provisioned_at, $4)
		WHERE id = $1 AND owner_id = $2
			AND (endpoint_uid IS NULL OR endpoint_uid = $3)
		RETURNING `+endpointColumns,
		endpointID,
		ownerID,
		endpointUID,
		now,
	)
	record, err := scanEndpoint(row)
	return endpointResult(record, err, "mark endpoint provisioned")
}

func (s *Store) GetEndpoint(ctx context.Context, ownerID, endpointID string) (controlapi.EndpointRecord, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+endpointColumns+`
		FROM endpoints
		WHERE id = $1 AND owner_id = $2`,
		endpointID,
		ownerID,
	)
	record, err := scanEndpoint(row)
	return endpointResult(record, err, "select endpoint")
}

func (s *Store) ListEndpoints(ctx context.Context, ownerID string, limit int) ([]controlapi.EndpointRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+endpointColumns+`
		FROM endpoints
		WHERE owner_id = $1
		ORDER BY created_at DESC
		LIMIT $2`,
		ownerID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	defer rows.Close()
	records := make([]controlapi.EndpointRecord, 0)
	for rows.Next() {
		record, err := scanEndpoint(rows)
		if err != nil {
			return nil, fmt.Errorf("scan endpoint list: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read endpoint list: %w", err)
	}
	return records, nil
}

func (s *Store) MarkEndpointDeletionRequested(ctx context.Context, ownerID, endpointID string, now time.Time) (controlapi.EndpointRecord, error) {
	row := s.pool.QueryRow(ctx, `UPDATE endpoints
		SET deletion_requested_at = COALESCE(deletion_requested_at, $3)
		WHERE id = $1 AND owner_id = $2
		RETURNING `+endpointColumns,
		endpointID,
		ownerID,
		now,
	)
	record, err := scanEndpoint(row)
	return endpointResult(record, err, "mark endpoint deletion requested")
}

func (s *Store) MarkEndpointDeleted(ctx context.Context, ownerID, endpointID string, now time.Time) (controlapi.EndpointRecord, error) {
	row := s.pool.QueryRow(ctx, `UPDATE endpoints
		SET deletion_requested_at = COALESCE(deletion_requested_at, $3),
			deleted_at = COALESCE(deleted_at, $3)
		WHERE id = $1 AND owner_id = $2
		RETURNING `+endpointColumns,
		endpointID,
		ownerID,
		now,
	)
	record, err := scanEndpoint(row)
	return endpointResult(record, err, "mark endpoint deleted")
}

func (s *Store) AppendAudit(ctx context.Context, event controlapi.AuditEvent) error {
	details := event.Details
	if len(details) == 0 {
		details = json.RawMessage(`{}`)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_events (
		actor, action, endpoint_id, endpoint_uid, request_id, result, details, created_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8)`,
		event.Actor,
		event.Action,
		nullableString(event.EndpointID),
		nullableString(event.EndpointUID),
		event.RequestID,
		event.Result,
		string(details),
		event.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

func (s *Store) ListAuditEvents(ctx context.Context, ownerID, endpointID string, limit int) ([]controlapi.AuditEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT
		a.id, a.actor, a.action, COALESCE(a.endpoint_id, ''), COALESCE(a.endpoint_uid, ''),
		a.request_id, a.result, a.details, a.created_at
		FROM audit_events a
		JOIN endpoints e ON e.id = a.endpoint_id
		WHERE e.owner_id = $1 AND e.id = $2
		ORDER BY a.created_at DESC, a.id DESC
		LIMIT $3`,
		ownerID,
		endpointID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	events := make([]controlapi.AuditEvent, 0)
	for rows.Next() {
		var event controlapi.AuditEvent
		if err := rows.Scan(
			&event.ID,
			&event.Actor,
			&event.Action,
			&event.EndpointID,
			&event.EndpointUID,
			&event.RequestID,
			&event.Result,
			&event.Details,
			&event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read audit events: %w", err)
	}
	return events, nil
}

const endpointColumns = `id, owner_id, display_name, model_id, revision, profile,
	min_replicas, max_replicas, target_queue_depth, idle_timeout_seconds,
	cache_preference, max_cold_start_fallback_seconds, endpoint_uid,
	created_at, provisioned_at, deletion_requested_at, deleted_at`

func scanEndpoint(row rowScanner) (controlapi.EndpointRecord, error) {
	return scanEndpointWithPrefix(row)
}

func scanEndpointWithPrefix(row rowScanner, prefix ...any) (controlapi.EndpointRecord, error) {
	var record controlapi.EndpointRecord
	var endpointUID *string
	destinations := append(prefix,
		&record.ID,
		&record.OwnerID,
		&record.DisplayName,
		&record.ModelID,
		&record.Revision,
		&record.Profile,
		&record.MinReplicas,
		&record.MaxReplicas,
		&record.TargetQueueDepth,
		&record.IdleTimeoutSeconds,
		&record.CachePreference,
		&record.MaxColdStartFallbackSeconds,
		&endpointUID,
		&record.CreatedAt,
		&record.ProvisionedAt,
		&record.DeletionRequestedAt,
		&record.DeletedAt,
	)
	if err := row.Scan(destinations...); err != nil {
		return controlapi.EndpointRecord{}, err
	}
	if endpointUID != nil {
		record.EndpointUID = *endpointUID
	}
	return record, nil
}

func endpointResult(record controlapi.EndpointRecord, err error, operation string) (controlapi.EndpointRecord, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return controlapi.EndpointRecord{}, controlapi.ErrEndpointNotFound
	}
	if err != nil {
		return controlapi.EndpointRecord{}, fmt.Errorf("%s: %w", operation, err)
	}
	return record, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
