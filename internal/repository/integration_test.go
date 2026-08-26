package repository_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/gsb/admission-score-ledger/internal/domain"
	"github.com/gsb/admission-score-ledger/internal/repository"
	"github.com/gsb/admission-score-ledger/internal/service"
)

const testDBURL = "postgres://huangding@localhost:5432/ledger_test?sslmode=disable"

func newTestSvc(repo *repository.Repo) *service.Service {
	return service.NewService(repo)
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		dsn = testDBURL
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(10)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("skipping integration test: cannot connect to database: %v", err)
	}
	if err := repository.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	cleanupTables(t, db)
	t.Cleanup(func() {
		cleanupTables(t, db)
		_ = db.Close()
	})
	return db
}

func cleanupTables(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`TRUNCATE TABLE outbox, audit_log, current_snapshots, submissions RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func makeSubmission(subID string, rev int32, score int64, scale domain.ScoreScale, school, major string) domain.SubmissionRequest {
	return domain.SubmissionRequest{
		SubmissionID:   subID,
		ProvinceCode:   "GD",
		AdmissionYear:  2025,
		BatchCode:      "本科批",
		SchoolCode:     school,
		MajorGroupCode: major,
		ScoreScale:     scale,
		ScoreValue:     score,
		SubmittedAt:    time.Now().UTC().Truncate(time.Microsecond),
		RuleVersion:    "v1",
		SourceRevision: rev,
	}
}

func TestIntegrationSubmitAndQueryCurrent(t *testing.T) {
	db := testDB(t)
	repo := repository.NewRepo(db)
	svc := newTestSvc(repo)

	req := makeSubmission("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 1, 6100, domain.ScoreScaleInteger, "szpu", "digital-media")
	resp, err := svc.Submit(context.Background(), req, "trace-1")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if resp.Status != domain.SubmissionStatusAccepted {
		t.Fatalf("expected ACCEPTED, got %s", resp.Status)
	}

	snap, err := repo.GetCurrentSnapshot(context.Background(), req.NaturalKey())
	if err != nil {
		t.Fatalf("get current: %v", err)
	}
	if snap.ScoreValue != 6100 {
		t.Errorf("expected 6100, got %d", snap.ScoreValue)
	}
}

func TestIntegrationStaleRevisionIgnored(t *testing.T) {
	db := testDB(t)
	repo := repository.NewRepo(db)
	svc := newTestSvc(repo)

	req1 := makeSubmission("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 3, 6100, domain.ScoreScaleInteger, "szpu", "digital-media")
	if _, err := svc.Submit(context.Background(), req1, "t1"); err != nil {
		t.Fatal(err)
	}

	req2 := makeSubmission("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", 2, 6098, domain.ScoreScaleInteger, "szpu", "digital-media")
	resp, err := svc.Submit(context.Background(), req2, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != domain.SubmissionStatusStaleIgnored {
		t.Errorf("expected STALE_IGNORED, got %s", resp.Status)
	}

	snap, _ := repo.GetCurrentSnapshot(context.Background(), req1.NaturalKey())
	if snap.SourceRevision != 3 {
		t.Errorf("current should remain rev 3, got %d", snap.SourceRevision)
	}
	if snap.ScoreValue != 6100 {
		t.Errorf("current score should remain 6100, got %d", snap.ScoreValue)
	}
}

func TestIntegrationConcurrentRevisionsHighestWins(t *testing.T) {
	db := testDB(t)
	repo := repository.NewRepo(db)
	svc := newTestSvc(repo)

	key := domain.NaturalKey{
		ProvinceCode: "GD", AdmissionYear: 2025, BatchCode: "本科批",
		SchoolCode: "szpu", MajorGroupCode: "digital-media",
	}

	req1 := makeSubmission("11111111-1111-1111-1111-111111111111", 1, 6100, domain.ScoreScaleDecimal1, "szpu", "digital-media")
	if _, err := svc.Submit(context.Background(), req1, "t1"); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 2)

	go func() {
		req := makeSubmission("22222222-2222-2222-2222-222222222222", 2, 6098, domain.ScoreScaleDecimal1, "szpu", "digital-media")
		_, err := svc.Submit(context.Background(), req, "t2")
		errCh <- err
	}()

	go func() {
		req := makeSubmission("33333333-3333-3333-3333-333333333333", 3, 6102, domain.ScoreScaleDecimal1, "szpu", "digital-media")
		_, err := svc.Submit(context.Background(), req, "t3")
		errCh <- err
	}()

	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent submit error: %v", err)
		}
	}

	snap, err := repo.GetCurrentSnapshot(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ScoreValue != 6102 {
		t.Errorf("expected final score 6102 (rev 3), got %d (rev %d)", snap.ScoreValue, snap.SourceRevision)
	}
	if snap.SourceRevision != 3 {
		t.Errorf("expected final rev 3, got %d", snap.SourceRevision)
	}

	history, err := repo.ListHistory(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("expected 3 history entries, got %d", len(history))
	}
	for _, h := range history {
		t.Logf("history: rev=%d score=%d status=%s display=%s",
			h.SourceRevision, h.ScoreValue, h.Status, h.ScoreDisplay)
	}
}

func TestIntegrationRankingsDisplay(t *testing.T) {
	db := testDB(t)
	repo := repository.NewRepo(db)
	svc := newTestSvc(repo)

	szpu1 := makeSubmission("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 1, 6100, domain.ScoreScaleDecimal1, "szpu", "digital-media")
	if _, err := svc.Submit(context.Background(), szpu1, "t1"); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 2)
	go func() {
		req := makeSubmission("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", 2, 6098, domain.ScoreScaleDecimal1, "szpu", "digital-media")
		_, err := svc.Submit(context.Background(), req, "t2")
		errCh <- err
	}()
	go func() {
		req := makeSubmission("cccccccc-cccc-cccc-cccc-cccccccccccc", 3, 6102, domain.ScoreScaleDecimal1, "szpu", "digital-media")
		_, err := svc.Submit(context.Background(), req, "t3")
		errCh <- err
	}()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}

	whpu := makeSubmission("dddddddd-dddd-dddd-dddd-dddddddddddd", 1, 6044, domain.ScoreScaleDecimal1, "whpu", "digital-media")
	if _, err := svc.Submit(context.Background(), whpu, "t4"); err != nil {
		t.Fatal(err)
	}

	rankings, err := repo.ListRankings(context.Background(), repository.RankingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rankings) != 2 {
		t.Fatalf("expected 2 rankings, got %d", len(rankings))
	}
	if rankings[0].SchoolCode != "szpu" || rankings[0].ScoreDisplay != "610.2" {
		t.Errorf("first should be szpu 610.2, got %s %s", rankings[0].SchoolCode, rankings[0].ScoreDisplay)
	}
	if rankings[1].SchoolCode != "whpu" || rankings[1].ScoreDisplay != "604.4" {
		t.Errorf("second should be whpu 604.4, got %s %s", rankings[1].SchoolCode, rankings[1].ScoreDisplay)
	}
}

func TestIntegration20ConcurrentWrites(t *testing.T) {
	db := testDB(t)
	repo := repository.NewRepo(db)
	svc := newTestSvc(repo)

	const n = 20
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			uuid := fmt.Sprintf("00000000-0000-0000-0000-%012d", idx+1)
			req := makeSubmission(uuid, int32(idx+1), int64(6000+idx), domain.ScoreScaleInteger, "conc", "test")
			_, err := svc.Submit(context.Background(), req, "trace")
			errCh <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent write %d: %v", i, err)
		}
	}

	key := domain.NaturalKey{
		ProvinceCode: "GD", AdmissionYear: 2025, BatchCode: "本科批",
		SchoolCode: "conc", MajorGroupCode: "test",
	}
	snap, err := repo.GetCurrentSnapshot(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SourceRevision != n {
		t.Errorf("expected highest rev %d, got %d", n, snap.SourceRevision)
	}

	history, err := repo.ListHistory(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != n {
		t.Errorf("expected %d history entries, got %d", n, len(history))
	}
}

func TestIntegrationTransactionRollbackNoOutbox(t *testing.T) {
	db := testDB(t)
	repo := repository.NewRepo(db)
	svc := newTestSvc(repo)

	req := makeSubmission("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 1, 6100, domain.ScoreScaleInteger, "szpu", "digital-media")
	if _, err := svc.Submit(context.Background(), req, "t1"); err != nil {
		t.Fatal(err)
	}

	outboxBefore, _ := repo.CountOutboxUnpublished(context.Background())
	if outboxBefore != 1 {
		t.Fatalf("expected 1 outbox event before rollback test, got %d", outboxBefore)
	}

	tx, err := repo.BeginTx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	req2 := makeSubmission("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", 2, 6102, domain.ScoreScaleInteger, "szpu", "digital-media")
	_ = tx.AdvisoryLockKey(context.Background(), req2.NaturalKey())

	sub := &domain.Submission{
		SubmissionID:   req2.SubmissionID,
		ProvinceCode:   req2.ProvinceCode,
		AdmissionYear:  req2.AdmissionYear,
		BatchCode:      req2.BatchCode,
		SchoolCode:     req2.SchoolCode,
		MajorGroupCode: req2.MajorGroupCode,
		ScoreScale:     req2.ScoreScale,
		ScoreValue:     req2.ScoreValue,
		SubmittedAt:    req2.SubmittedAt,
		RuleVersion:    req2.RuleVersion,
		SourceRevision: req2.SourceRevision,
		Status:         domain.SubmissionStatusAccepted,
	}
	if err := tx.InsertSubmission(context.Background(), sub); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.InsertOutboxEvent(context.Background(), &domain.OutboxEvent{
		EventType: "ranking.changed", AggregateType: "test", AggregateID: "test", Payload: []byte("{}"),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	outboxAfter, _ := repo.CountOutboxUnpublished(context.Background())
	if outboxAfter != outboxBefore {
		t.Errorf("outbox count changed after rollback: before=%d after=%d (should be unchanged)", outboxBefore, outboxAfter)
	}

	var subCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM submissions WHERE submission_id = $1", req2.SubmissionID).Scan(&subCount)
	if subCount != 0 {
		t.Errorf("submission should not exist after rollback, found %d", subCount)
	}
}

func TestIntegrationSameScoreSorting(t *testing.T) {
	db := testDB(t)
	repo := repository.NewRepo(db)
	svc := newTestSvc(repo)

	baseTime := time.Date(2025, 8, 25, 12, 0, 0, 0, time.UTC)

	schools := []struct {
		school string
		major  string
		at     time.Time
	}{
		{"school-c", "m2", baseTime.Add(2 * time.Hour)},
		{"school-a", "m1", baseTime},
		{"school-b", "m1", baseTime},
		{"school-a", "m2", baseTime},
	}

	for i, s := range schools {
		req := makeSubmission(
			fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1),
			1, 6100, domain.ScoreScaleInteger, s.school, s.major,
		)
		req.SubmittedAt = s.at
		if _, err := svc.Submit(context.Background(), req, "t"); err != nil {
			t.Fatal(err)
		}
	}

	rankings, err := repo.ListRankings(context.Background(), repository.RankingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rankings) != 4 {
		t.Fatalf("expected 4, got %d", len(rankings))
	}

	expected := []struct{ school, major string }{
		{"school-a", "m1"},
		{"school-a", "m2"},
		{"school-b", "m1"},
		{"school-c", "m2"},
	}
	for i, e := range expected {
		if rankings[i].SchoolCode != e.school || rankings[i].MajorGroupCode != e.major {
			t.Errorf("position %d: expected %s/%s, got %s/%s (submitted_at=%s)",
				i, e.school, e.major, rankings[i].SchoolCode, rankings[i].MajorGroupCode,
				rankings[i].SubmittedAt)
		}
	}
}

func TestIntegrationHistoryImmutability(t *testing.T) {
	db := testDB(t)
	repo := repository.NewRepo(db)
	svc := newTestSvc(repo)

	key := domain.NaturalKey{
		ProvinceCode: "GD", AdmissionYear: 2025, BatchCode: "本科批",
		SchoolCode: "szpu", MajorGroupCode: "digital-media",
	}

	for rev := int32(1); rev <= 5; rev++ {
		uuid := fmt.Sprintf("00000000-0000-0000-0000-%012d", rev)
		score := int64(6100 + int(rev-1)*2)
		req := makeSubmission(uuid, rev, score, domain.ScoreScaleInteger, "szpu", "digital-media")
		if _, err := svc.Submit(context.Background(), req, "t"); err != nil {
			t.Fatal(err)
		}
	}

	staleReq := makeSubmission("ffffffff-ffff-ffff-ffff-ffffffffffff", 3, 9999, domain.ScoreScaleInteger, "szpu", "digital-media")
	if _, err := svc.Submit(context.Background(), staleReq, "t"); err != nil {
		t.Fatal(err)
	}

	history, err := repo.ListHistory(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 6 {
		t.Fatalf("expected 6 history entries (5 accepted + 1 stale), got %d", len(history))
	}

	expectedOrder := []struct {
		rev    int32
		score  int64
		status domain.SubmissionStatus
	}{
		{1, 6100, domain.SubmissionStatusAccepted},
		{2, 6102, domain.SubmissionStatusAccepted},
		{3, 6104, domain.SubmissionStatusAccepted},
		{3, 9999, domain.SubmissionStatusStaleIgnored},
		{4, 6106, domain.SubmissionStatusAccepted},
		{5, 6108, domain.SubmissionStatusAccepted},
	}
	for i, e := range expectedOrder {
		if history[i].SourceRevision != e.rev {
			t.Errorf("history[%d]: expected rev %d, got %d", i, e.rev, history[i].SourceRevision)
		}
		if history[i].ScoreValue != e.score {
			t.Errorf("history[%d]: expected score %d, got %d", i, e.score, history[i].ScoreValue)
		}
		if history[i].Status != e.status {
			t.Errorf("history[%d]: expected %s, got %s", i, e.status, history[i].Status)
		}
	}
	if history[3].ScoreValue != 9999 || history[3].Status != domain.SubmissionStatusStaleIgnored {
		t.Errorf("history[3] should be the stale rev-3 entry (9999, STALE_IGNORED), got %d/%s",
			history[3].ScoreValue, history[3].Status)
	}

	snap, _ := repo.GetCurrentSnapshot(context.Background(), key)
	if snap.ScoreValue != 6108 {
		t.Errorf("current should be 6108 (rev 5), got %d", snap.ScoreValue)
	}
}

func TestIntegrationIdempotencySamePayload(t *testing.T) {
	db := testDB(t)
	repo := repository.NewRepo(db)
	svc := newTestSvc(repo)

	req := makeSubmission("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 1, 6100, domain.ScoreScaleInteger, "szpu", "digital-media")
	resp1, err := svc.Submit(context.Background(), req, "t1")
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := svc.Submit(context.Background(), req, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if !resp2.Duplicate {
		t.Error("second call should be marked duplicate")
	}
	if resp1.Status != resp2.Status {
		t.Errorf("status mismatch: %s vs %s", resp1.Status, resp2.Status)
	}
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM submissions WHERE submission_id = $1", req.SubmissionID).Scan(&count)
	if count != 1 {
		t.Errorf("expected exactly 1 submission row, got %d", count)
	}
}

func TestIntegrationIdempotencyConflict(t *testing.T) {
	db := testDB(t)
	repo := repository.NewRepo(db)
	svc := newTestSvc(repo)

	req := makeSubmission("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 1, 6100, domain.ScoreScaleInteger, "szpu", "digital-media")
	if _, err := svc.Submit(context.Background(), req, "t1"); err != nil {
		t.Fatal(err)
	}

	conflict := req
	conflict.ScoreValue = 9999
	_, err := svc.Submit(context.Background(), conflict, "t2")
	if err == nil {
		t.Fatal("expected conflict error")
	}

	var auditCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'CONFLICT'").Scan(&auditCount)
	if auditCount != 1 {
		t.Errorf("expected 1 CONFLICT audit, got %d", auditCount)
	}
}

func TestIntegrationAuditAndOutboxInTransaction(t *testing.T) {
	db := testDB(t)
	repo := repository.NewRepo(db)
	svc := newTestSvc(repo)

	req := makeSubmission("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 1, 6100, domain.ScoreScaleInteger, "szpu", "digital-media")
	if _, err := svc.Submit(context.Background(), req, "t1"); err != nil {
		t.Fatal(err)
	}

	var auditCount, outboxCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'ACCEPTED'").Scan(&auditCount)
	_ = db.QueryRow("SELECT COUNT(*) FROM outbox WHERE event_type = 'ranking.changed'").Scan(&outboxCount)
	if auditCount != 1 {
		t.Errorf("expected 1 ACCEPTED audit, got %d", auditCount)
	}
	if outboxCount != 1 {
		t.Errorf("expected 1 outbox event, got %d", outboxCount)
	}
}
