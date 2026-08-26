package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gsb/admission-score-ledger/internal/domain"
	"github.com/gsb/admission-score-ledger/internal/repository"
	"github.com/gsb/admission-score-ledger/internal/service"
)

// Handler holds HTTP handler dependencies.
type Handler struct {
	svc *service.Service
}

// NewHandler creates a new Handler.
func NewHandler(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// Routes returns the configured HTTP handler with all routes registered.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/submissions", h.handleCreateSubmission)
	mux.HandleFunc("GET /v1/rankings", h.handleListRankings)
	mux.HandleFunc("GET /v1/history/{school_code}/{major_group_code}", h.handleGetHistory)
	mux.HandleFunc("/", h.handleNotFound)

	return mux
}

func (h *Handler) handleCreateSubmission(w http.ResponseWriter, r *http.Request) {
	traceID := TraceIDFromContext(r.Context())

	var req domain.SubmissionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, traceID, http.StatusBadRequest, CodeBadRequest,
			"Bad Request", "invalid JSON body: "+err.Error())
		return
	}

	resp, err := h.svc.Submit(r.Context(), req, traceID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrValidation):
			writeProblem(w, traceID, http.StatusBadRequest, CodeValidationFailed,
				"Validation Failed", err.Error())
		case errors.Is(err, service.ErrConflict):
			writeProblem(w, traceID, http.StatusConflict, CodeConflict,
				"Conflict", "submission_id was already used with a different payload")
		default:
			writeProblem(w, traceID, http.StatusInternalServerError, CodeInternal,
				"Internal Server Error", err.Error())
		}
		return
	}

	status := http.StatusCreated
	if resp.Duplicate {
		status = http.StatusOK
	}
	if resp.Status == domain.SubmissionStatusStaleIgnored {
		status = http.StatusAccepted
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) handleListRankings(w http.ResponseWriter, r *http.Request) {
	traceID := TraceIDFromContext(r.Context())

	filter := repository.RankingFilter{
		ProvinceCode: r.URL.Query().Get("province_code"),
		BatchCode:    r.URL.Query().Get("batch_code"),
	}
	if yearStr := r.URL.Query().Get("admission_year"); yearStr != "" {
		year, err := strconv.Atoi(yearStr)
		if err != nil {
			writeProblem(w, traceID, http.StatusBadRequest, CodeBadRequest,
				"Bad Request", "admission_year must be an integer")
			return
		}
		filter.AdmissionYear = year
	}
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			writeProblem(w, traceID, http.StatusBadRequest, CodeBadRequest,
				"Bad Request", "limit must be a positive integer")
			return
		}
		filter.Limit = limit
	}

	rankings, err := h.svc.ListRankings(r.Context(), filter)
	if err != nil {
		writeProblem(w, traceID, http.StatusInternalServerError, CodeInternal,
			"Internal Server Error", err.Error())
		return
	}

	if rankings == nil {
		rankings = []domain.RankingEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"items": rankings,
		"count": len(rankings),
	})
}

func (h *Handler) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	traceID := TraceIDFromContext(r.Context())

	schoolCode := r.PathValue("school_code")
	majorGroupCode := r.PathValue("major_group_code")

	provinceCode := r.URL.Query().Get("province_code")
	batchCode := r.URL.Query().Get("batch_code")

	var admissionYear int
	if yearStr := r.URL.Query().Get("admission_year"); yearStr != "" {
		year, err := strconv.Atoi(yearStr)
		if err != nil {
			writeProblem(w, traceID, http.StatusBadRequest, CodeBadRequest,
				"Bad Request", "admission_year must be an integer")
			return
		}
		admissionYear = year
	}

	if schoolCode == "" || majorGroupCode == "" {
		writeProblem(w, traceID, http.StatusBadRequest, CodeBadRequest,
			"Bad Request", "school_code and major_group_code are required")
		return
	}

	key := domain.NaturalKey{
		ProvinceCode:   provinceCode,
		AdmissionYear:  admissionYear,
		BatchCode:      batchCode,
		SchoolCode:     schoolCode,
		MajorGroupCode: majorGroupCode,
	}

	history, err := h.svc.ListHistory(r.Context(), key)
	if err != nil {
		writeProblem(w, traceID, http.StatusInternalServerError, CodeInternal,
			"Internal Server Error", err.Error())
		return
	}
	if len(history) == 0 {
		writeProblem(w, traceID, http.StatusNotFound, CodeNotFound,
			"Not Found", "no history found for the given school and major group")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"school_code":      schoolCode,
		"major_group_code": majorGroupCode,
		"items":            history,
		"count":            len(history),
	})
}

func (h *Handler) handleNotFound(w http.ResponseWriter, r *http.Request) {
	traceID := TraceIDFromContext(r.Context())
	if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/v1/") {
		writeProblem(w, traceID, http.StatusNotFound, CodeNotFound,
			"Not Found", "the requested resource was not found")
		return
	}
	writeProblem(w, traceID, http.StatusNotFound, CodeNotFound,
		"Not Found", "the requested resource was not found")
}
