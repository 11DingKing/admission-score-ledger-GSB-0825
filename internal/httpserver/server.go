// Package httpserver exposes the admission score ledger over HTTP using only
// the standard library net/http package.
package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"admission-score-ledger/internal/domain"
	"admission-score-ledger/internal/repository"
	"admission-score-ledger/internal/service"
)

type traceContextKey struct{}

// Server wires the ledger service to HTTP handlers.
type Server struct {
	ledger *service.Ledger
}

// New builds the HTTP handler tree with routing and trace middleware.
func New(ledger *service.Ledger) http.Handler {
	s := &Server{ledger: ledger}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/submissions", s.handleSubmissions)
	mux.HandleFunc("/v1/rankings", s.handleRankings)
	mux.HandleFunc("/v1/history/{school_code}/{major_group_code}", s.handleHistory)
	mux.HandleFunc("/", s.handleNotFound)
	return traceMiddleware(mux)
}

// TraceIDFromContext returns the trace id stored in ctx, if any.
func TraceIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(traceContextKey{}).(string)
	return v
}

// ContextWithTraceID returns a copy of ctx carrying traceID.
func ContextWithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceContextKey{}, traceID)
}

func traceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := r.Header.Get("X-Trace-Id")
		if traceID == "" {
			traceID = newTraceID()
		}
		w.Header().Set("X-Trace-Id", traceID)
		ctx := context.WithValue(r.Context(), traceContextKey{}, traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

type submitRequest struct {
	SubmissionID   string `json:"submission_id"`
	ProvinceCode   string `json:"province_code"`
	AdmissionYear  *int   `json:"admission_year"`
	BatchCode      string `json:"batch_code"`
	SchoolCode     string `json:"school_code"`
	MajorGroupCode string `json:"major_group_code"`
	ScoreScale     string `json:"score_scale"`
	ScoreValue     *int64 `json:"score_value"`
	SubmittedAt    string `json:"submitted_at"`
	RuleVersion    string `json:"rule_version"`
	SourceRevision *int64 `json:"source_revision"`
}

type submitResponse struct {
	Decision     string                `json:"decision"`
	Replayed     bool                  `json:"replayed"`
	SubmissionID string                `json:"submission_id"`
	Current      *domain.Snapshot      `json:"current"`
	Received     *domain.HistoryRecord `json:"received"`
}

type rankingsResponse struct {
	ProvinceCode  string              `json:"province_code"`
	AdmissionYear int                 `json:"admission_year"`
	BatchCode     string              `json:"batch_code,omitempty"`
	Rankings      []domain.RankingRow `json:"rankings"`
}

type historyResponse struct {
	NaturalKey domain.NaturalKey      `json:"natural_key"`
	Records    []domain.HistoryRecord `json:"records"`
}

func (s *Server) handleSubmissions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProblem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllow,
			"Method not allowed", "POST is required for this endpoint", nil)
		return
	}

	var req submitRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, codeMalformedJSON,
			"Request body is not valid JSON", err.Error(), nil)
		return
	}

	var fieldErrs []service.FieldError
	if req.AdmissionYear == nil {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "admission_year", Reason: "is required"})
	}
	if req.ScoreValue == nil {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "score_value", Reason: "is required and must be integer tenths of a point"})
	}
	if req.SourceRevision == nil {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "source_revision", Reason: "is required"})
	}

	submittedAt, err := time.Parse(time.RFC3339, req.SubmittedAt)
	if req.SubmittedAt == "" {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "submitted_at", Reason: "is required and must be an RFC3339 timestamp"})
	} else if err != nil {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "submitted_at", Reason: "must be an RFC3339 timestamp"})
	}

	sub := domain.Submission{
		SubmissionID:   req.SubmissionID,
		ProvinceCode:   req.ProvinceCode,
		BatchCode:      req.BatchCode,
		SchoolCode:     req.SchoolCode,
		MajorGroupCode: req.MajorGroupCode,
		ScoreScale:     domain.ScoreScale(req.ScoreScale),
		RuleVersion:    req.RuleVersion,
		SubmittedAt:    submittedAt,
	}
	if req.AdmissionYear != nil {
		sub.AdmissionYear = *req.AdmissionYear
	}
	if req.ScoreValue != nil {
		sub.ScoreValue = *req.ScoreValue
	}
	if req.SourceRevision != nil {
		sub.SourceRevision = *req.SourceRevision
	}
	fieldErrs = append(fieldErrs, service.Validate(sub)...)

	if len(fieldErrs) > 0 {
		writeProblem(w, r, http.StatusBadRequest, codeValidation,
			"One or more fields are invalid", "reject the submission and write nothing", fieldErrs)
		return
	}

	result, err := s.ledger.Submit(r.Context(), sub, TraceIDFromContext(r.Context()))
	if err != nil {
		var vErr *service.ValidationError
		if errors.As(err, &vErr) {
			writeProblem(w, r, http.StatusBadRequest, codeValidation,
				"One or more fields are invalid", "", vErr.Fields)
			return
		}
		if errors.Is(err, service.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, codeSubmission409,
				"submission_id conflicts with an earlier submission",
				"the same submission_id was already used with a different payload", nil)
			return
		}
		log.Printf("submit failed: %v", err)
		writeProblem(w, r, http.StatusInternalServerError, codeInternal, "Internal server error", "", nil)
		return
	}

	status := http.StatusCreated
	if result.Decision == domain.DecisionStaleIgnored {
		status = http.StatusAccepted
	}
	writeJSON(w, status, submitResponse{
		Decision:     string(result.Decision),
		Replayed:     result.Replayed,
		SubmissionID: sub.SubmissionID,
		Current:      result.Current,
		Received:     result.Received,
	})
}

func (s *Server) handleRankings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllow,
			"Method not allowed", "GET is required for this endpoint", nil)
		return
	}
	q := r.URL.Query()
	var fieldErrs []service.FieldError
	province := q.Get("province_code")
	if province == "" {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "province_code", Reason: "is required"})
	}
	year, err := strconv.Atoi(q.Get("admission_year"))
	if q.Get("admission_year") == "" {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "admission_year", Reason: "is required"})
	} else if err != nil || year < 2000 || year > 2100 {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "admission_year", Reason: "must be a year between 2000 and 2100"})
	}
	if len(fieldErrs) > 0 {
		writeProblem(w, r, http.StatusBadRequest, codeValidation,
			"One or more query parameters are invalid", "", fieldErrs)
		return
	}

	rows, err := s.ledger.Rankings(r.Context(), repository.RankingFilter{
		ProvinceCode:  province,
		AdmissionYear: year,
		BatchCode:     q.Get("batch_code"),
	})
	if err != nil {
		log.Printf("rankings failed: %v", err)
		writeProblem(w, r, http.StatusInternalServerError, codeInternal, "Internal server error", "", nil)
		return
	}
	if rows == nil {
		rows = []domain.RankingRow{}
	}
	writeJSON(w, http.StatusOK, rankingsResponse{
		ProvinceCode:  province,
		AdmissionYear: year,
		BatchCode:     q.Get("batch_code"),
		Rankings:      rows,
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllow,
			"Method not allowed", "GET is required for this endpoint", nil)
		return
	}
	q := r.URL.Query()
	var fieldErrs []service.FieldError
	province := q.Get("province_code")
	if province == "" {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "province_code", Reason: "is required"})
	}
	batch := q.Get("batch_code")
	if batch == "" {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "batch_code", Reason: "is required"})
	}
	year, err := strconv.Atoi(q.Get("admission_year"))
	if q.Get("admission_year") == "" {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "admission_year", Reason: "is required"})
	} else if err != nil || year < 2000 || year > 2100 {
		fieldErrs = append(fieldErrs, service.FieldError{Field: "admission_year", Reason: "must be a year between 2000 and 2100"})
	}
	if len(fieldErrs) > 0 {
		writeProblem(w, r, http.StatusBadRequest, codeValidation,
			"One or more query parameters are invalid", "", fieldErrs)
		return
	}

	key := domain.NaturalKey{
		ProvinceCode:   province,
		AdmissionYear:  year,
		BatchCode:      batch,
		SchoolCode:     r.PathValue("school_code"),
		MajorGroupCode: r.PathValue("major_group_code"),
	}
	records, err := s.ledger.History(r.Context(), key)
	if err != nil {
		log.Printf("history failed: %v", err)
		writeProblem(w, r, http.StatusInternalServerError, codeInternal, "Internal server error", "", nil)
		return
	}
	if len(records) == 0 {
		writeProblem(w, r, http.StatusNotFound, codeHistoryMissing,
			"No history found for this school major group",
			"no submission record exists for the given natural key", nil)
		return
	}
	writeJSON(w, http.StatusOK, historyResponse{NaturalKey: key, Records: records})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusNotFound, codeRouteNotFound,
		"Route not found", r.URL.Path+" does not exist", nil)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write response: %v", err)
	}
}
