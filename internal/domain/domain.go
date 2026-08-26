// Package domain contains the core types and business rules of the admission
// score ledger: submissions, current snapshots, history records, the integer
// score representation and the deterministic ranking order.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ScoreScale describes how an integer score value must be rendered.
type ScoreScale string

const (
	// ScaleInteger renders scores without a fractional digit, e.g. 6100 -> "610".
	ScaleInteger ScoreScale = "INTEGER"
	// ScaleDecimal1 renders scores with exactly one fractional digit, e.g. 6044 -> "604.4".
	ScaleDecimal1 ScoreScale = "DECIMAL_1"
)

// Decision is the outcome of processing a submission.
type Decision string

const (
	// DecisionAccepted means the submission became the current value of its natural key.
	DecisionAccepted Decision = "ACCEPTED"
	// DecisionStaleIgnored means the submission carried an older source_revision
	// and was ignored for the current snapshot but retained in history.
	DecisionStaleIgnored Decision = "STALE_IGNORED"
)

// Submission is a single admission score snapshot reported by a source.
//
// ScoreValue is always stored as an integer count of 0.1 points: 610 points
// is 6100 and 604.4 points is 6044. Floating point numbers must never be used
// to carry score data.
type Submission struct {
	SubmissionID   string     `json:"submission_id"`
	ProvinceCode   string     `json:"province_code"`
	AdmissionYear  int        `json:"admission_year"`
	BatchCode      string     `json:"batch_code"`
	SchoolCode     string     `json:"school_code"`
	MajorGroupCode string     `json:"major_group_code"`
	ScoreScale     ScoreScale `json:"score_scale"`
	ScoreValue     int64      `json:"score_value"`
	SubmittedAt    time.Time  `json:"submitted_at"`
	RuleVersion    string     `json:"rule_version"`
	SourceRevision int64      `json:"source_revision"`
}

// NaturalKey identifies the school major group whose score is being tracked.
type NaturalKey struct {
	ProvinceCode   string `json:"province_code"`
	AdmissionYear  int    `json:"admission_year"`
	BatchCode      string `json:"batch_code"`
	SchoolCode     string `json:"school_code"`
	MajorGroupCode string `json:"major_group_code"`
}

// Key returns the natural key of the submission.
func (s Submission) Key() NaturalKey {
	return NaturalKey{
		ProvinceCode:   s.ProvinceCode,
		AdmissionYear:  s.AdmissionYear,
		BatchCode:      s.BatchCode,
		SchoolCode:     s.SchoolCode,
		MajorGroupCode: s.MajorGroupCode,
	}
}

// String renders the natural key as a stable pipe-separated identifier,
// e.g. "44|2025|B1|szpu|digital-media". It is used as the outbox aggregate key.
func (k NaturalKey) String() string {
	return strings.Join([]string{
		k.ProvinceCode,
		strconv.Itoa(k.AdmissionYear),
		k.BatchCode,
		k.SchoolCode,
		k.MajorGroupCode,
	}, "|")
}

// Snapshot is the current accepted value of a natural key.
type Snapshot struct {
	NaturalKey
	ScoreScale     ScoreScale `json:"score_scale"`
	ScoreValue     int64      `json:"score_value"`
	ScoreDisplay   string     `json:"score_display"`
	SourceRevision int64      `json:"source_revision"`
	RuleVersion    string     `json:"rule_version"`
	SubmittedAt    time.Time  `json:"submitted_at"`
	SubmissionID   string     `json:"submission_id"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// HistoryRecord is one immutable entry of the change history of a natural key.
type HistoryRecord struct {
	SubmissionID   string     `json:"submission_id"`
	SourceRevision int64      `json:"source_revision"`
	ScoreScale     ScoreScale `json:"score_scale"`
	ScoreValue     int64      `json:"score_value"`
	ScoreDisplay   string     `json:"score_display"`
	Status         Decision   `json:"status"`
	RuleVersion    string     `json:"rule_version"`
	SubmittedAt    time.Time  `json:"submitted_at"`
	RecordedAt     time.Time  `json:"recorded_at"`
}

// RankingRow is one line of the highest-score ranking.
type RankingRow struct {
	Rank           int        `json:"rank"`
	SchoolCode     string     `json:"school_code"`
	MajorGroupCode string     `json:"major_group_code"`
	ScoreScale     ScoreScale `json:"score_scale"`
	ScoreValue     int64      `json:"score_value"`
	ScoreDisplay   string     `json:"score_display"`
	SourceRevision int64      `json:"source_revision"`
	RuleVersion    string     `json:"rule_version"`
	SubmittedAt    time.Time  `json:"submitted_at"`
}

// FormatScore renders an integer tenth-of-a-point score value for API display.
// INTEGER scales render without a fractional digit (6100 -> "610") while
// DECIMAL_1 scales always render one fractional digit (6044 -> "604.4").
func FormatScore(scale ScoreScale, value int64) string {
	whole := value / 10
	tenths := value % 10
	if scale == ScaleInteger {
		return itoa(whole)
	}
	return itoa(whole) + "." + itoa(tenths)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// PayloadHash returns a stable SHA-256 hash of the submission payload.
//
// The hash is used to detect idempotency-key conflicts: the same
// submission_id retried with a different payload must be rejected.
func PayloadHash(s Submission) string {
	type canonical struct {
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
	b, err := json.Marshal(canonical{
		SubmissionID:   s.SubmissionID,
		ProvinceCode:   s.ProvinceCode,
		AdmissionYear:  s.AdmissionYear,
		BatchCode:      s.BatchCode,
		SchoolCode:     s.SchoolCode,
		MajorGroupCode: s.MajorGroupCode,
		ScoreScale:     string(s.ScoreScale),
		ScoreValue:     s.ScoreValue,
		SubmittedAt:    s.SubmittedAt.UTC().Format(time.RFC3339Nano),
		RuleVersion:    s.RuleVersion,
		SourceRevision: s.SourceRevision,
	})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CompareRanking defines the deterministic ranking order.
//
// Higher scores come first; when the displayed score is equal rows are ordered
// by submitted_at, then school_code, then major_group_code, all ascending, so
// repeated queries always return the same order.
func CompareRanking(a, b RankingRow) int {
	if a.ScoreValue != b.ScoreValue {
		if a.ScoreValue > b.ScoreValue {
			return -1
		}
		return 1
	}
	if !a.SubmittedAt.Equal(b.SubmittedAt) {
		if a.SubmittedAt.Before(b.SubmittedAt) {
			return -1
		}
		return 1
	}
	if a.SchoolCode != b.SchoolCode {
		if a.SchoolCode < b.SchoolCode {
			return -1
		}
		return 1
	}
	if a.MajorGroupCode != b.MajorGroupCode {
		if a.MajorGroupCode < b.MajorGroupCode {
			return -1
		}
		return 1
	}
	return 0
}

// SortRankings orders rows in place according to CompareRanking and assigns
// consecutive 1-based ranks.
func SortRankings(rows []RankingRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		return CompareRanking(rows[i], rows[j]) < 0
	})
	for i := range rows {
		rows[i].Rank = i + 1
	}
}
