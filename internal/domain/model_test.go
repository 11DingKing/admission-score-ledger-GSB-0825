package domain

import (
	"testing"
	"time"
)

func TestFormatScoreInteger(t *testing.T) {
	tests := []struct {
		name  string
		value int64
		want  string
	}{
		{"6100 -> 610", 6100, "610"},
		{"0 -> 0", 0, "0"},
		{"500 -> 50", 500, "50"},
		{"10000 -> 1000", 10000, "1000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatScore(ScoreScaleInteger, tt.value)
			if got != tt.want {
				t.Errorf("FormatScore(INTEGER, %d) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestFormatScoreDecimal1(t *testing.T) {
	tests := []struct {
		name  string
		value int64
		want  string
	}{
		{"6044 -> 604.4", 6044, "604.4"},
		{"6100 -> 610.0", 6100, "610.0"},
		{"5 -> 0.5", 5, "0.5"},
		{"0 -> 0.0", 0, "0.0"},
		{"9999 -> 999.9", 9999, "999.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatScore(ScoreScaleDecimal1, tt.value)
			if got != tt.want {
				t.Errorf("FormatScore(DECIMAL_1, %d) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestScoreScaleValid(t *testing.T) {
	if !ScoreScaleInteger.Valid() {
		t.Error("INTEGER should be valid")
	}
	if !ScoreScaleDecimal1.Valid() {
		t.Error("DECIMAL_1 should be valid")
	}
	if ScoreScale("FLOAT").Valid() {
		t.Error("FLOAT should not be valid")
	}
}

func TestSubmissionRequestValidate(t *testing.T) {
	valid := SubmissionRequest{
		SubmissionID:   "550e8400-e29b-41d4-a716-446655440000",
		ProvinceCode:   "GD",
		AdmissionYear:  2025,
		BatchCode:      "本科批",
		SchoolCode:     "szpu",
		MajorGroupCode: "digital-media",
		ScoreScale:     ScoreScaleInteger,
		ScoreValue:     6100,
		SubmittedAt:    time.Now().UTC(),
		RuleVersion:    "v1",
		SourceRevision: 1,
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("expected valid request, got: %v", err)
	}

	missingID := valid
	missingID.SubmissionID = ""
	if err := missingID.Validate(); err == nil {
		t.Error("expected error for missing submission_id")
	}

	invalidYear := valid
	invalidYear.AdmissionYear = 0
	if err := invalidYear.Validate(); err == nil {
		t.Error("expected error for invalid admission_year")
	}

	invalidScale := valid
	invalidScale.ScoreScale = "FLOAT"
	if err := invalidScale.Validate(); err == nil {
		t.Error("expected error for invalid score_scale")
	}

	negativeScore := valid
	negativeScore.ScoreValue = -1
	if err := negativeScore.Validate(); err == nil {
		t.Error("expected error for negative score_value")
	}

	invalidRevision := valid
	invalidRevision.SourceRevision = 0
	if err := invalidRevision.Validate(); err == nil {
		t.Error("expected error for invalid source_revision")
	}
}

func TestNaturalKey(t *testing.T) {
	s := Submission{
		ProvinceCode:   "GD",
		AdmissionYear:  2025,
		BatchCode:      "B1",
		SchoolCode:     "S1",
		MajorGroupCode: "M1",
	}
	key := s.NaturalKey()
	if key.ProvinceCode != "GD" || key.AdmissionYear != 2025 || key.SchoolCode != "S1" {
		t.Errorf("unexpected natural key: %+v", key)
	}
}
