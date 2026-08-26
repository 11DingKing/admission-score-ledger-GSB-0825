package httpserver

import (
	"encoding/json"
	"net/http"
)

// ProblemContentType is the RFC 9457 problem details media type.
const ProblemContentType = "application/problem+json"

// Problem is the error body returned by every failing endpoint. It follows
// RFC 9457 (type/title/status) and additionally carries a machine-readable
// code and the request trace id.
type Problem struct {
	// Type is a URI identifying the problem kind.
	Type string `json:"type"`
	// Title is a short human-readable summary.
	Title string `json:"title"`
	// Status is the HTTP status code.
	Status int `json:"status"`
	// Code is a stable machine-readable error code.
	Code string `json:"code"`
	// TraceID identifies the request for log correlation.
	TraceID string `json:"trace_id"`
	// Detail is a human-readable explanation specific to the occurrence.
	Detail string `json:"detail,omitempty"`
	// Instance is the request path that produced the problem.
	Instance string `json:"instance,omitempty"`
	// Errors carries per-field validation failures when present.
	Errors any `json:"errors,omitempty"`
}

// Problem codes used by the API.
const (
	codeMalformedJSON  = "MALFORMED_JSON"
	codeValidation     = "VALIDATION_ERROR"
	codeSubmission409  = "SUBMISSION_CONFLICT"
	codeHistoryMissing = "HISTORY_NOT_FOUND"
	codeRouteNotFound  = "ROUTE_NOT_FOUND"
	codeMethodNotAllow = "METHOD_NOT_ALLOWED"
	codeInternal       = "INTERNAL_ERROR"
)

const problemTypeBase = "https://admission-score-ledger.local/problems/"

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, title, detail string, errs any) {
	traceID := TraceIDFromContext(r.Context())
	p := Problem{
		Type:     problemTypeBase + code,
		Title:    title,
		Status:   status,
		Code:     code,
		TraceID:  traceID,
		Detail:   detail,
		Instance: r.URL.Path,
		Errors:   errs,
	}
	w.Header().Set("Content-Type", ProblemContentType)
	w.Header().Set("X-Trace-Id", traceID)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}
