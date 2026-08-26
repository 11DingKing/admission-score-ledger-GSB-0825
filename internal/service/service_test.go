package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gsb/admission-score-ledger/internal/domain"
	"github.com/gsb/admission-score-ledger/internal/repository"
)

// mockTx implements repository.Tx for unit testing.
type mockTx struct {
	mu             sync.Mutex
	commitErr      error
	rollbackErr    error
	submissions    map[string]*domain.Submission
	current        map[domain.NaturalKey]*domain.CurrentSnapshot
	audits         []*domain.AuditRecord
	outbox         []*domain.OutboxEvent
	commitCalled   bool
	rollbackCalled bool
	insertSubErr   error
	upsertErr      error
}

func newMockTx() *mockTx {
	return &mockTx{
		submissions: make(map[string]*domain.Submission),
		current:     make(map[domain.NaturalKey]*domain.CurrentSnapshot),
	}
}

func (m *mockTx) Commit() error {
	m.commitCalled = true
	return m.commitErr
}

func (m *mockTx) Rollback() error {
	m.rollbackCalled = true
	return m.rollbackErr
}

func (m *mockTx) AdvisoryLockKey(_ context.Context, _ domain.NaturalKey) error {
	return nil
}

func (m *mockTx) GetSubmissionByID(_ context.Context, id string) (*domain.Submission, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.submissions[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return s, nil
}

func (m *mockTx) GetCurrentSnapshot(_ context.Context, key domain.NaturalKey) (*domain.CurrentSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.current[key]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return s, nil
}

func (m *mockTx) InsertSubmission(_ context.Context, s *domain.Submission) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.insertSubErr != nil {
		return m.insertSubErr
	}
	if _, exists := m.submissions[s.SubmissionID]; exists {
		return repository.ErrDuplicateSubmission
	}
	s.ID = int64(len(m.submissions) + 1)
	s.CreatedAt = time.Now().UTC()
	m.submissions[s.SubmissionID] = s
	return nil
}

func (m *mockTx) UpsertCurrentSnapshot(_ context.Context, snap *domain.CurrentSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.upsertErr != nil {
		return m.upsertErr
	}
	snapCopy := *snap
	m.current[snap.NaturalKey] = &snapCopy
	return nil
}

func (m *mockTx) InsertAudit(_ context.Context, rec *domain.AuditRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec.ID = int64(len(m.audits) + 1)
	rec.CreatedAt = time.Now().UTC()
	m.audits = append(m.audits, rec)
	return nil
}

func (m *mockTx) InsertOutboxEvent(_ context.Context, evt *domain.OutboxEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	evt.ID = int64(len(m.outbox) + 1)
	evt.CreatedAt = time.Now().UTC()
	m.outbox = append(m.outbox, evt)
	return nil
}

func (m *mockTx) ClaimOutboxEvents(_ context.Context, limit int) ([]domain.OutboxEvent, error) {
	return nil, nil
}

// mockStore implements Store for unit testing.
type mockStore struct {
	mu       sync.Mutex
	tx       *mockTx
	txErr    error
	rankings []domain.RankingEntry
	history  []domain.HistoryEntry
}

func (m *mockStore) BeginTx(_ context.Context) (repository.Tx, error) {
	if m.txErr != nil {
		return nil, m.txErr
	}
	return m.tx, nil
}

func (m *mockStore) ListRankings(_ context.Context, _ repository.RankingFilter) ([]domain.RankingEntry, error) {
	return m.rankings, nil
}

func (m *mockStore) ListHistory(_ context.Context, _ domain.NaturalKey) ([]domain.HistoryEntry, error) {
	return m.history, nil
}

func (m *mockStore) CountOutboxUnpublished(_ context.Context) (int64, error) {
	return int64(len(m.tx.outbox)), nil
}

func baseRequest() domain.SubmissionRequest {
	return domain.SubmissionRequest{
		SubmissionID:   "550e8400-e29b-41d4-a716-446655440000",
		ProvinceCode:   "GD",
		AdmissionYear:  2025,
		BatchCode:      "B1",
		SchoolCode:     "szpu",
		MajorGroupCode: "digital-media",
		ScoreScale:     domain.ScoreScaleInteger,
		ScoreValue:     6100,
		SubmittedAt:    time.Date(2025, 8, 25, 10, 0, 0, 0, time.UTC),
		RuleVersion:    "v1",
		SourceRevision: 1,
	}
}

func TestSubmitFirstTimeAccepted(t *testing.T) {
	tx := newMockTx()
	store := &mockStore{tx: tx}
	svc := NewService(store)

	resp, err := svc.Submit(context.Background(), baseRequest(), "trace-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != domain.SubmissionStatusAccepted {
		t.Errorf("expected ACCEPTED, got %s", resp.Status)
	}
	if resp.CurrentScore == nil || *resp.CurrentScore != 6100 {
		t.Errorf("expected current score 6100, got %v", resp.CurrentScore)
	}
	if resp.CurrentDisplay != "610" {
		t.Errorf("expected display '610', got %q", resp.CurrentDisplay)
	}
	if !tx.commitCalled {
		t.Error("expected commit to be called")
	}
	if len(tx.outbox) != 1 {
		t.Errorf("expected 1 outbox event, got %d", len(tx.outbox))
	}
	if tx.outbox[0].EventType != "ranking.changed" {
		t.Errorf("expected ranking.changed event, got %s", tx.outbox[0].EventType)
	}
	if len(tx.audits) != 1 || tx.audits[0].Action != domain.AuditActionAccepted {
		t.Errorf("expected 1 ACCEPTED audit, got %+v", tx.audits)
	}
}

func TestSubmitStaleRevisionIgnored(t *testing.T) {
	tx := newMockTx()
	store := &mockStore{tx: tx}
	svc := NewService(store)

	req1 := baseRequest()
	if _, err := svc.Submit(context.Background(), req1, "trace-1"); err != nil {
		t.Fatalf("first submit: %v", err)
	}

	req2 := baseRequest()
	req2.SubmissionID = "660e8400-e29b-41d4-a716-446655440001"
	req2.SourceRevision = 1
	req2.ScoreValue = 6000
	resp, err := svc.Submit(context.Background(), req2, "trace-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != domain.SubmissionStatusStaleIgnored {
		t.Errorf("expected STALE_IGNORED, got %s", resp.Status)
	}
	if resp.CurrentScore != nil {
		t.Errorf("expected no current score on stale, got %v", resp.CurrentScore)
	}

	snap, _ := tx.GetCurrentSnapshot(context.Background(), req1.NaturalKey())
	if snap.ScoreValue != 6100 {
		t.Errorf("current score should remain 6100, got %d", snap.ScoreValue)
	}
	if len(tx.outbox) != 1 {
		t.Errorf("expected only 1 outbox event (no event for stale), got %d", len(tx.outbox))
	}
	if len(tx.audits) != 2 || tx.audits[1].Action != domain.AuditActionStaleIgnored {
		t.Errorf("expected STALE_IGNORED audit, got %+v", tx.audits)
	}
}

func TestSubmitHigherRevisionAccepted(t *testing.T) {
	tx := newMockTx()
	store := &mockStore{tx: tx}
	svc := NewService(store)

	req1 := baseRequest()
	if _, err := svc.Submit(context.Background(), req1, "trace-1"); err != nil {
		t.Fatal(err)
	}

	req2 := baseRequest()
	req2.SubmissionID = "660e8400-e29b-41d4-a716-446655440001"
	req2.SourceRevision = 2
	req2.ScoreValue = 6102
	resp, err := svc.Submit(context.Background(), req2, "trace-2")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != domain.SubmissionStatusAccepted {
		t.Errorf("expected ACCEPTED, got %s", resp.Status)
	}
	if *resp.CurrentScore != 6102 {
		t.Errorf("expected 6102, got %d", *resp.CurrentScore)
	}
	if len(tx.outbox) != 2 {
		t.Errorf("expected 2 outbox events, got %d", len(tx.outbox))
	}
}

func TestSubmitDuplicateSamePayload(t *testing.T) {
	tx := newMockTx()
	store := &mockStore{tx: tx}
	svc := NewService(store)

	req := baseRequest()
	if _, err := svc.Submit(context.Background(), req, "trace-1"); err != nil {
		t.Fatal(err)
	}

	resp, err := svc.Submit(context.Background(), req, "trace-2")
	if err != nil {
		t.Fatalf("duplicate same payload should not error: %v", err)
	}
	if !resp.Duplicate {
		t.Error("expected duplicate=true")
	}
	if resp.Status != domain.SubmissionStatusAccepted {
		t.Errorf("expected ACCEPTED, got %s", resp.Status)
	}
	if len(tx.submissions) != 1 {
		t.Errorf("expected only 1 submission record, got %d", len(tx.submissions))
	}
}

func TestSubmitDuplicateDifferentPayloadConflict(t *testing.T) {
	tx := newMockTx()
	store := &mockStore{tx: tx}
	svc := NewService(store)

	req := baseRequest()
	if _, err := svc.Submit(context.Background(), req, "trace-1"); err != nil {
		t.Fatal(err)
	}

	conflictReq := req
	conflictReq.ScoreValue = 9999
	_, err := svc.Submit(context.Background(), conflictReq, "trace-2")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}

	hasConflictAudit := false
	for _, a := range tx.audits {
		if a.Action == domain.AuditActionConflict {
			hasConflictAudit = true
		}
	}
	if !hasConflictAudit {
		t.Error("expected a CONFLICT audit record")
	}
}

func TestSubmitValidationError(t *testing.T) {
	tx := newMockTx()
	store := &mockStore{tx: tx}
	svc := NewService(store)

	req := baseRequest()
	req.SubmissionID = ""
	_, err := svc.Submit(context.Background(), req, "trace-1")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	if tx.commitCalled {
		t.Error("commit should not be called on validation error")
	}
}

func TestSubmitDecimal1Formatting(t *testing.T) {
	tx := newMockTx()
	store := &mockStore{tx: tx}
	svc := NewService(store)

	req := baseRequest()
	req.ScoreScale = domain.ScoreScaleDecimal1
	req.ScoreValue = 6044
	resp, err := svc.Submit(context.Background(), req, "trace-1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.CurrentDisplay != "604.4" {
		t.Errorf("expected '604.4', got %q", resp.CurrentDisplay)
	}
}

func TestListRankingsAndHistory(t *testing.T) {
	tx := newMockTx()
	store := &mockStore{
		tx: tx,
		rankings: []domain.RankingEntry{
			{SchoolCode: "b", ScoreValue: 6000, ScoreDisplay: "600"},
			{SchoolCode: "a", ScoreValue: 6000, ScoreDisplay: "600"},
		},
		history: []domain.HistoryEntry{
			{SubmissionID: "uuid-1", SourceRevision: 1, Status: domain.SubmissionStatusAccepted},
			{SubmissionID: "uuid-2", SourceRevision: 2, Status: domain.SubmissionStatusStaleIgnored},
		},
	}
	svc := NewService(store)

	rankings, err := svc.ListRankings(context.Background(), repository.RankingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rankings) != 2 {
		t.Errorf("expected 2 rankings, got %d", len(rankings))
	}

	history, err := svc.ListHistory(context.Background(), baseRequest().NaturalKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Errorf("expected 2 history entries, got %d", len(history))
	}
}
