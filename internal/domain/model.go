// Package domain defines the core domain models for the admission score ledger.
package domain

import (
	"fmt"
	"strings"
	"time"
)

// ScoreScale represents the scale used for a score value.
type ScoreScale string

const (
	// ScoreScaleInteger indicates scores are whole integers stored in tenths (e.g. 6100 = 610).
	ScoreScaleInteger ScoreScale = "INTEGER"
	// ScoreScaleDecimal1 indicates scores have one decimal place stored in tenths (e.g. 6044 = 604.4).
	ScoreScaleDecimal1 ScoreScale = "DECIMAL_1"
)

// Valid reports whether s is a known ScoreScale.
func (s ScoreScale) Valid() bool {
	switch s {
	case ScoreScaleInteger, ScoreScaleDecimal1:
		return true
	}
	return false
}

// SubmissionStatus represents the processing outcome of a submission.
type SubmissionStatus string

const (
	// SubmissionStatusAccepted means the submission became the current snapshot.
	SubmissionStatusAccepted SubmissionStatus = "ACCEPTED"
	// SubmissionStatusStaleIgnored means the submission had an older source_revision and was ignored.
	SubmissionStatusStaleIgnored SubmissionStatus = "STALE_IGNORED"
)

// AuditAction represents an action recorded in the append-only audit log.
type AuditAction string

const (
	// AuditActionAccepted records that a submission became the current snapshot.
	AuditActionAccepted AuditAction = "ACCEPTED"
	// AuditActionStaleIgnored records that a stale submission was ignored.
	AuditActionStaleIgnored AuditAction = "STALE_IGNORED"
	// AuditActionConflict records that an idempotency conflict was detected.
	AuditActionConflict AuditAction = "CONFLICT"
)

// Submission is a single score snapshot received from an external source.
type Submission struct {
	ID             int64
	SubmissionID   string
	ProvinceCode   string
	AdmissionYear  int
	BatchCode      string
	SchoolCode     string
	MajorGroupCode string
	ScoreScale     ScoreScale
	ScoreValue     int64
	SubmittedAt    time.Time
	RuleVersion    string
	SourceRevision int32
	Status         SubmissionStatus
	CreatedAt      time.Time
}

// NaturalKey returns the composite natural key that identifies a school major group
// within a province, year, and batch.
func (s Submission) NaturalKey() NaturalKey {
	return NaturalKey{
		ProvinceCode:   s.ProvinceCode,
		AdmissionYear:  s.AdmissionYear,
		BatchCode:      s.BatchCode,
		SchoolCode:     s.SchoolCode,
		MajorGroupCode: s.MajorGroupCode,
	}
}

// NaturalKey identifies a unique school major group slot.
type NaturalKey struct {
	ProvinceCode   string
	AdmissionYear  int
	BatchCode      string
	SchoolCode     string
	MajorGroupCode string
}

// CurrentSnapshot holds the currently accepted score for a natural key.
type CurrentSnapshot struct {
	NaturalKey
	ScoreScale     ScoreScale
	ScoreValue     int64
	SubmittedAt    time.Time
	RuleVersion    string
	SourceRevision int32
	SubmissionID   string
	AcceptedAt     time.Time
}

// FormatScore returns the human-readable representation of scoreValue based on scale.
// INTEGER: 6100 -> "610"; DECIMAL_1: 6044 -> "604.4".
func FormatScore(scale ScoreScale, value int64) string {
	switch scale {
	case ScoreScaleDecimal1:
		whole := value / 10
		frac := value % 10
		if frac < 0 {
			frac = -frac
		}
		return fmt.Sprintf("%d.%d", whole, frac)
	default:
		return fmt.Sprintf("%d", value/10)
	}
}

// RankingEntry is a single row in the highest-score ranking.
type RankingEntry struct {
	ProvinceCode   string     `json:"province_code"`
	AdmissionYear  int        `json:"admission_year"`
	BatchCode      string     `json:"batch_code"`
	SchoolCode     string     `json:"school_code"`
	MajorGroupCode string     `json:"major_group_code"`
	ScoreScale     ScoreScale `json:"score_scale"`
	ScoreValue     int64      `json:"score_value"`
	ScoreDisplay   string     `json:"score_display"`
	SubmittedAt    time.Time  `json:"submitted_at"`
	SourceRevision int32      `json:"source_revision"`
}

// HistoryEntry represents one immutable record in the change history of a natural key.
type HistoryEntry struct {
	SubmissionID   string           `json:"submission_id"`
	ScoreScale     ScoreScale       `json:"score_scale"`
	ScoreValue     int64            `json:"score_value"`
	ScoreDisplay   string           `json:"score_display"`
	SubmittedAt    time.Time        `json:"submitted_at"`
	RuleVersion    string           `json:"rule_version"`
	SourceRevision int32            `json:"source_revision"`
	Status         SubmissionStatus `json:"status"`
	CreatedAt      time.Time        `json:"created_at"`
}

// AuditRecord is an append-only audit log entry.
type AuditRecord struct {
	ID             int64
	SubmissionID   string
	Action         AuditAction
	ProvinceCode   string
	AdmissionYear  int
	BatchCode      string
	SchoolCode     string
	MajorGroupCode string
	OldRevision    *int32
	NewRevision    *int32
	OldScore       *int64
	NewScore       *int64
	Reason         string
	TraceID        string
	CreatedAt      time.Time
}

// OutboxEvent is a transactional outbox record for deferred event publishing.
type OutboxEvent struct {
	ID            int64
	EventType     string
	AggregateType string
	AggregateID   string
	Payload       []byte
	CreatedAt     time.Time
	PublishedAt   *time.Time
}

// RankingChangedEvent is the payload for the ranking.changed event.
type RankingChangedEvent struct {
	ProvinceCode   string     `json:"province_code"`
	AdmissionYear  int        `json:"admission_year"`
	BatchCode      string     `json:"batch_code"`
	SchoolCode     string     `json:"school_code"`
	MajorGroupCode string     `json:"major_group_code"`
	ScoreScale     ScoreScale `json:"score_scale"`
	ScoreValue     int64      `json:"score_value"`
	ScoreDisplay   string     `json:"score_display"`
	SourceRevision int32      `json:"source_revision"`
	SubmissionID   string     `json:"submission_id"`
	OccurredAt     time.Time  `json:"occurred_at"`
}

// SubmissionRequest is the inbound payload for POST /v1/submissions.
type SubmissionRequest struct {
	SubmissionID   string     `json:"submission_id"`
	ProvinceCode   string     `json:"province_code"`
	AdmissionYear  int        `json:"admission_year"`
	BatchCode      string     `json:"batch_code"`
	SchoolCode     string     `json:"school_code"`
	MajorGroupCode string     `json:"major_group_code"`
	ScoreScale     ScoreScale `json:"score_scale"`
	ScoreValue     int64      `json:"score_value"`
	SubmittedAt    time.Time  `json:"submitted_at"`
	RuleVersion    string     `json:"rule_version"`
	SourceRevision int32      `json:"source_revision"`
}

// NaturalKey returns the composite natural key from the request.
func (r SubmissionRequest) NaturalKey() NaturalKey {
	return NaturalKey{
		ProvinceCode:   r.ProvinceCode,
		AdmissionYear:  r.AdmissionYear,
		BatchCode:      r.BatchCode,
		SchoolCode:     r.SchoolCode,
		MajorGroupCode: r.MajorGroupCode,
	}
}

// SubmissionResponse is the result of a submission attempt.
type SubmissionResponse struct {
	SubmissionID   string           `json:"submission_id"`
	Status         SubmissionStatus `json:"status"`
	CurrentScore   *int64           `json:"current_score,omitempty"`
	CurrentDisplay string           `json:"current_score_display,omitempty"`
	SourceRevision int32            `json:"source_revision"`
	AcceptedAt     *time.Time       `json:"accepted_at,omitempty"`
	Duplicate      bool             `json:"duplicate,omitempty"`
}

// Validate performs basic field validation on a SubmissionRequest.
func (r SubmissionRequest) Validate() error {
	var errs []string
	if strings.TrimSpace(r.SubmissionID) == "" {
		errs = append(errs, "submission_id is required")
	}
	if strings.TrimSpace(r.ProvinceCode) == "" {
		errs = append(errs, "province_code is required")
	}
	if r.AdmissionYear <= 0 {
		errs = append(errs, "admission_year must be positive")
	}
	if strings.TrimSpace(r.BatchCode) == "" {
		errs = append(errs, "batch_code is required")
	}
	if strings.TrimSpace(r.SchoolCode) == "" {
		errs = append(errs, "school_code is required")
	}
	if strings.TrimSpace(r.MajorGroupCode) == "" {
		errs = append(errs, "major_group_code is required")
	}
	if !r.ScoreScale.Valid() {
		errs = append(errs, "score_scale must be INTEGER or DECIMAL_1")
	}
	if r.ScoreValue < 0 {
		errs = append(errs, "score_value must be non-negative")
	}
	if r.SubmittedAt.IsZero() {
		errs = append(errs, "submitted_at is required")
	}
	if strings.TrimSpace(r.RuleVersion) == "" {
		errs = append(errs, "rule_version is required")
	}
	if r.SourceRevision <= 0 {
		errs = append(errs, "source_revision must be positive")
	}
	if len(errs) > 0 {
		return fmt.Errorf("validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}
