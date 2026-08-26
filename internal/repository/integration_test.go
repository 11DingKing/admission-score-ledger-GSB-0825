package repository_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"admission-score-ledger/internal/domain"
	"admission-score-ledger/internal/repository"
	"admission-score-ledger/internal/testdb"
)

const (
	testProvince = "44"
	testYear     = 2025
	testBatch    = "B1"
)

func testSubmission(id, school, group string, revision, value int64, scale domain.ScoreScale, at time.Time) domain.Submission {
	return domain.Submission{
		SubmissionID:   id,
		ProvinceCode:   testProvince,
		AdmissionYear:  testYear,
		BatchCode:      testBatch,
		SchoolCode:     school,
		MajorGroupCode: group,
		ScoreScale:     scale,
		ScoreValue:     value,
		SubmittedAt:    at,
		RuleVersion:    "v1",
		SourceRevision: revision,
	}
}

func input(sub domain.Submission, traceID string) repository.SubmitInput {
	return repository.SubmitInput{
		Submission:  sub,
		PayloadHash: domain.PayloadHash(sub),
		TraceID:     traceID,
	}
}

func newRepo(t *testing.T) (*repository.PostgresRepository, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	return repository.NewPostgresRepository(pool), pool
}

func key(school, group string) domain.NaturalKey {
	return domain.NaturalKey{
		ProvinceCode:   testProvince,
		AdmissionYear:  testYear,
		BatchCode:      testBatch,
		SchoolCode:     school,
		MajorGroupCode: group,
	}
}

func TestSubmitAcceptReplayConflictStale(t *testing.T) {
	repo, pool := newRepo(t)
	ctx := context.Background()
	at := time.Date(2025, 8, 26, 9, 0, 0, 0, time.UTC)

	sub := testSubmission("sub-1", "szpu", "digital-media", 1, 6100, domain.ScaleDecimal1, at)
	res, err := repo.Submit(ctx, input(sub, "trace-1"))
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if res.Decision != domain.DecisionAccepted || res.Replayed {
		t.Fatalf("expected ACCEPTED new submission, got %s replayed=%v", res.Decision, res.Replayed)
	}
	if res.Current.ScoreDisplay != "610.0" || res.Current.SourceRevision != 1 {
		t.Fatalf("unexpected current: %+v", res.Current)
	}

	replay, err := repo.Submit(ctx, input(sub, "trace-1-replay"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed || replay.Decision != domain.DecisionAccepted {
		t.Fatalf("replay must return first result, got replayed=%v decision=%s", replay.Replayed, replay.Decision)
	}

	conflicting := sub
	conflicting.ScoreValue = 6102
	_, err = repo.Submit(ctx, input(conflicting, "trace-1-conflict"))
	if !errors.Is(err, repository.ErrSubmissionConflict) {
		t.Fatalf("expected ErrSubmissionConflict, got %v", err)
	}
	if n := testdb.CountRows(t, pool, "audit_log", "decision = 'CONFLICT'"); n != 1 {
		t.Fatalf("conflict must write exactly one audit row, got %d", n)
	}

	stale := testSubmission("sub-2", "szpu", "digital-media", 1, 6050, domain.ScaleDecimal1, at.Add(time.Minute))
	staleRes, err := repo.Submit(ctx, input(stale, "trace-2"))
	if err != nil {
		t.Fatalf("stale submit: %v", err)
	}
	if staleRes.Decision != domain.DecisionStaleIgnored {
		t.Fatalf("expected STALE_IGNORED, got %s", staleRes.Decision)
	}
	if staleRes.Current.ScoreValue != 6100 {
		t.Fatalf("stale write must not change current, got %d", staleRes.Current.ScoreValue)
	}

	higher := testSubmission("sub-3", "szpu", "digital-media", 2, 6102, domain.ScaleDecimal1, at.Add(2*time.Minute))
	higherRes, err := repo.Submit(ctx, input(higher, "trace-3"))
	if err != nil {
		t.Fatalf("higher revision: %v", err)
	}
	if higherRes.Decision != domain.DecisionAccepted || higherRes.Current.ScoreValue != 6102 {
		t.Fatalf("higher revision must become current, got decision=%s value=%d",
			higherRes.Decision, higherRes.Current.ScoreValue)
	}

	records, err := repo.ListHistory(ctx, key("szpu", "digital-media"))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("history must retain accepted + stale records, got %d", len(records))
	}
	statusCount := map[domain.Decision]int{}
	for _, r := range records {
		statusCount[r.Status]++
	}
	if statusCount[domain.DecisionAccepted] != 2 || statusCount[domain.DecisionStaleIgnored] != 1 {
		t.Fatalf("history statuses wrong: %v", statusCount)
	}

	if n := testdb.CountRows(t, pool, "outbox_events", ""); n != 2 {
		t.Fatalf("only accepted writes emit outbox events, got %d", n)
	}
	if n := testdb.CountRows(t, pool, "outbox_events", "event_type = 'ranking.changed'"); n != 2 {
		t.Fatalf("all events must be ranking.changed, got %d", n)
	}
}

func TestConcurrentRevisionArbitration(t *testing.T) {
	repo, pool := newRepo(t)
	ctx := context.Background()
	base := time.Date(2025, 8, 26, 9, 0, 0, 0, time.UTC)

	first := testSubmission("szpu-rev1", "szpu", "digital-media", 1, 6100, domain.ScaleDecimal1, base)
	if _, err := repo.Submit(ctx, input(first, "rev1")); err != nil {
		t.Fatalf("rev1: %v", err)
	}

	type outcome struct {
		revision int64
		decision domain.Decision
		err      error
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	outcomes := make([]outcome, 2)
	subs := []domain.Submission{
		testSubmission("szpu-rev2", "szpu", "digital-media", 2, 6098, domain.ScaleDecimal1, base.Add(time.Minute)),
		testSubmission("szpu-rev3", "szpu", "digital-media", 3, 6102, domain.ScaleDecimal1, base.Add(2*time.Minute)),
	}
	for i, sub := range subs {
		wg.Add(1)
		go func(i int, sub domain.Submission) {
			defer wg.Done()
			<-start
			res, err := repo.Submit(ctx, input(sub, fmt.Sprintf("rev%d", sub.SourceRevision)))
			oc := outcome{revision: sub.SourceRevision, err: err}
			if err == nil {
				oc.decision = res.Decision
			}
			outcomes[i] = oc
		}(i, sub)
	}
	close(start)
	wg.Wait()

	for _, oc := range outcomes {
		if oc.err != nil {
			t.Fatalf("concurrent submit rev%d: %v", oc.revision, oc.err)
		}
	}

	current, err := loadCurrentSnapshot(pool, "szpu", "digital-media")
	if err != nil {
		t.Fatalf("load current: %v", err)
	}
	if current.sourceRevision != 3 || current.scoreValue != 6102 {
		t.Fatalf("current must be rev3 6102, got rev%d value %d", current.sourceRevision, current.scoreValue)
	}

	records, err := repo.ListHistory(ctx, key("szpu", "digital-media"))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("all three submissions must be explainable in history, got %d", len(records))
	}
	byRevision := map[int64]domain.HistoryRecord{}
	for _, r := range records {
		byRevision[r.SourceRevision] = r
	}
	if byRevision[1].Status != domain.DecisionAccepted {
		t.Fatalf("rev1 must be ACCEPTED")
	}
	if byRevision[3].Status != domain.DecisionAccepted {
		t.Fatalf("rev3 must be ACCEPTED")
	}
	if byRevision[2].Status != domain.DecisionAccepted && byRevision[2].Status != domain.DecisionStaleIgnored {
		t.Fatalf("rev2 must be ACCEPTED or STALE_IGNORED, got %s", byRevision[2].Status)
	}

	accepted := 0
	for _, r := range records {
		if r.Status == domain.DecisionAccepted {
			accepted++
		}
	}
	if n := testdb.CountRows(t, pool, "outbox_events", ""); n != accepted {
		t.Fatalf("outbox events (%d) must equal accepted count (%d)", n, accepted)
	}
	if n := testdb.CountRows(t, pool, "audit_log",
		"school_code = 'szpu' AND major_group_code = 'digital-media'"); n != 3 {
		t.Fatalf("every submission must be audited, got %d rows", n)
	}

	rankings, err := repo.ListRankings(ctx, repository.RankingFilter{
		ProvinceCode: testProvince, AdmissionYear: testYear, BatchCode: testBatch,
	})
	if err != nil {
		t.Fatalf("rankings: %v", err)
	}
	if len(rankings) != 1 || rankings[0].ScoreDisplay != "610.2" {
		t.Fatalf("ranking must show 610.2, got %+v", rankings)
	}
}

func TestRollbackLeavesNoRowsAndNoOutbox(t *testing.T) {
	_, pool := newRepo(t)
	ctx := context.Background()
	at := time.Date(2025, 8, 26, 9, 0, 0, 0, time.UTC)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	sub := testSubmission("rollback-1", "ro-school", "ro-group", 1, 6100, domain.ScaleDecimal1, at)
	if _, err := repository.ApplyInTx(ctx, tx, input(sub, "rollback-trace")); err != nil {
		t.Fatalf("apply in tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	assertEmptyForKey(t, pool, "ro-school", "ro-group")

	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	bad := testSubmission("rollback-2", "ro-school", "ro-group", 1, -5, domain.ScaleDecimal1, at)
	if _, err := repository.ApplyInTx(ctx, tx2, input(bad, "rollback-fail")); err == nil {
		t.Fatalf("negative score must fail inside the transaction")
	}
	_ = tx2.Rollback(ctx)

	assertEmptyForKey(t, pool, "ro-school", "ro-group")
}

func assertEmptyForKey(t *testing.T, pool *pgxpool.Pool, school, group string) {
	t.Helper()
	naturalPredicate := "province_code = '44' AND admission_year = 2025 AND batch_code = 'B1' AND school_code = $1 AND major_group_code = $2"
	for _, table := range []string{"current_snapshots", "submission_records", "audit_log"} {
		var n int
		err := pool.QueryRow(context.Background(),
			"SELECT count(*) FROM "+table+" WHERE "+naturalPredicate, school, group).Scan(&n)
		if err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("table %s must be empty after rollback, got %d", table, n)
		}
	}
	aggregateKey := fmt.Sprintf("44|2025|B1|%s|%s", school, group)
	var outboxCount int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM outbox_events WHERE aggregate_key = $1", aggregateKey).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox_events: %v", err)
	}
	if outboxCount != 0 {
		t.Fatalf("outbox_events must be empty after rollback, got %d", outboxCount)
	}
}

func TestAppendOnlyTablesRejectMutation(t *testing.T) {
	repo, pool := newRepo(t)
	ctx := context.Background()
	at := time.Date(2025, 8, 26, 9, 0, 0, 0, time.UTC)
	sub := testSubmission("immutable-1", "im-school", "im-group", 1, 6100, domain.ScaleDecimal1, at)
	if _, err := repo.Submit(ctx, input(sub, "immutable")); err != nil {
		t.Fatalf("seed submit: %v", err)
	}

	mutations := []string{
		`UPDATE submission_records SET score_value = 9999 WHERE submission_id = 'immutable-1'`,
		`DELETE FROM submission_records WHERE submission_id = 'immutable-1'`,
		`UPDATE audit_log SET decision = 'CONFLICT' WHERE trace_id = 'immutable'`,
		`DELETE FROM audit_log WHERE trace_id = 'immutable'`,
	}
	for _, m := range mutations {
		if _, err := pool.Exec(ctx, m); err == nil {
			t.Fatalf("append-only guard must reject: %s", m)
		}
	}

	records, err := repo.ListHistory(ctx, key("im-school", "im-group"))
	if err != nil || len(records) != 1 || records[0].ScoreValue != 6100 {
		t.Fatalf("history must be unchanged, got %+v err=%v", records, err)
	}
}

func TestRankingFiltersAndIntegerScale(t *testing.T) {
	repo, _ := newRepo(t)
	ctx := context.Background()
	at := time.Date(2025, 8, 26, 9, 0, 0, 0, time.UTC)

	whpu := testSubmission("whpu-1", "whpu", "digital-media", 1, 6044, domain.ScaleDecimal1, at)
	if _, err := repo.Submit(ctx, input(whpu, "whpu")); err != nil {
		t.Fatalf("whpu: %v", err)
	}
	intSchool := testSubmission("int-1", "intsch", "arts", 1, 6100, domain.ScaleInteger, at)
	if _, err := repo.Submit(ctx, input(intSchool, "int")); err != nil {
		t.Fatalf("integer school: %v", err)
	}

	rows, err := repo.ListRankings(ctx, repository.RankingFilter{
		ProvinceCode: testProvince, AdmissionYear: testYear, BatchCode: testBatch,
	})
	if err != nil {
		t.Fatalf("rankings: %v", err)
	}
	bySchool := map[string]domain.RankingRow{}
	for _, r := range rows {
		bySchool[r.SchoolCode] = r
	}
	if bySchool["whpu"].ScoreDisplay != "604.4" {
		t.Fatalf("whpu must display 604.4, got %q", bySchool["whpu"].ScoreDisplay)
	}
	if bySchool["intsch"].ScoreDisplay != "610" {
		t.Fatalf("INTEGER scale must display 610, got %q", bySchool["intsch"].ScoreDisplay)
	}
	if rows[0].SchoolCode != "intsch" || rows[1].SchoolCode != "whpu" {
		t.Fatalf("rank order wrong: %s before %s", rows[0].SchoolCode, rows[1].SchoolCode)
	}

	otherYear, err := repo.ListRankings(ctx, repository.RankingFilter{
		ProvinceCode: testProvince, AdmissionYear: 2030,
	})
	if err != nil {
		t.Fatalf("rankings other year: %v", err)
	}
	if len(otherYear) != 0 {
		t.Fatalf("year filter must isolate data, got %d rows", len(otherYear))
	}
}

type snapshotRow struct {
	scoreValue     int64
	sourceRevision int64
}

func loadCurrentSnapshot(pool *pgxpool.Pool, school, group string) (snapshotRow, error) {
	var row snapshotRow
	err := pool.QueryRow(context.Background(), `
		SELECT score_value, source_revision FROM current_snapshots
		WHERE province_code = $1 AND admission_year = $2 AND batch_code = $3
		  AND school_code = $4 AND major_group_code = $5`,
		testProvince, testYear, testBatch, school, group).Scan(&row.scoreValue, &row.sourceRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshotRow{}, fmt.Errorf("snapshot not found")
	}
	return row, err
}
