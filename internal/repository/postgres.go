// Package repository contains all SQL access for the admission score ledger.
// No ORM is used: every statement lives in this package and uses positional
// parameter binding.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"admission-score-ledger/internal/domain"
)

// ErrSubmissionConflict is returned when a submission_id is reused with a
// payload whose hash differs from the first submission carrying that id.
var ErrSubmissionConflict = errors.New("submission_id reused with a different payload")

// querier is satisfied by both pgx.Tx and *pgxpool.Pool.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SubmitInput carries a submission together with its precomputed payload hash
// and the trace id of the request that produced it.
type SubmitInput struct {
	Submission  domain.Submission
	PayloadHash string
	TraceID     string
}

// SubmitResult is the outcome of persisting one submission.
type SubmitResult struct {
	// Decision is ACCEPTED or STALE_IGNORED.
	Decision domain.Decision
	// Replayed is true when an identical submission_id+payload was already processed.
	Replayed bool
	// Current is the snapshot that is current after processing.
	Current *domain.Snapshot
	// Received is the record describing the submitted payload.
	Received *domain.HistoryRecord
	// RecordedAt is when the record was first stored.
	RecordedAt time.Time
}

// PostgresRepository stores snapshots, history, audit and outbox events in
// PostgreSQL. All multi-row writes happen inside a single transaction.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository builds a repository backed by the given pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

// Pool returns the underlying connection pool.
func (r *PostgresRepository) Pool() *pgxpool.Pool {
	return r.pool
}

// Ping verifies database connectivity.
func (r *PostgresRepository) Ping(ctx context.Context) error {
	return r.pool.Ping(ctx)
}

// Close releases the connection pool.
func (r *PostgresRepository) Close() {
	r.pool.Close()
}

// Migrate runs all pending schema migrations.
func (r *PostgresRepository) Migrate(ctx context.Context) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := Migrate(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Submit processes one submission. The snapshot upsert, the append-only
// history record, the audit row and the outbox event are all written in one
// transaction; no external system is contacted before commit.
func (r *PostgresRepository) Submit(ctx context.Context, in SubmitInput) (SubmitResult, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return SubmitResult{}, fmt.Errorf("begin tx: %w", err)
		}
		result, err := ApplyInTx(ctx, tx, in)
		if err != nil {
			if errors.Is(err, ErrSubmissionConflict) {
				if commitErr := tx.Commit(ctx); commitErr != nil {
					return SubmitResult{}, fmt.Errorf("commit conflict audit: %w", commitErr)
				}
				return SubmitResult{}, err
			}
			_ = tx.Rollback(ctx)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "submission_records_submission_id_key" {
				lastErr = err
				continue
			}
			return SubmitResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			lastErr = err
			continue
		}
		return result, nil
	}
	return SubmitResult{}, fmt.Errorf("submit failed after retries: %w", lastErr)
}

// ApplyInTx runs the full submission logic inside the provided transaction.
// It is exported so tests can drive the transactional boundary directly
// (for example to assert rollback behaviour).
func ApplyInTx(ctx context.Context, tx pgx.Tx, in SubmitInput) (SubmitResult, error) {
	sub := in.Submission

	var existingStatus string
	var existingScale string
	var existingValue int64
	var existingRevision int64
	var existingHash string
	var existingCreatedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT status, score_scale, score_value, source_revision, payload_hash, created_at
		FROM submission_records
		WHERE submission_id = $1`, sub.SubmissionID).
		Scan(&existingStatus, &existingScale, &existingValue, &existingRevision, &existingHash, &existingCreatedAt)
	if err == nil {
		if existingHash == in.PayloadHash {
			return replayResult(ctx, tx, in, existingStatus, existingCreatedAt)
		}
		if err := writeAudit(ctx, tx, auditEntry{
			eventType:  "SUBMISSION_CONFLICT",
			decision:   "CONFLICT",
			submission: sub,
			traceID:    in.TraceID,
			detail: map[string]any{
				"reason":              "idempotency_key_payload_mismatch",
				"stored_payload_hash": existingHash,
				"incoming_hash":       in.PayloadHash,
			},
		}); err != nil {
			return SubmitResult{}, err
		}
		return SubmitResult{}, ErrSubmissionConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SubmitResult{}, fmt.Errorf("lookup submission: %w", err)
	}

	lockKey := strings.Join([]string{
		sub.ProvinceCode, fmt.Sprint(sub.AdmissionYear), sub.BatchCode, sub.SchoolCode, sub.MajorGroupCode,
	}, "|")
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return SubmitResult{}, fmt.Errorf("acquire natural key lock: %w", err)
	}

	cur, err := loadSnapshot(ctx, tx, sub.Key())
	if err != nil {
		return SubmitResult{}, err
	}

	if cur != nil && sub.SourceRevision <= cur.SourceRevision {
		return applyStale(ctx, tx, in, cur)
	}
	return applyAccepted(ctx, tx, in, cur)
}

func replayResult(ctx context.Context, q querier, in SubmitInput, status string, recordedAt time.Time) (SubmitResult, error) {
	sub := in.Submission
	cur, err := loadSnapshot(ctx, q, sub.Key())
	if err != nil {
		return SubmitResult{}, err
	}
	decision := domain.Decision(status)
	return SubmitResult{
		Decision: decision,
		Replayed: true,
		Current:  cur,
		Received: &domain.HistoryRecord{
			SubmissionID:   sub.SubmissionID,
			SourceRevision: sub.SourceRevision,
			ScoreScale:     sub.ScoreScale,
			ScoreValue:     sub.ScoreValue,
			ScoreDisplay:   domain.FormatScore(sub.ScoreScale, sub.ScoreValue),
			Status:         decision,
			RuleVersion:    sub.RuleVersion,
			SubmittedAt:    sub.SubmittedAt,
			RecordedAt:     recordedAt,
		},
		RecordedAt: recordedAt,
	}, nil
}

func applyStale(ctx context.Context, tx pgx.Tx, in SubmitInput, cur *domain.Snapshot) (SubmitResult, error) {
	sub := in.Submission
	recordedAt, err := insertRecord(ctx, tx, in, domain.DecisionStaleIgnored)
	if err != nil {
		return SubmitResult{}, err
	}
	if err := writeAudit(ctx, tx, auditEntry{
		eventType:  "SUBMISSION_STALE_IGNORED",
		decision:   "STALE_IGNORED",
		submission: sub,
		traceID:    in.TraceID,
		detail: map[string]any{
			"reason":            "source_revision_not_newer",
			"current_revision":  cur.SourceRevision,
			"incoming_revision": sub.SourceRevision,
			"current_score":     cur.ScoreValue,
			"incoming_score":    sub.ScoreValue,
		},
	}); err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{
		Decision:   domain.DecisionStaleIgnored,
		Current:    cur,
		Received:   buildRecord(in, domain.DecisionStaleIgnored, recordedAt),
		RecordedAt: recordedAt,
	}, nil
}

func applyAccepted(ctx context.Context, tx pgx.Tx, in SubmitInput, old *domain.Snapshot) (SubmitResult, error) {
	sub := in.Submission
	if _, err := tx.Exec(ctx, `
		INSERT INTO current_snapshots
			(province_code, admission_year, batch_code, school_code, major_group_code,
			 score_scale, score_value, source_revision, rule_version, last_submission_id, submitted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (province_code, admission_year, batch_code, school_code, major_group_code)
		DO UPDATE SET
			score_scale       = EXCLUDED.score_scale,
			score_value       = EXCLUDED.score_value,
			source_revision   = EXCLUDED.source_revision,
			rule_version      = EXCLUDED.rule_version,
			last_submission_id= EXCLUDED.last_submission_id,
			submitted_at      = EXCLUDED.submitted_at,
			updated_at        = now()`,
		sub.ProvinceCode, sub.AdmissionYear, sub.BatchCode, sub.SchoolCode, sub.MajorGroupCode,
		string(sub.ScoreScale), sub.ScoreValue, sub.SourceRevision, sub.RuleVersion,
		sub.SubmissionID, sub.SubmittedAt); err != nil {
		return SubmitResult{}, fmt.Errorf("upsert snapshot: %w", err)
	}

	recordedAt, err := insertRecord(ctx, tx, in, domain.DecisionAccepted)
	if err != nil {
		return SubmitResult{}, err
	}

	if err := writeAudit(ctx, tx, auditEntry{
		eventType:  "SUBMISSION_ACCEPTED",
		decision:   "ACCEPTED",
		submission: sub,
		traceID:    in.TraceID,
		detail: map[string]any{
			"old": snapshotDetail(old),
			"new": map[string]any{
				"score_scale":     string(sub.ScoreScale),
				"score_value":     sub.ScoreValue,
				"score_display":   domain.FormatScore(sub.ScoreScale, sub.ScoreValue),
				"source_revision": sub.SourceRevision,
			},
		},
	}); err != nil {
		return SubmitResult{}, err
	}

	if err := insertOutbox(ctx, tx, old, sub, in.TraceID); err != nil {
		return SubmitResult{}, err
	}

	current, err := loadSnapshot(ctx, tx, sub.Key())
	if err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{
		Decision:   domain.DecisionAccepted,
		Current:    current,
		Received:   buildRecord(in, domain.DecisionAccepted, recordedAt),
		RecordedAt: recordedAt,
	}, nil
}

func insertRecord(ctx context.Context, q querier, in SubmitInput, status domain.Decision) (time.Time, error) {
	sub := in.Submission
	var recordedAt time.Time
	err := q.QueryRow(ctx, `
		INSERT INTO submission_records
			(submission_id, province_code, admission_year, batch_code, school_code, major_group_code,
			 score_scale, score_value, source_revision, rule_version, submitted_at, status, payload_hash, trace_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING created_at`,
		sub.SubmissionID, sub.ProvinceCode, sub.AdmissionYear, sub.BatchCode, sub.SchoolCode,
		sub.MajorGroupCode, string(sub.ScoreScale), sub.ScoreValue, sub.SourceRevision,
		sub.RuleVersion, sub.SubmittedAt, string(status), in.PayloadHash, in.TraceID).
		Scan(&recordedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("insert submission record: %w", err)
	}
	return recordedAt, nil
}

func buildRecord(in SubmitInput, status domain.Decision, recordedAt time.Time) *domain.HistoryRecord {
	sub := in.Submission
	return &domain.HistoryRecord{
		SubmissionID:   sub.SubmissionID,
		SourceRevision: sub.SourceRevision,
		ScoreScale:     sub.ScoreScale,
		ScoreValue:     sub.ScoreValue,
		ScoreDisplay:   domain.FormatScore(sub.ScoreScale, sub.ScoreValue),
		Status:         status,
		RuleVersion:    sub.RuleVersion,
		SubmittedAt:    sub.SubmittedAt,
		RecordedAt:     recordedAt,
	}
}

type auditEntry struct {
	eventType  string
	decision   string
	submission domain.Submission
	traceID    string
	detail     map[string]any
}

func writeAudit(ctx context.Context, q querier, e auditEntry) error {
	detail, err := json.Marshal(e.detail)
	if err != nil {
		return fmt.Errorf("marshal audit detail: %w", err)
	}
	sub := e.submission
	_, err = q.Exec(ctx, `
		INSERT INTO audit_log
			(event_type, decision, submission_id, province_code, admission_year, batch_code,
			 school_code, major_group_code, source_revision, detail, trace_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11)`,
		e.eventType, e.decision, sub.SubmissionID, sub.ProvinceCode, sub.AdmissionYear,
		sub.BatchCode, sub.SchoolCode, sub.MajorGroupCode, sub.SourceRevision, string(detail), e.traceID)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

func insertOutbox(ctx context.Context, q querier, old *domain.Snapshot, sub domain.Submission, traceID string) error {
	payload := map[string]any{
		"event_type":      "ranking.changed",
		"occurred_at":     time.Now().UTC(),
		"trace_id":        traceID,
		"natural_key":     sub.Key(),
		"submission_id":   sub.SubmissionID,
		"source_revision": sub.SourceRevision,
		"old":             snapshotDetail(old),
		"new": map[string]any{
			"score_scale":     string(sub.ScoreScale),
			"score_value":     sub.ScoreValue,
			"score_display":   domain.FormatScore(sub.ScoreScale, sub.ScoreValue),
			"source_revision": sub.SourceRevision,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	aggregateKey := sub.Key().String()
	_, err = q.Exec(ctx, `
		INSERT INTO outbox_events (event_type, aggregate_key, payload)
		VALUES ('ranking.changed', $1, $2::jsonb)`,
		aggregateKey, string(body))
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

func snapshotDetail(s *domain.Snapshot) map[string]any {
	if s == nil {
		return nil
	}
	return map[string]any{
		"score_scale":     string(s.ScoreScale),
		"score_value":     s.ScoreValue,
		"score_display":   s.ScoreDisplay,
		"source_revision": s.SourceRevision,
	}
}

func loadSnapshot(ctx context.Context, q querier, key domain.NaturalKey) (*domain.Snapshot, error) {
	var s domain.Snapshot
	var scale string
	err := q.QueryRow(ctx, `
		SELECT province_code, admission_year, batch_code, school_code, major_group_code,
		       score_scale, score_value, source_revision, rule_version, submitted_at,
		       last_submission_id, updated_at
		FROM current_snapshots
		WHERE province_code = $1 AND admission_year = $2 AND batch_code = $3
		  AND school_code = $4 AND major_group_code = $5`,
		key.ProvinceCode, key.AdmissionYear, key.BatchCode, key.SchoolCode, key.MajorGroupCode).
		Scan(&s.ProvinceCode, &s.AdmissionYear, &s.BatchCode, &s.SchoolCode, &s.MajorGroupCode,
			&scale, &s.ScoreValue, &s.SourceRevision, &s.RuleVersion, &s.SubmittedAt,
			&s.SubmissionID, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load snapshot: %w", err)
	}
	s.ScoreScale = domain.ScoreScale(scale)
	s.ScoreDisplay = domain.FormatScore(s.ScoreScale, s.ScoreValue)
	return &s, nil
}

// RankingFilter narrows a ranking query to one province, year and optionally
// one admission batch.
type RankingFilter struct {
	ProvinceCode  string
	AdmissionYear int
	BatchCode     string
}

// ListRankings returns the current ranking rows for a filter, ordered by the
// deterministic ranking rule (score desc, then submitted_at / school_code /
// major_group_code asc).
func (r *PostgresRepository) ListRankings(ctx context.Context, f RankingFilter) ([]domain.RankingRow, error) {
	query := `
		SELECT school_code, major_group_code, score_scale, score_value, source_revision,
		       rule_version, submitted_at
		FROM current_snapshots
		WHERE province_code = $1 AND admission_year = $2`
	args := []any{f.ProvinceCode, f.AdmissionYear}
	if f.BatchCode != "" {
		query += ` AND batch_code = $3`
		args = append(args, f.BatchCode)
	}
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query rankings: %w", err)
	}
	defer rows.Close()

	var result []domain.RankingRow
	for rows.Next() {
		var row domain.RankingRow
		var scale string
		if err := rows.Scan(&row.SchoolCode, &row.MajorGroupCode, &scale, &row.ScoreValue,
			&row.SourceRevision, &row.RuleVersion, &row.SubmittedAt); err != nil {
			return nil, fmt.Errorf("scan ranking row: %w", err)
		}
		row.ScoreScale = domain.ScoreScale(scale)
		row.ScoreDisplay = domain.FormatScore(row.ScoreScale, row.ScoreValue)
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rankings: %w", err)
	}
	domain.SortRankings(result)
	return result, nil
}

// ListHistory returns every stored record for a natural key in revision order,
// including STALE_IGNORED records.
func (r *PostgresRepository) ListHistory(ctx context.Context, key domain.NaturalKey) ([]domain.HistoryRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT submission_id, source_revision, score_scale, score_value, status,
		       rule_version, submitted_at, created_at
		FROM submission_records
		WHERE province_code = $1 AND admission_year = $2 AND batch_code = $3
		  AND school_code = $4 AND major_group_code = $5
		ORDER BY source_revision ASC, id ASC`,
		key.ProvinceCode, key.AdmissionYear, key.BatchCode, key.SchoolCode, key.MajorGroupCode)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()

	var result []domain.HistoryRecord
	for rows.Next() {
		var rec domain.HistoryRecord
		var scale, status string
		if err := rows.Scan(&rec.SubmissionID, &rec.SourceRevision, &scale, &rec.ScoreValue,
			&status, &rec.RuleVersion, &rec.SubmittedAt, &rec.RecordedAt); err != nil {
			return nil, fmt.Errorf("scan history row: %w", err)
		}
		rec.ScoreScale = domain.ScoreScale(scale)
		rec.ScoreDisplay = domain.FormatScore(rec.ScoreScale, rec.ScoreValue)
		rec.Status = domain.Decision(status)
		result = append(result, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate history: %w", err)
	}
	return result, nil
}
