package service

import (
	"strings"
	"testing"
	"time"

	"admission-score-ledger/internal/domain"
)

func validSubmission() domain.Submission {
	return domain.Submission{
		SubmissionID:   "sub-valid",
		ProvinceCode:   "44",
		AdmissionYear:  2025,
		BatchCode:      "B1",
		SchoolCode:     "szpu",
		MajorGroupCode: "digital-media",
		ScoreScale:     domain.ScaleInteger,
		ScoreValue:     6100,
		SubmittedAt:    time.Date(2025, 8, 25, 9, 0, 0, 0, time.UTC),
		RuleVersion:    "rv1",
		SourceRevision: 1,
	}
}

func TestValidateAcceptsWellFormed(t *testing.T) {
	if errs := Validate(validSubmission()); len(errs) != 0 {
		t.Fatalf("expected no validation errors, got %v", errs)
	}
	decimal := validSubmission()
	decimal.ScoreScale = domain.ScaleDecimal1
	decimal.ScoreValue = 6044
	if errs := Validate(decimal); len(errs) != 0 {
		t.Fatalf("DECIMAL_1 6044 should be valid, got %v", errs)
	}
}

func TestValidateRejectsMissingFields(t *testing.T) {
	errs := Validate(domain.Submission{})
	fields := map[string]bool{}
	for _, e := range errs {
		fields[e.Field] = true
	}
	for _, f := range []string{
		"submission_id", "province_code", "admission_year", "batch_code",
		"school_code", "major_group_code", "score_scale",
		"source_revision", "rule_version", "submitted_at",
	} {
		if !fields[f] {
			t.Errorf("expected validation error for %q, got fields %v", f, fields)
		}
	}
}

func TestValidateRejectsIntegerWithTenths(t *testing.T) {
	s := validSubmission()
	s.ScoreScale = domain.ScaleInteger
	s.ScoreValue = 6105 // 610.5 is not representable on an INTEGER scale
	errs := Validate(s)
	found := false
	for _, e := range errs {
		if e.Field == "score_value" && strings.Contains(e.Reason, "multiple of 10") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected INTEGER tenths error, got %v", errs)
	}
}

func TestValidateRejectsBadScaleAndRevision(t *testing.T) {
	s := validSubmission()
	s.ScoreScale = domain.ScoreScale("FLOAT")
	errs := Validate(s)
	if len(errs) == 0 {
		t.Fatal("expected error for unsupported score_scale")
	}

	s = validSubmission()
	s.SourceRevision = 0
	errs = Validate(s)
	if !hasField(errs, "source_revision") {
		t.Fatalf("expected source_revision error, got %v", errs)
	}
}

func TestValidateRejectsNegativeScore(t *testing.T) {
	s := validSubmission()
	s.ScoreValue = -10
	errs := Validate(s)
	if !hasField(errs, "score_value") {
		t.Fatalf("expected score_value error for negative value, got %v", errs)
	}
}

func hasField(errs []FieldError, field string) bool {
	for _, e := range errs {
		if e.Field == field {
			return true
		}
	}
	return false
}

func TestValidationErrorMessage(t *testing.T) {
	err := &ValidationError{Fields: []FieldError{{Field: "score_value", Reason: "bad"}}}
	if !strings.Contains(err.Error(), "score_value: bad") {
		t.Fatalf("unexpected error message %q", err.Error())
	}
}
