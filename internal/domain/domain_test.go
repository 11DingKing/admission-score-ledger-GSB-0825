package domain

import (
	"testing"
	"time"
)

func TestFormatScore(t *testing.T) {
	cases := []struct {
		scale ScoreScale
		value int64
		want  string
	}{
		{ScaleInteger, 6100, "610"},
		{ScaleInteger, 0, "0"},
		{ScaleInteger, 5900, "590"},
		{ScaleDecimal1, 6044, "604.4"},
		{ScaleDecimal1, 6102, "610.2"},
		{ScaleDecimal1, 6098, "609.8"},
		{ScaleDecimal1, 5, "0.5"},
		{ScaleDecimal1, 0, "0.0"},
	}
	for _, c := range cases {
		if got := FormatScore(c.scale, c.value); got != c.want {
			t.Errorf("FormatScore(%s, %d) = %q, want %q", c.scale, c.value, got, c.want)
		}
	}
}

func TestPayloadHashStableAndSensitive(t *testing.T) {
	at := time.Date(2025, 8, 25, 9, 0, 0, 0, time.UTC)
	base := Submission{
		SubmissionID:   "sub-1",
		ProvinceCode:   "44",
		AdmissionYear:  2025,
		BatchCode:      "B1",
		SchoolCode:     "szpu",
		MajorGroupCode: "digital-media",
		ScoreScale:     ScaleInteger,
		ScoreValue:     6100,
		SubmittedAt:    at,
		RuleVersion:    "rv1",
		SourceRevision: 1,
	}
	h1 := PayloadHash(base)
	h2 := PayloadHash(base)
	if h1 != h2 {
		t.Fatalf("PayloadHash not stable: %q vs %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("PayloadHash = %q, want 64 hex chars", h1)
	}

	changed := base
	changed.ScoreValue = 6098
	if PayloadHash(changed) == h1 {
		t.Fatal("PayloadHash must change when score_value changes")
	}

	// Timezone representation must not change the canonical hash.
	otherTZ := base
	otherTZ.SubmittedAt = at.In(time.FixedZone("CST", 8*3600))
	if PayloadHash(otherTZ) != h1 {
		t.Fatal("PayloadHash must be identical for the same instant in different time zones")
	}
}

func TestCompareRankingScoreDesc(t *testing.T) {
	higher := RankingRow{ScoreValue: 6102}
	lower := RankingRow{ScoreValue: 6044}
	if CompareRanking(higher, lower) >= 0 {
		t.Fatal("higher score must sort before lower score")
	}
	if CompareRanking(lower, higher) <= 0 {
		t.Fatal("lower score must sort after higher score")
	}
}

func TestSortRankingsTieBreak(t *testing.T) {
	t1 := time.Date(2025, 8, 25, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 8, 25, 10, 0, 0, 0, time.UTC)
	rows := []RankingRow{
		// Tie on score 6100: later submitted_at, should go later.
		{SchoolCode: "b-school", MajorGroupCode: "g2", ScoreValue: 6100, SubmittedAt: t2},
		// Tie on score AND submitted_at with the next row: school_code decides.
		{SchoolCode: "z-school", MajorGroupCode: "g1", ScoreValue: 6000, SubmittedAt: t1},
		{SchoolCode: "a-school", MajorGroupCode: "g9", ScoreValue: 6000, SubmittedAt: t1},
		// Highest score overall.
		{SchoolCode: "c-school", MajorGroupCode: "g3", ScoreValue: 6200, SubmittedAt: t1},
		// Tie on score+time+school with a-school: major_group_code decides.
		{SchoolCode: "a-school", MajorGroupCode: "g0", ScoreValue: 6000, SubmittedAt: t1},
		{SchoolCode: "b-school", MajorGroupCode: "g1", ScoreValue: 6100, SubmittedAt: t1},
	}
	SortRankings(rows)

	wantOrder := []struct {
		school, group string
		score         int64
	}{
		{"c-school", "g3", 6200},
		{"b-school", "g1", 6100}, // same score as next, earlier submitted_at
		{"b-school", "g2", 6100},
		{"a-school", "g0", 6000}, // same score+time: school a before z; group g0 before g9
		{"a-school", "g9", 6000},
		{"z-school", "g1", 6000},
	}
	for i, w := range wantOrder {
		if rows[i].Rank != i+1 {
			t.Fatalf("position %d has rank %d, want %d", i, rows[i].Rank, i+1)
		}
		if rows[i].SchoolCode != w.school || rows[i].MajorGroupCode != w.group || rows[i].ScoreValue != w.score {
			t.Fatalf("position %d = (%s,%s,%d), want (%s,%s,%d)",
				i, rows[i].SchoolCode, rows[i].MajorGroupCode, rows[i].ScoreValue,
				w.school, w.group, w.score)
		}
	}
}

func TestNaturalKeyString(t *testing.T) {
	k := NaturalKey{ProvinceCode: "44", AdmissionYear: 2025, BatchCode: "B1", SchoolCode: "szpu", MajorGroupCode: "digital-media"}
	if s := k.String(); s != "44|2025|B1|szpu|digital-media" {
		t.Fatalf("NaturalKey.String() = %q", s)
	}
}
