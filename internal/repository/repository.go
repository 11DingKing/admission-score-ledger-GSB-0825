// Package repository provides PostgreSQL-backed persistence for the admission score ledger.
// All SQL statements are centralized here; no ORM is used.
package repository

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/gsb/admission-score-ledger/internal/domain"
	"github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("record not found")

// ErrDuplicateSubmission is returned when a submission_id already exists with a different payload.
var ErrDuplicateSubmission = errors.New("duplicate submission_id with different payload")

// Querier is the common interface satisfied by both *sql.DB and *sql.Tx.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx defines the methods available within a database transaction.
type Tx interface {
	Commit() error
	Rollback() error
	AdvisoryLockKey(ctx context.Context, key domain.NaturalKey) error
	GetSubmissionByID(ctx context.Context, submissionID string) (*domain.Submission, error)
	GetCurrentSnapshot(ctx context.Context, key domain.NaturalKey) (*domain.CurrentSnapshot, error)
	InsertSubmission(ctx context.Context, s *domain.Submission) error
	UpsertCurrentSnapshot(ctx context.Context, snap *domain.CurrentSnapshot) error
	InsertAudit(ctx context.Context, rec *domain.AuditRecord) error
	InsertOutboxEvent(ctx context.Context, evt *domain.OutboxEvent) error
	ClaimOutboxEvents(ctx context.Context, limit int) ([]domain.OutboxEvent, error)
}

// RankingFilter holds optional filters for the rankings query.
type RankingFilter struct {
	ProvinceCode  string
	AdmissionYear int
	BatchCode     string
	Limit         int
}

// queries holds all SQL-bound methods and is shared by Repo and TxRepo.
type queries struct {
	q Querier
}

// Repo is the root repository backed by a *sql.DB connection pool.
type Repo struct {
	db *sql.DB
	*queries
}

// TxRepo is a repository bound to a single database transaction.
type TxRepo struct {
	tx *sql.Tx
	*queries
}

// NewRepo creates a new Repo using the given database connection pool.
func NewRepo(db *sql.DB) *Repo {
	return &Repo{
		db:      db,
		queries: &queries{q: db},
	}
}

// DB returns the underlying *sql.DB.
func (r *Repo) DB() *sql.DB {
	return r.db
}

// BeginTx starts a new transaction and returns a Tx bound to it.
func (r *Repo) BeginTx(ctx context.Context) (Tx, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	return &TxRepo{
		tx:      tx,
		queries: &queries{q: tx},
	}, nil
}

// Commit commits the transaction.
func (t *TxRepo) Commit() error {
	return t.tx.Commit()
}

// Rollback aborts the transaction.
func (t *TxRepo) Rollback() error {
	return t.tx.Rollback()
}

// AdvisoryLockKey acquires a transaction-scoped advisory lock for the given natural key.
// The lock is automatically released when the transaction commits or rolls back.
func (t *TxRepo) AdvisoryLockKey(ctx context.Context, key domain.NaturalKey) error {
	lockStr := fmt.Sprintf("%s|%d|%s|%s|%s",
		key.ProvinceCode, key.AdmissionYear, key.BatchCode, key.SchoolCode, key.MajorGroupCode)
	_, err := t.tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", lockStr)
	if err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	return nil
}

// GetSubmissionByID retrieves a submission by its submission_id.
func (q *queries) GetSubmissionByID(ctx context.Context, submissionID string) (*domain.Submission, error) {
	const query = `
		SELECT id, submission_id, province_code, admission_year, batch_code,
		       school_code, major_group_code, score_scale, score_value,
		       submitted_at, rule_version, source_revision, status, created_at
		  FROM submissions
		 WHERE submission_id = $1`
	s := &domain.Submission{}
	var scale, status string
	err := q.q.QueryRowContext(ctx, query, submissionID).Scan(
		&s.ID, &s.SubmissionID, &s.ProvinceCode, &s.AdmissionYear, &s.BatchCode,
		&s.SchoolCode, &s.MajorGroupCode, &scale, &s.ScoreValue,
		&s.SubmittedAt, &s.RuleVersion, &s.SourceRevision, &status, &s.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get submission by id: %w", err)
	}
	s.ScoreScale = domain.ScoreScale(scale)
	s.Status = domain.SubmissionStatus(status)
	return s, nil
}

// GetCurrentSnapshot retrieves the current snapshot for a natural key.
func (q *queries) GetCurrentSnapshot(ctx context.Context, key domain.NaturalKey) (*domain.CurrentSnapshot, error) {
	const query = `
		SELECT province_code, admission_year, batch_code, school_code, major_group_code,
		       score_scale, score_value, submitted_at, rule_version,
		       source_revision, submission_id, accepted_at
		  FROM current_snapshots
		 WHERE province_code = $1 AND admission_year = $2 AND batch_code = $3
		   AND school_code = $4 AND major_group_code = $5`
	s := &domain.CurrentSnapshot{}
	var scale string
	err := q.q.QueryRowContext(ctx, query,
		key.ProvinceCode, key.AdmissionYear, key.BatchCode,
		key.SchoolCode, key.MajorGroupCode,
	).Scan(
		&s.ProvinceCode, &s.AdmissionYear, &s.BatchCode, &s.SchoolCode, &s.MajorGroupCode,
		&scale, &s.ScoreValue, &s.SubmittedAt, &s.RuleVersion,
		&s.SourceRevision, &s.SubmissionID, &s.AcceptedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get current snapshot: %w", err)
	}
	s.ScoreScale = domain.ScoreScale(scale)
	return s, nil
}

// InsertSubmission inserts a new submission record. The ID and CreatedAt are populated on success.
func (q *queries) InsertSubmission(ctx context.Context, s *domain.Submission) error {
	const query = `
		INSERT INTO submissions (
		    submission_id, province_code, admission_year, batch_code, school_code,
		    major_group_code, score_scale, score_value, submitted_at, rule_version,
		    source_revision, status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING id, created_at`
	err := q.q.QueryRowContext(ctx, query,
		s.SubmissionID, s.ProvinceCode, s.AdmissionYear, s.BatchCode, s.SchoolCode,
		s.MajorGroupCode, string(s.ScoreScale), s.ScoreValue, s.SubmittedAt, s.RuleVersion,
		s.SourceRevision, string(s.Status),
	).Scan(&s.ID, &s.CreatedAt)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return ErrDuplicateSubmission
		}
		return fmt.Errorf("insert submission: %w", err)
	}
	return nil
}

// UpsertCurrentSnapshot inserts or updates the current snapshot for a natural key.
func (q *queries) UpsertCurrentSnapshot(ctx context.Context, snap *domain.CurrentSnapshot) error {
	const query = `
		INSERT INTO current_snapshots (
		    province_code, admission_year, batch_code, school_code, major_group_code,
		    score_scale, score_value, submitted_at, rule_version,
		    source_revision, submission_id, accepted_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (province_code, admission_year, batch_code, school_code, major_group_code)
		DO UPDATE SET
		    score_scale     = EXCLUDED.score_scale,
		    score_value     = EXCLUDED.score_value,
		    submitted_at    = EXCLUDED.submitted_at,
		    rule_version    = EXCLUDED.rule_version,
		    source_revision = EXCLUDED.source_revision,
		    submission_id   = EXCLUDED.submission_id,
		    accepted_at     = EXCLUDED.accepted_at`
	_, err := q.q.ExecContext(ctx, query,
		snap.ProvinceCode, snap.AdmissionYear, snap.BatchCode, snap.SchoolCode, snap.MajorGroupCode,
		string(snap.ScoreScale), snap.ScoreValue, snap.SubmittedAt, snap.RuleVersion,
		snap.SourceRevision, snap.SubmissionID, snap.AcceptedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert current snapshot: %w", err)
	}
	return nil
}

// InsertAudit writes an append-only audit record. The ID and CreatedAt are populated on success.
func (q *queries) InsertAudit(ctx context.Context, rec *domain.AuditRecord) error {
	const query = `
		INSERT INTO audit_log (
		    submission_id, action, province_code, admission_year, batch_code,
		    school_code, major_group_code, old_revision, new_revision,
		    old_score, new_score, reason, trace_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id, created_at`
	err := q.q.QueryRowContext(ctx, query,
		rec.SubmissionID, string(rec.Action), rec.ProvinceCode, rec.AdmissionYear, rec.BatchCode,
		rec.SchoolCode, rec.MajorGroupCode, rec.OldRevision, rec.NewRevision,
		rec.OldScore, rec.NewScore, rec.Reason, rec.TraceID,
	).Scan(&rec.ID, &rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}

// InsertOutboxEvent writes a transactional outbox event. The ID and CreatedAt are populated.
func (q *queries) InsertOutboxEvent(ctx context.Context, evt *domain.OutboxEvent) error {
	const query = `
		INSERT INTO outbox (event_type, aggregate_type, aggregate_id, payload)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`
	err := q.q.QueryRowContext(ctx, query,
		evt.EventType, evt.AggregateType, evt.AggregateID, evt.Payload,
	).Scan(&evt.ID, &evt.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// ListRankings returns the current highest-score rankings ordered by
// score_value DESC, submitted_at ASC, school_code ASC, major_group_code ASC.
func (q *queries) ListRankings(ctx context.Context, filter RankingFilter) ([]domain.RankingEntry, error) {
	query := `
		SELECT province_code, admission_year, batch_code, school_code, major_group_code,
		       score_scale, score_value, submitted_at, source_revision
		  FROM current_snapshots`
	var args []any
	argIdx := 1
	if filter.ProvinceCode != "" || filter.AdmissionYear != 0 || filter.BatchCode != "" {
		query += " WHERE 1=1"
		if filter.ProvinceCode != "" {
			query += fmt.Sprintf(" AND province_code = $%d", argIdx)
			args = append(args, filter.ProvinceCode)
			argIdx++
		}
		if filter.AdmissionYear != 0 {
			query += fmt.Sprintf(" AND admission_year = $%d", argIdx)
			args = append(args, filter.AdmissionYear)
			argIdx++
		}
		if filter.BatchCode != "" {
			query += fmt.Sprintf(" AND batch_code = $%d", argIdx)
			args = append(args, filter.BatchCode)
			argIdx++
		}
	}
	query += `
	  ORDER BY score_value DESC, submitted_at ASC, school_code ASC, major_group_code ASC`
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argIdx)
		args = append(args, filter.Limit)
	}

	rows, err := q.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list rankings: %w", err)
	}
	defer rows.Close()

	var results []domain.RankingEntry
	for rows.Next() {
		var e domain.RankingEntry
		var scale string
		if err := rows.Scan(
			&e.ProvinceCode, &e.AdmissionYear, &e.BatchCode, &e.SchoolCode, &e.MajorGroupCode,
			&scale, &e.ScoreValue, &e.SubmittedAt, &e.SourceRevision,
		); err != nil {
			return nil, fmt.Errorf("scan ranking: %w", err)
		}
		e.ScoreScale = domain.ScoreScale(scale)
		e.ScoreDisplay = domain.FormatScore(e.ScoreScale, e.ScoreValue)
		results = append(results, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rankings rows: %w", err)
	}
	return results, nil
}

// ListHistory returns all submissions for a natural key, oldest first.
func (q *queries) ListHistory(ctx context.Context, key domain.NaturalKey) ([]domain.HistoryEntry, error) {
	const query = `
		SELECT submission_id, score_scale, score_value, submitted_at, rule_version,
		       source_revision, status, created_at
		  FROM submissions
		 WHERE province_code = $1 AND admission_year = $2 AND batch_code = $3
		   AND school_code = $4 AND major_group_code = $5
	  ORDER BY source_revision ASC, created_at ASC`
	rows, err := q.q.QueryContext(ctx, query,
		key.ProvinceCode, key.AdmissionYear, key.BatchCode,
		key.SchoolCode, key.MajorGroupCode,
	)
	if err != nil {
		return nil, fmt.Errorf("list history: %w", err)
	}
	defer rows.Close()

	var results []domain.HistoryEntry
	for rows.Next() {
		var e domain.HistoryEntry
		var scale, status string
		if err := rows.Scan(
			&e.SubmissionID, &scale, &e.ScoreValue, &e.SubmittedAt, &e.RuleVersion,
			&e.SourceRevision, &status, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		e.ScoreScale = domain.ScoreScale(scale)
		e.Status = domain.SubmissionStatus(status)
		e.ScoreDisplay = domain.FormatScore(e.ScoreScale, e.ScoreValue)
		results = append(results, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history rows: %w", err)
	}
	return results, nil
}

// CountOutboxUnpublished returns the number of unpublished outbox events.
func (q *queries) CountOutboxUnpublished(ctx context.Context) (int64, error) {
	const query = `SELECT COUNT(*) FROM outbox WHERE published_at IS NULL`
	var count int64
	if err := q.q.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("count outbox: %w", err)
	}
	return count, nil
}

// ClaimOutboxEvents atomically marks up to limit unpublished events as published
// and returns them. It must be called within a transaction.
func (q *queries) ClaimOutboxEvents(ctx context.Context, limit int) ([]domain.OutboxEvent, error) {
	const query = `
		UPDATE outbox SET published_at = NOW()
		 WHERE id IN (
		     SELECT id FROM outbox WHERE published_at IS NULL
		     ORDER BY created_at ASC LIMIT $1
		 )
		 RETURNING id, event_type, aggregate_type, aggregate_id, payload, created_at, published_at`
	rows, err := q.q.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	defer rows.Close()

	var events []domain.OutboxEvent
	for rows.Next() {
		var e domain.OutboxEvent
		if err := rows.Scan(
			&e.ID, &e.EventType, &e.AggregateType, &e.AggregateID,
			&e.Payload, &e.CreatedAt, &e.PublishedAt,
		); err != nil {
			return nil, fmt.Errorf("scan outbox: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox rows: %w", err)
	}
	return events, nil
}

// RunMigrations executes all embedded SQL migration files in order.
func RunMigrations(ctx context.Context, db *sql.DB) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	var upFiles []string
	for _, e := range entries {
		name := e.Name()
		if len(name) > 7 && name[len(name)-7:] == ".up.sql" {
			upFiles = append(upFiles, name)
		}
	}
	sort.Strings(upFiles)

	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, name := range upFiles {
		var version int
		if _, err := fmt.Sscanf(name, "%d_", &version); err != nil {
			return fmt.Errorf("parse migration version from %s: %w", name, err)
		}

		var exists bool
		if err := db.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)", version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %d: %w", version, err)
		}
		if exists {
			continue
		}

		sqlBytes, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration tx %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING", version,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}
	}
	return nil
}

// Now returns the current UTC time. It is a variable so tests can override it.
var Now = func() time.Time {
	return time.Now().UTC()
}
