package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/gsb/admission-score-ledger/internal/domain"
	"github.com/gsb/admission-score-ledger/internal/repository"
	"github.com/gsb/admission-score-ledger/internal/service"
)

const testDBURL = "postgres://huangding@localhost:5432/ledger_test?sslmode=disable"

func setupTestServer(t *testing.T) (http.Handler, *sql.DB) {
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
		t.Skipf("skip: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("skip (no db): %v", err)
	}
	if err := repository.RunMigrations(ctx, db); err != nil {
		db.Close()
		t.Fatalf("migrations: %v", err)
	}
	db.Exec(`TRUNCATE TABLE outbox, audit_log, current_snapshots, submissions RESTART IDENTITY CASCADE`)
	t.Cleanup(func() {
		db.Exec(`TRUNCATE TABLE outbox, audit_log, current_snapshots, submissions RESTART IDENTITY CASCADE`)
		db.Close()
	})
	repo := repository.NewRepo(db)
	svc := service.NewService(repo)
	h := NewHandler(svc)
	handler := h.Routes()
	handler = JSONContentTypeMiddleware(handler)
	handler = TraceMiddleware(handler)
	handler = RecoveryMiddleware(handler)
	return handler, db
}

func TestHTTPCreateSubmission(t *testing.T) {
	handler, _ := setupTestServer(t)

	body := domain.SubmissionRequest{
		SubmissionID:   "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		ProvinceCode:   "GD",
		AdmissionYear:  2025,
		BatchCode:      "B1",
		SchoolCode:     "szpu",
		MajorGroupCode: "digital-media",
		ScoreScale:     domain.ScoreScaleInteger,
		ScoreValue:     6100,
		SubmittedAt:    time.Now().UTC(),
		RuleVersion:    "v1",
		SourceRevision: 1,
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/submissions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp domain.SubmissionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != domain.SubmissionStatusAccepted {
		t.Errorf("expected ACCEPTED, got %s", resp.Status)
	}
	if resp.CurrentDisplay != "610" {
		t.Errorf("expected 610, got %s", resp.CurrentDisplay)
	}
}

func TestHTTPStaleReturns202(t *testing.T) {
	handler, _ := setupTestServer(t)

	send := func(id string, rev int32, score int64) *httptest.ResponseRecorder {
		body := domain.SubmissionRequest{
			SubmissionID:   id,
			ProvinceCode:   "GD",
			AdmissionYear:  2025,
			BatchCode:      "B1",
			SchoolCode:     "szpu",
			MajorGroupCode: "digital-media",
			ScoreScale:     domain.ScoreScaleInteger,
			ScoreValue:     score,
			SubmittedAt:    time.Now().UTC(),
			RuleVersion:    "v1",
			SourceRevision: rev,
		}
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/submissions", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec1 := send("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 2, 6100)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first: expected 201, got %d", rec1.Code)
	}

	rec2 := send("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", 1, 6000)
	if rec2.Code != http.StatusAccepted {
		t.Errorf("stale: expected 202, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestHTTPConflictReturnsProblemJSON(t *testing.T) {
	handler, _ := setupTestServer(t)

	send := func(score int64) *httptest.ResponseRecorder {
		body := domain.SubmissionRequest{
			SubmissionID:   "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			ProvinceCode:   "GD",
			AdmissionYear:  2025,
			BatchCode:      "B1",
			SchoolCode:     "szpu",
			MajorGroupCode: "digital-media",
			ScoreScale:     domain.ScoreScaleInteger,
			ScoreValue:     score,
			SubmittedAt:    time.Now().UTC(),
			RuleVersion:    "v1",
			SourceRevision: 1,
		}
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/submissions", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec1 := send(6100)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first: expected 201, got %d", rec1.Code)
	}

	rec2 := send(9999)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec2.Code)
	}
	ct := rec2.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("expected application/problem+json, got %s", ct)
	}
	var problem Problem
	if err := json.Unmarshal(rec2.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Type == "" || problem.Title == "" || problem.Status != 409 ||
		problem.Code == "" || problem.TraceID == "" {
		t.Errorf("problem+json missing required fields: %+v", problem)
	}
}

func TestHTTPRankingsAndHistory(t *testing.T) {
	handler, _ := setupTestServer(t)

	send := func(id, school, major string, rev int32, score int64, scale domain.ScoreScale) {
		body := domain.SubmissionRequest{
			SubmissionID:   id,
			ProvinceCode:   "GD",
			AdmissionYear:  2025,
			BatchCode:      "B1",
			SchoolCode:     school,
			MajorGroupCode: major,
			ScoreScale:     scale,
			ScoreValue:     score,
			SubmittedAt:    time.Now().UTC(),
			RuleVersion:    "v1",
			SourceRevision: rev,
		}
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/submissions", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("setup submit failed: %d %s", rec.Code, rec.Body.String())
		}
	}

	send("11111111-1111-1111-1111-111111111111", "szpu", "digital-media", 1, 6100, domain.ScoreScaleInteger)
	send("22222222-2222-2222-2222-222222222222", "whpu", "digital-media", 1, 6044, domain.ScoreScaleDecimal1)

	req := httptest.NewRequest(http.MethodGet, "/v1/rankings", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rankings: expected 200, got %d", rec.Code)
	}
	var rankings struct {
		Items []domain.RankingEntry `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &rankings)
	if len(rankings.Items) != 2 {
		t.Fatalf("expected 2 rankings, got %d", len(rankings.Items))
	}
	if rankings.Items[0].ScoreDisplay != "610" {
		t.Errorf("expected 610, got %s", rankings.Items[0].ScoreDisplay)
	}
	if rankings.Items[1].ScoreDisplay != "604.4" {
		t.Errorf("expected 604.4, got %s", rankings.Items[1].ScoreDisplay)
	}

	histReq := httptest.NewRequest(http.MethodGet,
		"/v1/history/szpu/digital-media?province_code=GD&admission_year=2025&batch_code=B1", nil)
	histRec := httptest.NewRecorder()
	handler.ServeHTTP(histRec, histReq)
	if histRec.Code != http.StatusOK {
		t.Fatalf("history: expected 200, got %d: %s", histRec.Code, histRec.Body.String())
	}
	var history struct {
		Items []domain.HistoryEntry `json:"items"`
	}
	json.Unmarshal(histRec.Body.Bytes(), &history)
	if len(history.Items) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(history.Items))
	}
}

func TestHTTPValidationError(t *testing.T) {
	handler, _ := setupTestServer(t)

	body := `{"submission_id":""}`
	req := httptest.NewRequest(http.MethodPost, "/v1/submissions", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
	var problem Problem
	json.Unmarshal(rec.Body.Bytes(), &problem)
	if problem.Code != CodeValidationFailed {
		t.Errorf("expected code %s, got %s", CodeValidationFailed, problem.Code)
	}
}
