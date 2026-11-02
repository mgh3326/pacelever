package pacelever

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestUnratedSeedContainsOnlyPlaceholders(t *testing.T) {
	data, err := os.ReadFile("testdata/catalog_unrated_hk2639.json")
	if err != nil {
		t.Fatal(err)
	}
	var seed struct {
		Catalog []struct {
			CatalogRow
			BenchmarkSource *string `json:"benchmark_source"`
		}
	}
	if err := json.Unmarshal(data, &seed); err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{
		"codex-sol@low":    "hk:doc/briefs/b953/tier-coverage",
		"codex-sol@medium": "hk:task/1786 (director-1 r682045b95x push list)",
		"codex-sol@high":   "hk:task/1786 (director-1 r682045b95x push list)",
		"omp-sol@low":      "hk:task/2476", "omp-sol@medium": "hk:task/2476", "omp-sol@high": "hk:task/2476",
		"omp-terra61@low": "hk:task/2476", "omp-terra61@medium": "hk:task/2476", "omp-terra61@high": "hk:task/2476", "omp-terra61@xhigh": "hk:task/2476", "omp-terra61@max": "hk:task/2476",
		"omp-sonnet@low": "hk:task/2476", "omp-sonnet@medium": "hk:task/2476",
	}
	if len(seed.Catalog) != len(refs) {
		t.Fatalf("seed rows=%d want=%d", len(seed.Catalog), len(refs))
	}
	for _, row := range seed.Catalog {
		key := row.Profile + "@" + row.Effort
		ref, ok := refs[key]
		if !ok || row.Grade != "unrated" || row.Score != nil || row.BenchmarkSource != nil || row.DeviationRef != ref || row.RetiredAt != nil {
			t.Fatalf("unexpected placeholder retag: %+v", row)
		}
		delete(refs, key)
	}
}

func TestUnratedCatalogRoundTrip(t *testing.T) {
	row := explorerRow("example", "high", "example-model", "claude", "unrated")
	row.DecidedAt = testNow.Format(time.RFC3339)
	row.DecidedBy, row.DeviationRef = "operator", "hk:task/2639"
	rows, err := decodeCatalog(CatalogWire([]CatalogRow{row}))
	if err != nil || len(rows) != 1 || rows[0].Grade != "unrated" {
		t.Fatalf("decode unrated: rows=%+v err=%v", rows, err)
	}
	called := false
	b := fakeBench(t, func(r *http.Request) (*http.Response, error) {
		called = true
		var body struct{ Catalog []CatalogRow }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Catalog) != 1 || body.Catalog[0].Grade != "unrated" {
			t.Fatalf("push unrated: body=%+v err=%v", body, err)
		}
		return response(200, `{"upserted":1}`), nil
	})
	if err := b.PushCatalog(context.Background(), []CatalogRow{row}); err != nil || !called {
		t.Fatalf("push unrated: called=%t err=%v", called, err)
	}
	called = false
	row.Grade = "unknown"
	if err := b.PushCatalog(context.Background(), []CatalogRow{row}); err == nil || called {
		t.Fatalf("invalid grade reached backend: called=%t err=%v", called, err)
	}
}

func TestMonotonicityIgnoresUnrated(t *testing.T) {
	for _, tt := range []struct {
		name   string
		grades []string
		bad    bool
	}{
		{"first", []string{"unrated", "B", "A"}, false},
		{"middle", []string{"B", "unrated", "A"}, false},
		{"last", []string{"B", "A", "unrated"}, false},
		{"all", []string{"unrated", "unrated", "unrated"}, false},
		{"rated violation across unrated", []string{"A", "unrated", "B"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var rows []CatalogRow
			for i, grade := range tt.grades {
				rows = append(rows, CatalogRow{Profile: "example", Effort: []string{"low", "medium", "high"}[i], Grade: grade})
			}
			if err := ValidateMonotonic(rows); (err != nil) != tt.bad {
				t.Fatalf("grades=%v err=%v want violation=%t", tt.grades, err, tt.bad)
			}
		})
	}
}

func TestGradedRecommendationsSkipUnrated(t *testing.T) {
	in := explorationInput("claude", []CatalogRow{
		explorerRow("sonnet", "high", "sonnet-model", "claude", "A"),
		explorerRow("haiku", "high", "haiku-model", "claude", "unrated"),
	})
	in.Config = Config{}
	for _, grade := range []string{"S+", "S", "A+", "A", "B", "C", "unrated"} {
		rows := Recommend(in, grade, "worker", "")
		if grade == "A" && len(rows) != 1 {
			t.Fatalf("rated placement disappeared: %+v", rows)
		}
		for _, row := range rows {
			if row.Grade == "unrated" {
				t.Fatalf("grade %s recommended unrated: %+v", grade, row)
			}
		}
	}
}

func TestUnratedLaunchWithoutGrade(t *testing.T) {
	c := Catalog{Rows: []CatalogRow{explorerRow("codex-sol", "high", "gpt-6.1-sol", "codex", "unrated")}}
	c.Rows[0].Placed = false
	for _, role := range []string{"lead", "consult", "builder", "counsel"} {
		d, err := ResolveLaunch(c, Config{}, Request{Profile: "codex-sol", Effort: "high", Role: role})
		if err != nil || d.Grade != "unrated" || d.Effort != "high" {
			t.Fatalf("%s: launch=%+v err=%v", role, d, err)
		}
	}
}

func TestExplorationCanPickUnrated(t *testing.T) {
	for _, target := range []string{"opus", "sonnet"} {
		t.Run(target, func(t *testing.T) {
			effort := "high"
			if target == "opus" {
				effort = "medium"
			}
			in := explorationInput("claude", []CatalogRow{
				explorerRow("opus", "high", "opus-model", "claude", "A"),
				explorerRow(target, effort, target+"-model", "claude", "unrated"),
			})
			in.Catalog.Rows[1].Placed = false
			req := Request{Profile: "opus", Effort: "high", Role: "worker", RequiredGrade: "A", TaskID: "hk:task/2639"}
			d, err := ResolveLaunchWithInputs(in, req)
			if err != nil || d.Profile != target || d.Effort != effort || d.Grade != "unrated" || d.Label.Arm != "downshift" {
				t.Fatalf("unrated exploration: launch=%+v err=%v", d, err)
			}
			offers := RecommendFor(in, RecommendationRequest{Grade: "A", Role: "worker", TaskID: req.TaskID})
			if len(offers) != 2 || offers[0].Grade != "unrated" || offers[0].Label.Arm != "downshift" || offers[1].Label.Arm != "baseline" {
				t.Fatalf("unrated exploration offers: %+v", offers)
			}
			req.RequiredGrade = "unrated"
			d, err = ResolveLaunchWithInputs(in, req)
			if err != nil || d.Label.Arm != "baseline" {
				t.Fatalf("unrated was treated as a task floor: launch=%+v err=%v", d, err)
			}
		})
	}
}

func TestUnratedGradeEvidence(t *testing.T) {
	t.Run("clean evidence rates an unrated row", func(t *testing.T) {
		c := gradeCatalog()
		c.Rows[0].Grade = "unrated"
		p := ProposeGrades(c, gradeReps(), BenchServer, 2, 5, 0.4)
		if len(p.Changes) != 1 || p.Changes[0].From != "unrated" || p.Changes[0].To != "A" {
			t.Fatalf("unrated promotion: %+v", p)
		}
	})
	t.Run("C failures stay rated", func(t *testing.T) {
		c := gradeCatalog()
		c.Rows[0].Grade = "C"
		reps := gradeReps()
		for i := range reps {
			reps[i].Grade, reps[i].Completed = "C", ptr(0)
			reps[i].BlockersFound = 1
		}
		if p := ProposeGrades(c, reps, BenchServer, 2, 5, 0.4); len(p.Changes) != 0 {
			t.Fatalf("C was demoted into unmeasured: %+v", p.Changes)
		}
	})
	t.Run("unrated tasks cannot grade a model", func(t *testing.T) {
		reps := gradeReps()
		for i := range reps {
			reps[i].Grade, reps[i].Completed = "unrated", ptr(0)
			reps[i].BlockersFound = 1
		}
		p := ProposeGrades(gradeCatalog(), reps, BenchServer, 2, 5, 0.4)
		if len(p.Changes) != 0 || len(p.Excluded) != len(reps) {
			t.Fatalf("unrated task counted as evidence: %+v", p)
		}
	})
}
