// Package service implements the business logic for the admission score ledger.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gsb/admission-score-ledger/internal/domain"
	"github.com/gsb/admission-score-ledger/internal/repository"
)

// ErrConflict indicates an idempotency conflict (same submission_id, different payload).
var ErrConflict = errors.New("idempotency conflict: submission_id reused with different payload")

// ErrValidation indicates a request failed validation.
var ErrValidation = errors.New("validation error")

// Store defines the persistence operations required by the Service.
// It is implemented by *repository.Repo and can be mocked in tests.
type Store interface {
	BeginTx(ctx context.Context) (repository.Tx, error)
	ListRankings(ctx context.Context, filter repository.RankingFilter) ([]domain.RankingEntry, error)
	ListHistory(ctx context.Context, key domain.NaturalKey) ([]domain.HistoryEntry, error)
	CountOutboxUnpublished(ctx context.Context) (int64, error)
}

// Service contains the core business logic for processing submissions and queries.
type Service struct {
	store Store
	now   func() time.Time
}

// NewService creates a new Service with the given Store.
func NewService(store Store) *Service {
	return &Service{
		store: store,
		now:   repository.Now,
	}
}

// Submit processes a score submission with idempotency and optimistic concurrency control.
// It runs within a single database transaction that also writes audit and outbox records.
func (s *Service) Submit(ctx context.Context, req domain.SubmissionRequest, traceID string) (*domain.SubmissionResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	key := domain.NaturalKey{
		ProvinceCode:   req.ProvinceCode,
		AdmissionYear:  req.AdmissionYear,
		BatchCode:      req.BatchCode,
		SchoolCode:     req.SchoolCode,
		MajorGroupCode: req.MajorGroupCode,
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.AdvisoryLockKey(ctx, key); err != nil {
		return nil, err
	}

	existing, err := tx.GetSubmissionByID(ctx, req.SubmissionID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		if !payloadMatches(existing, req) {
			conflictAudit := &domain.AuditRecord{
				SubmissionID:   req.SubmissionID,
				Action:         domain.AuditActionConflict,
				ProvinceCode:   req.ProvinceCode,
				AdmissionYear:  req.AdmissionYear,
				BatchCode:      req.BatchCode,
				SchoolCode:     req.SchoolCode,
				MajorGroupCode: req.MajorGroupCode,
				NewRevision:    &req.SourceRevision,
				Reason:         "submission_id reused with different payload",
				TraceID:        traceID,
			}
			if err := tx.InsertAudit(ctx, conflictAudit); err != nil {
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("commit conflict audit: %w", err)
			}
			return nil, ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit duplicate: %w", err)
		}
		return s.buildDuplicateResponse(existing), nil
	}

	current, err := tx.GetCurrentSnapshot(ctx, key)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, err
	}

	var status domain.SubmissionStatus
	var oldRev *int32
	var oldScore *int64
	now := s.now()

	if current != nil {
		rev := current.SourceRevision
		score := current.ScoreValue
		oldRev = &rev
		oldScore = &score
	}

	accepted := current == nil || req.SourceRevision > current.SourceRevision
	if accepted {
		status = domain.SubmissionStatusAccepted
	} else {
		status = domain.SubmissionStatusStaleIgnored
	}

	sub := &domain.Submission{
		SubmissionID:   req.SubmissionID,
		ProvinceCode:   req.ProvinceCode,
		AdmissionYear:  req.AdmissionYear,
		BatchCode:      req.BatchCode,
		SchoolCode:     req.SchoolCode,
		MajorGroupCode: req.MajorGroupCode,
		ScoreScale:     req.ScoreScale,
		ScoreValue:     req.ScoreValue,
		SubmittedAt:    req.SubmittedAt,
		RuleVersion:    req.RuleVersion,
		SourceRevision: req.SourceRevision,
		Status:         status,
	}
	if err := tx.InsertSubmission(ctx, sub); err != nil {
		return nil, err
	}

	if accepted {
		snap := &domain.CurrentSnapshot{
			NaturalKey:     key,
			ScoreScale:     req.ScoreScale,
			ScoreValue:     req.ScoreValue,
			SubmittedAt:    req.SubmittedAt,
			RuleVersion:    req.RuleVersion,
			SourceRevision: req.SourceRevision,
			SubmissionID:   req.SubmissionID,
			AcceptedAt:     now,
		}
		if err := tx.UpsertCurrentSnapshot(ctx, snap); err != nil {
			return nil, err
		}
	}

	newRev := req.SourceRevision
	newScore := req.ScoreValue
	auditAction := domain.AuditActionAccepted
	reason := "new revision accepted"
	if !accepted {
		auditAction = domain.AuditActionStaleIgnored
		reason = fmt.Sprintf("source_revision %d <= current %d", req.SourceRevision, current.SourceRevision)
	}

	audit := &domain.AuditRecord{
		SubmissionID:   req.SubmissionID,
		Action:         auditAction,
		ProvinceCode:   req.ProvinceCode,
		AdmissionYear:  req.AdmissionYear,
		BatchCode:      req.BatchCode,
		SchoolCode:     req.SchoolCode,
		MajorGroupCode: req.MajorGroupCode,
		OldRevision:    oldRev,
		NewRevision:    &newRev,
		OldScore:       oldScore,
		NewScore:       &newScore,
		Reason:         reason,
		TraceID:        traceID,
	}
	if err := tx.InsertAudit(ctx, audit); err != nil {
		return nil, err
	}

	if accepted {
		payload, err := json.Marshal(domain.RankingChangedEvent{
			ProvinceCode:   req.ProvinceCode,
			AdmissionYear:  req.AdmissionYear,
			BatchCode:      req.BatchCode,
			SchoolCode:     req.SchoolCode,
			MajorGroupCode: req.MajorGroupCode,
			ScoreScale:     req.ScoreScale,
			ScoreValue:     req.ScoreValue,
			ScoreDisplay:   domain.FormatScore(req.ScoreScale, req.ScoreValue),
			SourceRevision: req.SourceRevision,
			SubmissionID:   req.SubmissionID,
			OccurredAt:     now,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal outbox payload: %w", err)
		}
		outboxEvt := &domain.OutboxEvent{
			EventType:     "ranking.changed",
			AggregateType: "major_group_score",
			AggregateID:   fmt.Sprintf("%s:%d:%s:%s:%s", req.ProvinceCode, req.AdmissionYear, req.BatchCode, req.SchoolCode, req.MajorGroupCode),
			Payload:       payload,
		}
		if err := tx.InsertOutboxEvent(ctx, outboxEvt); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit submit: %w", err)
	}

	resp := &domain.SubmissionResponse{
		SubmissionID:   req.SubmissionID,
		Status:         status,
		SourceRevision: req.SourceRevision,
	}
	if accepted {
		score := req.ScoreValue
		resp.CurrentScore = &score
		resp.CurrentDisplay = domain.FormatScore(req.ScoreScale, req.ScoreValue)
		resp.AcceptedAt = &now
	}
	return resp, nil
}

// ListRankings returns the current highest-score rankings.
func (s *Service) ListRankings(ctx context.Context, filter repository.RankingFilter) ([]domain.RankingEntry, error) {
	return s.store.ListRankings(ctx, filter)
}

// ListHistory returns the full change history for a school major group.
func (s *Service) ListHistory(ctx context.Context, key domain.NaturalKey) ([]domain.HistoryEntry, error) {
	return s.store.ListHistory(ctx, key)
}

// CountOutboxUnpublished returns the count of unpublished outbox events.
func (s *Service) CountOutboxUnpublished(ctx context.Context) (int64, error) {
	return s.store.CountOutboxUnpublished(ctx)
}

// payloadMatches reports whether an existing submission's fields match the incoming request.
func payloadMatches(existing *domain.Submission, req domain.SubmissionRequest) bool {
	return existing.ProvinceCode == req.ProvinceCode &&
		existing.AdmissionYear == req.AdmissionYear &&
		existing.BatchCode == req.BatchCode &&
		existing.SchoolCode == req.SchoolCode &&
		existing.MajorGroupCode == req.MajorGroupCode &&
		existing.ScoreScale == req.ScoreScale &&
		existing.ScoreValue == req.ScoreValue &&
		existing.SubmittedAt.Equal(req.SubmittedAt) &&
		existing.RuleVersion == req.RuleVersion &&
		existing.SourceRevision == req.SourceRevision
}

func (s *Service) buildDuplicateResponse(existing *domain.Submission) *domain.SubmissionResponse {
	resp := &domain.SubmissionResponse{
		SubmissionID:   existing.SubmissionID,
		Status:         existing.Status,
		SourceRevision: existing.SourceRevision,
		Duplicate:      true,
	}
	if existing.Status == domain.SubmissionStatusAccepted {
		score := existing.ScoreValue
		resp.CurrentScore = &score
		resp.CurrentDisplay = domain.FormatScore(existing.ScoreScale, existing.ScoreValue)
		at := existing.CreatedAt
		resp.AcceptedAt = &at
	}
	return resp
}

// OutboxDispatcher polls for unpublished outbox events and marks them published.
// In a production system this would forward to a message broker; here it logs and marks.
type OutboxDispatcher struct {
	repo   *repository.Repo
	logger *log.Logger
}

// NewOutboxDispatcher creates a new OutboxDispatcher.
func NewOutboxDispatcher(repo *repository.Repo, logger *log.Logger) *OutboxDispatcher {
	return &OutboxDispatcher{repo: repo, logger: logger}
}

// Run starts the polling loop. It blocks until the context is cancelled.
func (d *OutboxDispatcher) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.dispatchOnce(ctx); err != nil {
				d.logger.Printf("outbox dispatch error: %v", err)
			}
		}
	}
}

func (d *OutboxDispatcher) dispatchOnce(ctx context.Context) error {
	tx, err := d.repo.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	events, err := tx.ClaimOutboxEvents(ctx, 100)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	for _, e := range events {
		d.logger.Printf("outbox event published: id=%d type=%s aggregate=%s payload=%s",
			e.ID, e.EventType, e.AggregateID, string(e.Payload))
	}
	return tx.Commit()
}
