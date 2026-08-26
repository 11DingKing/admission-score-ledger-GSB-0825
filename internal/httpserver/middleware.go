package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"time"
)

type contextKey string

const traceIDKey contextKey = "trace_id"

// TraceIDFromContext extracts the trace ID from the request context.
func TraceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey).(string); ok {
		return v
	}
	return ""
}

func generateTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// TraceMiddleware injects a trace ID into the request context and response header.
func TraceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := r.Header.Get("X-Request-ID")
		if traceID == "" {
			traceID = generateTraceID()
		}
		w.Header().Set("X-Trace-ID", traceID)
		ctx := context.WithValue(r.Context(), traceIDKey, traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// LoggingMiddleware logs each request with method, path, status, duration and trace ID.
func LoggingMiddleware(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		traceID := TraceIDFromContext(r.Context())
		logger.Printf("%s %s %d %s trace_id=%s",
			r.Method, r.URL.Path, sw.status, time.Since(start), traceID)
	})
}

// RecoveryMiddleware recovers from panics and returns a 500 problem+json.
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				traceID := TraceIDFromContext(r.Context())
				writeProblem(w, traceID, http.StatusInternalServerError, CodeInternal,
					"Internal Server Error", "an unexpected error occurred")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// JSONContentTypeMiddleware enforces application/json for requests with a body.
func JSONContentTypeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			ct := r.Header.Get("Content-Type")
			if ct != "" && ct != "application/json" {
				traceID := TraceIDFromContext(r.Context())
				writeProblem(w, traceID, http.StatusUnsupportedMediaType, CodeUnsupportedMedia,
					"Unsupported Media Type", "Content-Type must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
