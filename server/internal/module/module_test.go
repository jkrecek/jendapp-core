package module

import (
	"path/filepath"
	"testing"
)

func TestMarathonRouting(t *testing.T) {
	m, err := Load(filepath.Join("..", "..", "..", "modules", "marathon"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"2026-10-23-151848-Outdoor Running-Honza’s Apple Watch U4.csv": "runs",
		"2026-10-01-103649-Outdoor Cycling-Honza’s Apple Watch U4.csv": "cycling",
		"2026-09-08-105701-Indoor Running-Honza’s Apple Watch.csv":     "exercise",
		"2026-08-08-123909-Hiking-Honza’s Apple Watch.csv":             "exercise",
		"Fitness-2026-10-23-162135.csv":                                ".",
		"strong_workouts.csv":                                          ".",
	}
	for name, want := range cases {
		r, ok := m.MatchRule(name)
		if !ok || r.Dest != want {
			t.Errorf("%s: got %q (matched %v), want %q", name, r.Dest, ok, want)
		}
	}
	if _, ok := m.MatchRule("invoice.pdf"); ok {
		t.Error("invoice.pdf should not match")
	}
}
