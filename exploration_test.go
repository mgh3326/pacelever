package pacelever

import (
	"fmt"
	"testing"
	"time"
)

func explorationInput(pool string, rows []CatalogRow) Inputs {
	c := BundledCatalog()
	c.Rows = rows
	return Inputs{Catalog: c, Now: testNow, Config: Config{Pools: map[string]PoolPolicy{pool: {Exploration: ExplorationPolicy{Fraction: 1, ExperimentID: "experiment-fixture"}}}}, Observations: []Observation{observation(pool, 10)}}
}
func explorerRow(profile, effort, model, pool, grade string) CatalogRow {
	return CatalogRow{Profile: profile, Effort: effort, ModelID: model, Pool: pool, Grade: grade, Gate: "default", Placed: true}
}
func TestExplorationDefaultsOffAndLabels(t *testing.T) {
	c := BundledCatalog()
	d, e := ResolveLaunch(c, Config{}, Request{Profile: "sonnet", Effort: "high", Role: "worker"})
	if e != nil || d.Label.Profile != "sonnet" || d.Label.Effort != "high" || d.Label.Arm != "baseline" || d.Label.ExperimentID != nil {
		t.Fatalf("baseline label %+v %v", d.Label, e)
	}
	in := Inputs{Catalog: c, Now: testNow, Observations: []Observation{observation("claude", 10)}}
	d, e = ResolveLaunchWithInputs(in, Request{Profile: "sonnet", Effort: "high", Role: "worker", RequiredGrade: "A", TaskID: "hk:task/1"})
	if e != nil || d.Label.Arm != "baseline" || d.Exploration != nil {
		t.Fatal("default policy explored")
	}
}
func TestExplorationLowerEffort(t *testing.T) {
	in := explorationInput("claude", []CatalogRow{explorerRow("opus", "high", "opus-model", "claude", "A"), explorerRow("opus", "medium", "opus-model", "claude", "A")})
	req := Request{Profile: "opus", Effort: "high", Role: "worker", RequiredGrade: "A", TaskID: "hk:task/1"}
	d, e := ResolveLaunchWithInputs(in, req)
	if e != nil || d.Profile != "opus" || d.Effort != "medium" || d.Label.Arm != "downshift" || d.Label.ExperimentID == nil || *d.Label.ExperimentID != "experiment-fixture" || !d.Exploration.Eligible {
		t.Fatalf("downshift %+v %v", d, e)
	}
	for i := 0; i < 10; i++ {
		again, e := ResolveLaunchWithInputs(in, req)
		if e != nil || again.Label != d.Label {
			if again.Label.Arm != d.Label.Arm || again.Label.Profile != d.Label.Profile || again.Label.Effort != d.Label.Effort {
				t.Fatal("task selection changed")
			}
		}
	}
}
func TestExplorationModelLadders(t *testing.T) {
	for _, tt := range []struct{ pool, from, to string }{{"claude", "opus", "sonnet"}, {"claude", "sonnet", "haiku"}, {"codex", "codex-sol", "codex-terra61"}, {"codex", "codex-terra61", "codex-luna"}, {"anthropic-api", "pi-opus55", "pi-sonnet55"}, {"anthropic-api", "pi-sonnet55", "pi-haiku55"}} {
		t.Run(tt.pool+"/"+tt.from, func(t *testing.T) {
			in := explorationInput(tt.pool, []CatalogRow{explorerRow(tt.from, "high", tt.from+"-model", tt.pool, "A"), explorerRow(tt.to, "high", tt.to+"-model", tt.pool, "A")})
			if tt.pool == "anthropic-api" {
				in.Observations[0].Buckets = append(in.Observations[0].Buckets, screenBucket("credit", 10, ""))
			}
			d, e := ResolveLaunchWithInputs(in, Request{Profile: tt.from, Effort: "high", Role: "worker", RequiredGrade: "A", TaskID: "hk:task/1"})
			if e != nil || d.Profile != tt.to || d.Label.Arm != "downshift" {
				t.Fatalf("model ladder %+v %v", d, e)
			}
		})
	}
}
func TestExplorationGuards(t *testing.T) {
	for _, name := range []string{"unknown pace", "pace exactly one", "pace over one", "stale", "grade floor", "unplaced target", "unplaced normal", "consult target", "subscription", "cross pool", "no task", "no floor"} {
		t.Run(name, func(t *testing.T) {
			in := explorationInput("claude", []CatalogRow{explorerRow("opus", "high", "opus-model", "claude", "A"), explorerRow("opus", "medium", "opus-model", "claude", "A")})
			req := Request{Profile: "opus", Effort: "high", Role: "worker", RequiredGrade: "A", TaskID: "hk:task/1"}
			switch name {
			case "unknown pace":
				in.Observations[0].Buckets[1].ResetsAt = ""
			case "pace exactly one":
				in.Observations[0].Buckets[1].UsedPct = Pct(50)
				in.Observations[0].Buckets[1].ResetsAt = testNow.Add(84 * time.Hour).Format(time.RFC3339)
			case "pace over one":
				in.Observations[0].Buckets[1].UsedPct = Pct(80)
			case "stale":
				in.Observations[0].Stale = true
			case "grade floor":
				in.Catalog.Rows[1].Grade = "B"
			case "unplaced target":
				in.Catalog.Rows[1].Placed = false
			case "unplaced normal":
				in.Catalog.Rows[0].Placed = false
				req.E6Arm = "opus@high"
			case "consult target":
				in.Catalog.Rows[1].Gate = "consult_only"
			case "subscription":
				in.Config.Profiles = map[string]ProfilePolicy{"opus": {Subscribed: ptr(false)}}
			case "cross pool":
				in.Catalog.Rows[1].Pool = "codex"
			case "no task":
				req.TaskID = ""
			case "no floor":
				req.RequiredGrade = ""
			}
			d, e := ResolveLaunchWithInputs(in, req)
			if name == "subscription" {
				if e == nil {
					t.Fatal("unsubscribed launch resolved")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if d.Label.Arm != "baseline" || d.Label.ExperimentID != nil {
				t.Fatalf("guard failed %+v", d)
			}
		})
	}
}
func TestExplorationDoesNotSkipIntermediate(t *testing.T) {
	in := explorationInput("claude", []CatalogRow{explorerRow("opus", "high", "opus-model", "claude", "A"), explorerRow("opus", "medium", "opus-model", "claude", "B"), explorerRow("opus", "low", "opus-model", "claude", "A")})
	d, e := ResolveLaunchWithInputs(in, Request{Profile: "opus", Effort: "high", RequiredGrade: "A", TaskID: "hk:task/1"})
	if e != nil || d.Label.Arm != "baseline" {
		t.Fatal("skipped the ineligible intermediate effort")
	}
}
func TestExplorationDeterministicFraction(t *testing.T) {
	p := ExplorationPolicy{Fraction: 0.25, ExperimentID: "cohort-fixture"}
	count := 0
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("hk:task/%d", i+1)
		selected := experimentSelected(p, "claude", key)
		if selected != experimentSelected(p, "claude", fmt.Sprintf("hk%d", i+1)) {
			t.Fatal("equivalent task keys changed cohort")
		}
		if selected {
			count++
		}
	}
	if count < 180 || count > 320 {
		t.Fatalf("fraction outside expected deterministic range: %d", count)
	}
}
func TestExplorationFreeAndRecommendOffer(t *testing.T) {
	rows := []CatalogRow{explorerRow("devin-swe2", "", "swe-2", "devin", "A+"), explorerRow("devin-swe2-medium", "", "swe-2-medium", "devin", "A")}
	in := explorationInput("devin", rows)
	in.Observations[0].Buckets = nil
	for _, rung := range []string{"swe-2-high", "swe-2-medium"} {
		in.Observations[0].Buckets = append(in.Observations[0].Buckets, Bucket{Label: rung, Scope: Scope{Kind: "model", Name: rung}, UsedPct: Pct(0)})
	}
	d, e := ResolveLaunchWithInputs(in, Request{Profile: "devin-swe2", RequiredGrade: "A", TaskID: "hk:task/1", Role: "worker"})
	if e != nil || d.Profile != "devin-swe2-medium" || d.Label.Arm != "downshift" {
		t.Fatalf("free headroom %+v %v", d, e)
	}
	in = explorationInput("claude", []CatalogRow{explorerRow("opus", "high", "opus-model", "claude", "A"), explorerRow("sonnet", "high", "sonnet-model", "claude", "A")})
	offers := RecommendFor(in, RecommendationRequest{Grade: "A", Role: "worker", TaskID: "hk:task/1"})
	if len(offers) != 2 || offers[0].Profile != "sonnet" || offers[0].Label.Arm != "downshift" || offers[1].Profile != "opus" || offers[1].Label.Arm != "baseline" {
		t.Fatalf("offer lost baseline: %+v", offers)
	}
}

func TestExplorationTargetSubscriptionAndDisabledPolicy(t *testing.T) {
	in := explorationInput("claude", []CatalogRow{explorerRow("opus", "high", "opus-model", "claude", "A"), explorerRow("sonnet", "high", "sonnet-model", "claude", "A")})
	req := Request{Profile: "opus", Effort: "high", RequiredGrade: "A", TaskID: "hk:task/1", Role: "worker"}
	in.Config.Profiles = map[string]ProfilePolicy{"sonnet": {Subscribed: ptr(false)}}
	d, e := ResolveLaunchWithInputs(in, req)
	if e != nil || d.Label.Arm != "baseline" {
		t.Fatal("explored unsubscribed target")
	}
	p := in.Config.Pools["claude"]
	p.Exploration.Enabled = ptr(false)
	in.Config.Pools["claude"] = p
	d, e = ResolveLaunchWithInputs(in, req)
	if e != nil || d.Exploration != nil || d.Label.ExperimentID != nil {
		t.Fatal("disabled policy explored")
	}
}
