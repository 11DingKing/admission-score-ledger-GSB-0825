// Package service implements the admission score ledger business rules:
// request validation, idempotency hashing and orchestration of the
// repository transaction.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"admission-score-ledger/internal/domain"
	"admission-score-ledger/internal/repository"
)

// ErrConflict is returned when an idempotency key is reused with a different
// payload.
var ErrConflict = errors.New("submission_id reused with a different payload")

// FieldError describes a single validation failure.
type FieldError struct {
	// Field is the JSON name of the offending field.
	Field string `json:"field"`
	// Reason explains why the value was rejected.
	Reason string `json:"reason"`
}

// ValidationError aggregates all field-level validation failures.
type ValidationError struct {
	// Fields lists every rejected field.
	Fields []FieldError `json:"fields"`
}

// Error implements the error interface.
func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Reason)
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// Ledger validates submissions and coordinates persistence.
type Ledger struct {
	repo *repository.PostgresRepository
}

// NewLedger builds a Ledger backed by the given repository.
func NewLedger(repo *repository.PostgresRepository) *Ledger {
	return &Ledger{repo: repo}
}

// Result is the outcome of a submission request.
type Result struct {
	// Decision is ACCEPTED or STALE_IGNORED.
	Decision domain.Decision
	// Replayed reports whether the request was an idempotent replay.
	Replayed bool
	// Current is the snapshot current after processing.
	Current *domain.Snapshot
	// Received is the stored record describing the submitted payload.
	Received *domain.HistoryRecord
}

const (
	maxIDLength      = 64
	maxCodeLength    = 32
	maxGroupLength   = 64
	maxRuleLength    = 32
	minAdmissionYear = 2000
	maxAdmissionYear = 2100
)

// Validate checks a submission against the ledger's input rules.
func Validate(sub domain.Submission) []FieldError {
	var errs []FieldError
	requireText := func(field, value string, max int) {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, FieldError{Field: field, Reason: "is required"})
		} else if utf8.RuneCountInString(value) > max {
			errs = append(errs, FieldError{Field: field, Reason: fmt.Sprintf("must be at most %d characters", max)})
		}
	}

	requireText("submission_id", sub.SubmissionID, maxIDLength)
	requireText("province_code", sub.ProvinceCode, maxCodeLength)
	if sub.AdmissionYear < minAdmissionYear || sub.AdmissionYear > maxAdmissionYear {
		errs = append(errs, FieldError{Field: "admission_year", Reason: fmt.Sprintf("must be between %d and %d", minAdmissionYear, maxAdmissionYear)})
	}
	requireText("batch_code", sub.BatchCode, maxCodeLength)
	requireText("school_code", sub.SchoolCode, maxCodeLength)
	requireText("major_group_code", sub.MajorGroupCode, maxGroupLength)
	switch sub.ScoreScale {
	case domain.ScaleInteger, domain.ScaleDecimal1:
	default:
		errs = append(errs, FieldError{Field: "score_scale", Reason: "must be INTEGER or DECIMAL_1"})
	}
	if sub.ScoreValue < 0 {
		errs = append(errs, FieldError{Field: "score_value", Reason: "must be >= 0 and stored as tenths of a point"})
	}
	if sub.ScoreScale == domain.ScaleInteger && sub.ScoreValue%10 != 0 {
		errs = append(errs, FieldError{Field: "score_value", Reason: "INTEGER scale requires a multiple of 10 tenths"})
	}
	if sub.SourceRevision < 1 {
		errs = append(errs, FieldError{Field: "source_revision", Reason: "must be >= 1"})
	}
	requireText("rule_version", sub.RuleVersion, maxRuleLength)
	if sub.SubmittedAt.IsZero() {
		errs = append(errs, FieldError{Field: "submitted_at", Reason: "is required and must be an RFC3339 timestamp"})
	}
	return errs
}

// Submit validates and persists a submission. The payload hash is computed
// from the canonical submission so idempotent retries can be detected.
func (l *Ledger) Submit(ctx context.Context, sub domain.Submission, traceID string) (Result, error) {
	if errs := Validate(sub); len(errs) > 0 {
		return Result{}, &ValidationError{Fields: errs}
	}
	res, err := l.repo.Submit(ctx, repository.SubmitInput{
		Submission:  sub,
		PayloadHash: domain.PayloadHash(sub),
		TraceID:     traceID,
	})
	if err != nil {
		if errors.Is(err, repository.ErrSubmissionConflict) {
			return Result{}, ErrConflict
		}
		return Result{}, fmt.Errorf("persist submission: %w", err)
	}
	return Result{
		Decision: res.Decision,
		Replayed: res.Replayed,
		Current:  res.Current,
		Received: res.Received,
	}, nil
}

// Rankings returns the ordered ranking for the given filter.
func (l *Ledger) Rankings(ctx context.Context, f repository.RankingFilter) ([]domain.RankingRow, error) {
	rows, err := l.repo.ListRankings(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("list rankings: %w", err)
	}
	return rows, nil
}

// History returns the immutable change records of one natural key.
func (l *Ledger) History(ctx context.Context, key domain.NaturalKey) ([]domain.HistoryRecord, error) {
	records, err := l.repo.ListHistory(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("list history: %w", err)
	}
	return records, nil
}
