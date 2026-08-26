package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteProblemShape(t *testing.T) {
	traceID := "trace-unit-1"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, http.StatusConflict, codeSubmission409,
			"Submission conflict", "submission_id reused with a different payload", nil)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions", nil)
	req = req.WithContext(ContextWithTraceID(req.Context(), traceID))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Fatalf("Content-Type = %q, want %q", ct, ProblemContentType)
	}
	if got := rec.Header().Get("X-Trace-Id"); got != traceID {
		t.Fatalf("X-Trace-Id = %q, want %q", got, traceID)
	}

	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem body is not JSON: %v", err)
	}
	for _, pair := range [][2]string{
		{"type", p.Type}, {"title", p.Title}, {"code", p.Code}, {"trace_id", p.TraceID},
	} {
		if pair[1] == "" {
			t.Errorf("problem field %s must not be empty", pair[0])
		}
	}
	if p.Status != http.StatusConflict {
		t.Errorf("problem status = %d, want 409", p.Status)
	}
	if p.Code != codeSubmission409 {
		t.Errorf("problem code = %q, want %q", p.Code, codeSubmission409)
	}
	if p.TraceID != traceID {
		t.Errorf("problem trace_id = %q, want %q", p.TraceID, traceID)
	}
	if p.Instance != "/v1/submissions" {
		t.Errorf("problem instance = %q, want /v1/submissions", p.Instance)
	}
}

func TestTraceMiddlewareHonoursHeader(t *testing.T) {
	var seen string
	h := traceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = TraceIDFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/rankings", nil)
	req.Header.Set("X-Trace-Id", "trace-from-header")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "trace-from-header" {
		t.Fatalf("context trace id = %q, want trace-from-header", seen)
	}
	if rec.Header().Get("X-Trace-Id") != "trace-from-header" {
		t.Fatalf("response X-Trace-Id = %q, want trace-from-header", rec.Header().Get("X-Trace-Id"))
	}
}

func TestTraceMiddlewareGeneratesWhenAbsent(t *testing.T) {
	h := traceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/rankings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	id := rec.Header().Get("X-Trace-Id")
	if len(id) != 32 {
		t.Fatalf("generated trace id = %q, want 32 hex chars", id)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/rankings", nil))
	if rec2.Header().Get("X-Trace-Id") == id {
		t.Fatal("trace middleware must generate unique trace ids")
	}
}

func TestTraceIDRoundTrip(t *testing.T) {
	ctx := ContextWithTraceID(t.Context(), "abc")
	if got := TraceIDFromContext(ctx); got != "abc" {
		t.Fatalf("TraceIDFromContext = %q, want abc", got)
	}
	if got := TraceIDFromContext(t.Context()); got != "" {
		t.Fatalf("TraceIDFromContext on empty context = %q, want empty", got)
	}
}

func TestNewTraceID(t *testing.T) {
	id := newTraceID()
	if len(id) != 32 {
		t.Fatalf("newTraceID = %q, want 32 hex chars", id)
	}
	if id == newTraceID() {
		t.Fatal("newTraceID must produce unique values")
	}
}
