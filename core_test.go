package pacelever

import (
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)

func observation(pool string, used float64) Observation {
	return Observation{ID: pool, Plan: "plus", FetchedAt: float64(testNow.Unix()), Buckets: []Bucket{{Label: "5h", Window: "5h", UsedPct: Pct(used), Scope: Scope{Kind: "account"}, ResetsAt: testNow.Add(3 * time.Hour).Format(time.RFC3339)}, {Label: "7d", Window: "7d", UsedPct: Pct(used), Scope: Scope{Kind: "account"}, ResetsAt: testNow.Add(144 * time.Hour).Format(time.RFC3339)}}}
}
func TestGateCodes(t *testing.T) {
	for _, tt := range []struct {
		name string
		req  Request
		obs  []Observation
		cfg  Config
		code int
	}{
		{"admit", Request{Profile: "sonnet"}, []Observation{observation("claude", 10)}, Config{}, 0},
		{"exhausted", Request{Profile: "sonnet"}, []Observation{observation("claude", 99)}, Config{}, 3},
		{"unmeasured", Request{Profile: "sonnet"}, nil, Config{}, 4},
		{"role", Request{Profile: "codex-astra", Role: "worker"}, nil, Config{}, 5},
		{"unsubscribed", Request{Profile: "sonnet"}, nil, Config{Pools: map[string]PoolPolicy{"claude": {Subscribed: ptr(false)}}}, 6},
		{"profile override", Request{Profile: "sonnet"}, []Observation{observation("claude", 10)}, Config{Pools: map[string]PoolPolicy{"claude": {Subscribed: ptr(false)}}, Profiles: map[string]ProfilePolicy{"sonnet": {Subscribed: ptr(true)}}}, 0},
		{"preserve 90", Request{Profile: "codex-luna"}, []Observation{observation("codex", 90)}, Config{Pools: map[string]PoolPolicy{"codex": {Class: "preserve"}}}, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Gate(Inputs{Now: testNow, Config: tt.cfg, Observations: tt.obs}, tt.req)
			if got.ExitCode != tt.code {
				t.Fatalf("got %d: %s; want %d", got.ExitCode, got.Reason, tt.code)
			}
		})
	}
}
func TestCreditSeats(t *testing.T) {
	for _, profile := range []string{"pi-sonnet55", "cmd-dsflash41"} {
		for _, role := range []string{"worker", "tester", "builder", "director", ""} {
			t.Run(profile+"/"+role, func(t *testing.T) {
				_, e := ResolveLaunch(BundledCatalog(), Config{}, Request{Profile: profile, Effort: "high", Role: role})
				want := role == "worker" || role == "tester" || role == "builder"
				if (e == nil) != want {
					t.Fatalf("error=%v allowed=%t", e, want)
				}
			})
		}
	}
}
func TestManualFallback(t *testing.T) {
	m := ManualStore{Latest: map[string]string{}}
	for _, w := range []string{"5h", "7d"} {
		id := "manual-" + w
		m.History = append(m.History, ManualObservation{ID: id, Event: "set", Pool: "claude", Window: w, UsedPct: Pct(10), MeasuredAt: testNow.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: testNow.Add(time.Minute).Format(time.RFC3339)})
		m.Latest["claude:"+w] = id
	}
	in := Inputs{Now: testNow, Manual: m, Observations: []Observation{{ID: "claude", Error: "probe failed"}}}
	d := Gate(in, Request{Profile: "sonnet"})
	if d.ExitCode != 0 || d.Source == nil || *d.Source != "operator" {
		t.Fatalf("manual failed: %+v", d)
	}
	in.Observations[0].Buckets = observation("claude", 99).Buckets
	d = Gate(in, Request{Profile: "sonnet"})
	if d.ExitCode != 3 {
		t.Fatal("manual masked automatic cutoff")
	}
	in.Observations[0].Buckets = nil
	in.Now = testNow.Add(2 * time.Minute)
	if Gate(in, Request{Profile: "sonnet"}).ExitCode != 4 {
		t.Fatal("expired observation admitted")
	}
}
func TestFreeScope(t *testing.T) {
	o := observation("devin", 100)
	o.Buckets = append(o.Buckets, Bucket{Label: "swe-2-high", Scope: Scope{Kind: "model", Name: "swe-2-high"}, UsedPct: Pct(0)})
	in := Inputs{Now: testNow, Observations: []Observation{o}}
	if d := Gate(in, Request{Profile: "devin-swe2"}); d.ExitCode != 0 {
		t.Fatal(d.Reason)
	}
	if d := Gate(in, Request{Profile: "devin-swe2-medium"}); d.ExitCode != 4 {
		t.Fatalf("borrowed free sibling: %d", d.ExitCode)
	}
}
func TestPaceAllPaidPools(t *testing.T) {
	for _, pool := range ProviderIDs {
		t.Run(pool, func(t *testing.T) {
			cat := BundledCatalog()
			cat.Rows = []CatalogRow{{Profile: "primary", Effort: "high", ModelID: "primary-model", Pool: pool, Grade: "A", Gate: "default", Placed: true}, {Profile: "alternative", Effort: "high", ModelID: "alternative-model", Pool: "anthropic-api", Grade: "A", Gate: "default", Placed: true}}
			o := observation(pool, 50)
			alternative := observation("anthropic-api", 1)
			in := Inputs{Now: testNow, Catalog: cat, Observations: []Observation{o, alternative}}
			rows := Recommend(in, "A", "worker", "")
			if len(rows) != 2 || rows[0].Profile != "alternative" || rows[0].Promotion == "" {
				t.Fatalf("not promoted: %+v", rows)
			}
			if d := Gate(in, Request{Profile: "primary", Effort: "high"}); !d.OK {
				t.Fatalf("pace excluded primary: %s", d.Reason)
			}
		})
	}
}
