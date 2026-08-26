package httpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"admission-score-ledger/internal/httpserver"
	"admission-score-ledger/internal/repository"
	"admission-score-ledger/internal/service"
	"admission-score-ledger/internal/testdb"
)

const (
	province = "44"
	year     = 2025
	batch    = "B1"
)

type submitAPIBody struct {
	SubmissionID   string `json:"submission_id"`
	ProvinceCode   string `json:"province_code"`
	AdmissionYear  int    `json:"admission_year"`
	BatchCode      string `json:"batch_code"`
	SchoolCode     string `json:"school_code"`
	MajorGroupCode string `json:"major_group_code"`
	ScoreScale     string `json:"score_scale"`
	ScoreValue     int64  `json:"score_value"`
	SubmittedAt    string `json:"submitted_at"`
	RuleVersion    string `json:"rule_version"`
	SourceRevision int64  `json:"source_revision"`
}

type submitAPIResponse struct {
	Decision     string `json:"decision"`
	Replayed     bool   `json:"replayed"`
	SubmissionID string `json:"submission_id"`
	Current      *struct {
		ScoreValue     int64  `json:"score_value"`
		ScoreDisplay   string `json:"score_display"`
		SourceRevision int64  `json:"source_revision"`
	} `json:"current"`
	Received *struct {
		SourceRevision int64  `json:"source_revision"`
		ScoreDisplay   string `json:"score_display"`
		Status         string `json:"status"`
	} `json:"received"`
}

type rankingRow struct {
	Rank           int    `json:"rank"`
	SchoolCode     string `json:"school_code"`
	MajorGroupCode string `json:"major_group_code"`
	ScoreScale     string `json:"score_scale"`
	ScoreValue     int64  `json:"score_value"`
	ScoreDisplay   string `json:"score_display"`
	SourceRevision int64  `json:"source_revision"`
}

type rankingsAPIResponse struct {
	Rankings []rankingRow `json:"rankings"`
}

type historyRecord struct {
	SubmissionID   string `json:"submission_id"`
	SourceRevision int64  `json:"source_revision"`
	ScoreValue     int64  `json:"score_value"`
	ScoreDisplay   string `json:"score_display"`
	Status         string `json:"status"`
}

type historyAPIResponse struct {
	Records []historyRecord `json:"records"`
}

type problemAPIResponse struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Status  int    `json:"status"`
	Code    string `json:"code"`
	TraceID string `json:"trace_id"`
}

func newTestServer(t *testing.T) (*httptest.Server, *repository.PostgresRepository) {
	t.Helper()
	pool := testdb.New(t)
	repo := repository.NewPostgresRepository(pool)
	ledger := service.NewLedger(repo)
	srv := httptest.NewServer(httpserver.New(ledger))
	t.Cleanup(srv.Close)
	return srv, repo
}

func submissionBody(id, school, group, scale string, value, revision int64, at time.Time) submitAPIBody {
	return submitAPIBody{
		SubmissionID:   id,
		ProvinceCode:   province,
		AdmissionYear:  year,
		BatchCode:      batch,
		SchoolCode:     school,
		MajorGroupCode: group,
		ScoreScale:     scale,
		ScoreValue:     value,
		SubmittedAt:    at.UTC().Format(time.RFC3339),
		RuleVersion:    "rv-2025",
		SourceRevision: revision,
	}
}

func doJSON(t *testing.T, method, url string, body any) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, resp.Header, data
}

func postSubmission(t *testing.T, baseURL string, body submitAPIBody) (int, submitAPIResponse) {
	t.Helper()
	status, _, data := doJSON(t, http.MethodPost, baseURL+"/v1/submissions", body)
	var resp submitAPIResponse
	if status == http.StatusCreated || status == http.StatusAccepted {
		if err := json.Unmarshal(data, &resp); err != nil {
			t.Fatalf("decode submit response %d: %v\nbody: %s", status, err, data)
		}
	}
	return status, resp
}

func getRankings(t *testing.T, baseURL string) []rankingRow {
	t.Helper()
	url := fmt.Sprintf("%s/v1/rankings?province_code=%s&admission_year=%d&batch_code=%s",
		baseURL, province, year, batch)
	status, _, data := doJSON(t, http.MethodGet, url, nil)
	if status != http.StatusOK {
		t.Fatalf("rankings status = %d, body: %s", status, data)
	}
	var resp rankingsAPIResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decode rankings: %v", err)
	}
	return resp.Rankings
}

func getHistory(t *testing.T, baseURL, school, group string) (int, []historyRecord) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/history/%s/%s?province_code=%s&admission_year=%d&batch_code=%s",
		baseURL, school, group, province, year, batch)
	status, _, data := doJSON(t, http.MethodGet, url, nil)
	if status == http.StatusOK {
		var resp historyAPIResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			t.Fatalf("decode history: %v", err)
		}
		return status, resp.Records
	}
	return status, nil
}

func currentScoreValue(t *testing.T, repo *repository.PostgresRepository, school, group string) int64 {
	t.Helper()
	var value int64
	err := repo.Pool().QueryRow(context.Background(), `
		SELECT score_value FROM current_snapshots
		WHERE province_code = $1 AND admission_year = $2 AND batch_code = $3
		  AND school_code = $4 AND major_group_code = $5`,
		province, year, batch, school, group).Scan(&value)
	if err != nil {
		t.Fatalf("load current snapshot %s/%s: %v", school, group, err)
	}
	return value
}

// TestEndToEndScenario replays the business scenario: szpu revision 1 first,
// then concurrent revision 2 (6098) and revision 3 (6102), then whpu 6044.
// The current szpu value must be 6102 (display 610.2) and the ranking must
// show 610.2 ahead of 604.4. All three szpu changes remain explainable in
// history, and idempotent retries / conflicts behave as specified.
func TestEndToEndScenario(t *testing.T) {
	srv, repo := newTestServer(t)
	base := srv.URL

	t1 := time.Date(2025, 8, 25, 9, 0, 0, 0, time.UTC)

	// 1. First write: szpu/digital-media 6100, revision 1.
	st, resp := postSubmission(t, base, submissionBody("szpu-sub-1", "szpu", "digital-media", "DECIMAL_1", 6100, 1, t1))
	if st != http.StatusCreated {
		t.Fatalf("first submission status = %d, want 201", st)
	}
	if resp.Decision != "ACCEPTED" || resp.Replayed {
		t.Fatalf("first submission decision = %s replayed = %v, want ACCEPTED/false", resp.Decision, resp.Replayed)
	}
	if got := currentScoreValue(t, repo, "szpu", "digital-media"); got != 6100 {
		t.Fatalf("current score after rev1 = %d, want 6100", got)
	}

	// 2. Concurrent revision 2 (6098) and revision 3 (6102) on the same natural key.
	t2 := t1.Add(time.Minute)
	t3 := t1.Add(2 * time.Minute)
	type concurrentResult struct {
		id     string
		status int
		body   submitAPIResponse
	}
	results := make([]concurrentResult, 2)
	var wg sync.WaitGroup
	for i, c := range []struct {
		id    string
		value int64
		rev   int64
		at    time.Time
	}{
		{"szpu-sub-2", 6098, 2, t2},
		{"szpu-sub-3", 6102, 3, t3},
	} {
		wg.Add(1)
		go func(i int, c struct {
			id    string
			value int64
			rev   int64
			at    time.Time
		}) {
			defer wg.Done()
			st, body := postSubmission(t, base, submissionBody(c.id, "szpu", "digital-media", "DECIMAL_1", c.value, c.rev, c.at))
			results[i] = concurrentResult{id: c.id, status: st, body: body}
		}(i, c)
	}
	wg.Wait()

	var accepted, stale int
	for _, r := range results {
		switch r.status {
		case http.StatusCreated:
			accepted++
			if r.body.Decision != "ACCEPTED" {
				t.Fatalf("%s: 201 but decision = %s", r.id, r.body.Decision)
			}
		case http.StatusAccepted:
			stale++
			if r.body.Decision != "STALE_IGNORED" {
				t.Fatalf("%s: 202 but decision = %s", r.id, r.body.Decision)
			}
		default:
			t.Fatalf("%s: unexpected status %d", r.id, r.status)
		}
	}
	if accepted < 1 || accepted+stale != 2 {
		t.Fatalf("concurrent rev2/rev3: accepted = %d stale = %d, want 1-2 accepted and the rest stale", accepted, stale)
	}

	// The highest revision must always win regardless of acquisition order.
	if got := currentScoreValue(t, repo, "szpu", "digital-media"); got != 6102 {
		t.Fatalf("current score after concurrent writes = %d, want 6102", got)
	}

	// 3. History must explain all three changes.
	_, records := getHistory(t, base, "szpu", "digital-media")
	if len(records) != 3 {
		t.Fatalf("history length = %d, want 3", len(records))
	}
	byRev := map[int64]historyRecord{}
	for _, r := range records {
		byRev[r.SourceRevision] = r
	}
	if byRev[1].Status != "ACCEPTED" || byRev[1].ScoreValue != 6100 {
		t.Fatalf("rev1 history = %+v, want ACCEPTED 6100", byRev[1])
	}
	if byRev[3].Status != "ACCEPTED" || byRev[3].ScoreValue != 6102 {
		t.Fatalf("rev3 history = %+v, want ACCEPTED 6102", byRev[3])
	}
	switch byRev[2].Status {
	case "ACCEPTED", "STALE_IGNORED":
		// rev2 is ACCEPTED if it won the lock before rev3, STALE_IGNORED otherwise:
		// both are explainable, but it must always be retained in history.
	default:
		t.Fatalf("rev2 history status = %q, want ACCEPTED or STALE_IGNORED", byRev[2].Status)
	}
	if byRev[2].ScoreValue != 6098 {
		t.Fatalf("rev2 history score = %d, want 6098", byRev[2].ScoreValue)
	}

	// 4. Outbox events exist exactly for the accepted changes.
	outboxN := testdb.CountRows(t, repo.Pool(), "outbox_events",
		"aggregate_key = $1", "44|2025|B1|szpu|digital-media")
	wantOutbox := 0
	for _, r := range records {
		if r.Status == "ACCEPTED" {
			wantOutbox++
		}
	}
	if outboxN != wantOutbox {
		t.Fatalf("outbox events for szpu = %d, want %d (one per ACCEPTED record)", outboxN, wantOutbox)
	}

	// 5. Idempotent replay: identical payload returns the first decision and marks replayed.
	rev2Status := http.StatusCreated
	if byRev[2].Status == "STALE_IGNORED" {
		rev2Status = http.StatusAccepted
	}
	st, replay := postSubmission(t, base, submissionBody("szpu-sub-2", "szpu", "digital-media", "DECIMAL_1", 6098, 2, t2))
	if st != rev2Status {
		t.Fatalf("replay rev2 status = %d, want %d (the first result)", st, rev2Status)
	}
	if !replay.Replayed {
		t.Fatal("identical replay must set replayed = true")
	}
	if _, replayRecords := getHistory(t, base, "szpu", "digital-media"); len(replayRecords) != 3 {
		t.Fatalf("history length after replay = %d, want 3", len(replayRecords))
	}
	if got := currentScoreValue(t, repo, "szpu", "digital-media"); got != 6102 {
		t.Fatalf("current score changed after replay = %d, want 6102", got)
	}

	// 6. Same submission_id with a different payload is a 409 problem+json conflict.
	st, headers, data := doJSON(t, http.MethodPost, base+"/v1/submissions",
		submissionBody("szpu-sub-1", "szpu", "digital-media", "DECIMAL_1", 9999, 9, t1))
	if st != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409", st)
	}
	if ct := headers.Get("Content-Type"); ct != httpserver.ProblemContentType {
		t.Fatalf("conflict Content-Type = %q, want %q", ct, httpserver.ProblemContentType)
	}
	var problem problemAPIResponse
	if err := json.Unmarshal(data, &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "SUBMISSION_CONFLICT" || problem.Status != 409 || problem.TraceID == "" || problem.Type == "" || problem.Title == "" {
		t.Fatalf("problem body missing required fields: %+v", problem)
	}
	// The conflict is audit-logged but writes no snapshot, record or outbox row.
	auditN := testdb.CountRows(t, repo.Pool(), "audit_log",
		"school_code = $1 AND decision = 'CONFLICT'", "szpu")
	if auditN != 1 {
		t.Fatalf("conflict audit rows = %d, want 1", auditN)
	}
	if _, recordsAfter := getHistory(t, base, "szpu", "digital-media"); len(recordsAfter) != 3 {
		t.Fatalf("history length after conflict = %d, want 3", len(recordsAfter))
	}

	// 7. whpu/digital-media 6044 (604.4 points on DECIMAL_1).
	st, _ = postSubmission(t, base, submissionBody("whpu-sub-1", "whpu", "digital-media", "DECIMAL_1", 6044, 1, t1))
	if st != http.StatusCreated {
		t.Fatalf("whpu submission status = %d, want 201", st)
	}

	// 8. Ranking shows 610.2 then 604.4, formatted without floats.
	rows := getRankings(t, base)
	if len(rows) != 2 {
		t.Fatalf("ranking length = %d, want 2", len(rows))
	}
	if rows[0].SchoolCode != "szpu" || rows[0].ScoreDisplay != "610.2" || rows[0].Rank != 1 {
		t.Fatalf("rank 1 = %+v, want szpu 610.2", rows[0])
	}
	if rows[1].SchoolCode != "whpu" || rows[1].ScoreDisplay != "604.4" || rows[1].Rank != 2 {
		t.Fatalf("rank 2 = %+v, want whpu 604.4", rows[1])
	}
}

// TestTwentyConcurrentWrites fires 20 concurrent submissions for 20 distinct
// keys (all must be accepted) plus a 20-revision concurrent burst on one key
// (only the highest revision may be current). Outbox events must match the
// number of ACCEPTED records exactly.
func TestTwentyConcurrentWrites(t *testing.T) {
	srv, repo := newTestServer(t)
	base := srv.URL
	baseTime := time.Date(2025, 8, 25, 9, 0, 0, 0, time.UTC)

	statuses := make([]int, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			school := fmt.Sprintf("school-%02d", i)
			body := submissionBody(fmt.Sprintf("sub-%02d", i), school, "digital-media",
				"DECIMAL_1", int64(5000+i*10), 1, baseTime.Add(time.Duration(i)*time.Second))
			st, _ := postSubmission(t, base, body)
			statuses[i] = st
		}(i)
	}
	wg.Wait()
	for i, st := range statuses {
		if st != http.StatusCreated {
			t.Fatalf("concurrent write %d status = %d, want 201", i, st)
		}
	}
	if rows := getRankings(t, base); len(rows) != 20 {
		t.Fatalf("ranking length = %d, want 20", len(rows))
	}

	// 20 revisions of one natural key submitted concurrently.
	burstStatuses := make([]int, 20)
	var wg2 sync.WaitGroup
	for rev := int64(1); rev <= 20; rev++ {
		wg2.Add(1)
		go func(rev int64) {
			defer wg2.Done()
			body := submissionBody(fmt.Sprintf("burst-%02d", rev), "burst", "digital-media",
				"DECIMAL_1", 5000+rev*10, rev, baseTime.Add(time.Duration(rev)*time.Second))
			st, _ := postSubmission(t, base, body)
			burstStatuses[rev-1] = st
		}(rev)
	}
	wg2.Wait()
	for rev, st := range burstStatuses {
		if st != http.StatusCreated && st != http.StatusAccepted {
			t.Fatalf("burst rev %d status = %d, want 201 or 202", rev+1, st)
		}
	}

	if got := currentScoreValue(t, repo, "burst", "digital-media"); got != 5200 {
		t.Fatalf("burst current score = %d, want 5200 (revision 20)", got)
	}
	_, burstHistory := getHistory(t, base, "burst", "digital-media")
	if len(burstHistory) != 20 {
		t.Fatalf("burst history length = %d, want 20", len(burstHistory))
	}
	accepted := 0
	for _, r := range burstHistory {
		if r.Status == "ACCEPTED" {
			accepted++
		}
		if r.SourceRevision == 20 && r.Status != "ACCEPTED" {
			t.Fatalf("revision 20 status = %s, want ACCEPTED", r.Status)
		}
	}
	outboxN := testdb.CountRows(t, repo.Pool(), "outbox_events",
		"aggregate_key = $1", "44|2025|B1|burst|digital-media")
	if outboxN != accepted {
		t.Fatalf("burst outbox events = %d, want %d (exactly one per ACCEPTED record)", outboxN, accepted)
	}
	auditN := testdb.CountRows(t, repo.Pool(), "audit_log",
		"school_code = $1", "burst")
	if auditN != 20 {
		t.Fatalf("burst audit rows = %d, want 20 (every decision is audited)", auditN)
	}
}

// TestRankingTieOrderingOverHTTP verifies the deterministic tie-break: equal
// scores order by submitted_at, then school_code, then major_group_code, all
// ascending. It also checks INTEGER-scale display (6100 -> "610") end to end.
func TestRankingTieOrderingOverHTTP(t *testing.T) {
	srv, _ := newTestServer(t)
	base := srv.URL
	t1 := time.Date(2025, 8, 25, 9, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	type fixture struct {
		id, school, group, scale string
		value                    int64
		at                       time.Time
	}
	fixtures := []fixture{
		{"tie-1", "bbb", "g1", "DECIMAL_1", 6000, t1}, // earliest submitted_at
		{"tie-2", "aaa", "g1", "DECIMAL_1", 6000, t2}, // same time as tie-3, school aaa < aaa... group decides
		{"tie-3", "aaa", "g0", "DECIMAL_1", 6000, t2},
		{"tie-4", "int-school", "g1", "INTEGER", 6100, t1}, // highest score, INTEGER display "610"
	}
	for _, f := range fixtures {
		st, _ := postSubmission(t, base, submissionBody(f.id, f.school, f.group, f.scale, f.value, 1, f.at))
		if st != http.StatusCreated {
			t.Fatalf("fixture %s status = %d, want 201", f.id, st)
		}
	}

	rows := getRankings(t, base)
	want := []struct {
		school, group, display string
		rank                   int
	}{
		{"int-school", "g1", "610", 1},
		{"bbb", "g1", "600.0", 2}, // t1 beats t2
		{"aaa", "g0", "600.0", 3}, // t2 + aaa, group g0 < g1
		{"aaa", "g1", "600.0", 4},
	}
	if len(rows) != len(want) {
		t.Fatalf("ranking length = %d, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].SchoolCode != w.school || rows[i].MajorGroupCode != w.group ||
			rows[i].ScoreDisplay != w.display || rows[i].Rank != w.rank {
			t.Fatalf("rank %d = (%s,%s,%s), want (%s,%s,%s)",
				i+1, rows[i].SchoolCode, rows[i].MajorGroupCode, rows[i].ScoreDisplay,
				w.school, w.group, w.display)
		}
	}
}

// TestProblemResponses verifies every error path returns application/problem+json
// with type/title/status/code/trace_id populated.
func TestProblemResponses(t *testing.T) {
	srv, _ := newTestServer(t)
	base := srv.URL

	assertProblem := func(t *testing.T, status int, headers http.Header, data []byte, wantCode string) {
		t.Helper()
		if ct := headers.Get("Content-Type"); ct != httpserver.ProblemContentType {
			t.Fatalf("Content-Type = %q, want %q", ct, httpserver.ProblemContentType)
		}
		var p problemAPIResponse
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatalf("decode problem: %v", err)
		}
		if p.Status != status || p.Code != wantCode || p.Type == "" || p.Title == "" || p.TraceID == "" {
			t.Fatalf("problem = %+v, want status %d code %s and all fields set", p, status, wantCode)
		}
	}

	// 400 validation error: missing required fields.
	st, headers, data := doJSON(t, http.MethodPost, base+"/v1/submissions",
		map[string]any{"submission_id": "broken"})
	if st != http.StatusBadRequest {
		t.Fatalf("invalid submission status = %d, want 400", st)
	}
	assertProblem(t, http.StatusBadRequest, headers, data, "VALIDATION_ERROR")

	// 400 malformed JSON.
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/submissions", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do malformed request: %v", err)
	}
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON status = %d, want 400", resp.StatusCode)
	}
	assertProblem(t, http.StatusBadRequest, resp.Header, data, "MALFORMED_JSON")

	// 404 unknown history.
	st, headers, data = doJSON(t, http.MethodGet,
		base+"/v1/history/ghost/ghost?province_code=44&admission_year=2025&batch_code=B1", nil)
	if st != http.StatusNotFound {
		t.Fatalf("unknown history status = %d, want 404", st)
	}
	assertProblem(t, http.StatusNotFound, headers, data, "HISTORY_NOT_FOUND")

	// 404 unknown route.
	st, headers, data = doJSON(t, http.MethodGet, base+"/v1/does-not-exist", nil)
	if st != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", st)
	}
	assertProblem(t, http.StatusNotFound, headers, data, "ROUTE_NOT_FOUND")

	// 405 wrong method.
	st, headers, data = doJSON(t, http.MethodPut, base+"/v1/submissions", nil)
	if st != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status = %d, want 405", st)
	}
	assertProblem(t, http.StatusMethodNotAllowed, headers, data, "METHOD_NOT_ALLOWED")

	// rankings requires province_code / admission_year.
	st, headers, data = doJSON(t, http.MethodGet, base+"/v1/rankings", nil)
	if st != http.StatusBadRequest {
		t.Fatalf("rankings without filters status = %d, want 400", st)
	}
	assertProblem(t, http.StatusBadRequest, headers, data, "VALIDATION_ERROR")
}
