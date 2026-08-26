// Package httpserver provides the HTTP handlers and middleware for the admission score ledger API.
package httpserver

import (
	"encoding/json"
	"net/http"
)

// ProblemType is the URI identifying the problem type.
const ProblemType = "about:blank"

// Problem represents an RFC 7807 application/problem+json error response.
type Problem struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Status  int    `json:"status"`
	Code    string `json:"code"`
	TraceID string `json:"trace_id"`
	Detail  string `json:"detail,omitempty"`
}

// Error codes used in the Problem.Code field.
const (
	CodeValidationFailed = "VALIDATION_FAILED"
	CodeConflict         = "IDEMPOTENCY_CONFLICT"
	CodeBadRequest       = "BAD_REQUEST"
	CodeNotFound         = "NOT_FOUND"
	CodeInternal         = "INTERNAL_ERROR"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	CodeUnsupportedMedia = "UNSUPPORTED_MEDIA_TYPE"
)

func writeProblem(w http.ResponseWriter, traceID string, status int, code, title, detail string) {
	p := Problem{
		Type:    ProblemType,
		Title:   title,
		Status:  status,
		Code:    code,
		TraceID: traceID,
		Detail:  detail,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}
