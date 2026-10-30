package pacelever

import (
	"encoding/json"
	"strings"
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
	// No record: the unmeasurable free bucket is available, even with no
	// observation and with the account weekly window exhausted.
	for name, obs := range map[string][]Observation{"none": nil, "weekly100": {observation("devin", 100)}} {
		in := Inputs{Now: testNow, Observations: obs}
		for _, profile := range []string{"devin-swe2", "devin-swe2-medium", "devin-swe2-max"} {
			d := Gate(in, Request{Profile: profile})
			if d.ExitCode != 0 || d.FreeTag != FreeAvailable || !strings.HasPrefix(d.Reason, FreeAvailable) || strings.Contains(d.Reason, "unmeasur") {
				t.Fatalf("%s %s: %+v", name, profile, d)
			}
		}
	}
}
func TestFreeLimitExceed(t *testing.T) {
	reset := testNow.Add(time.Hour).Format(time.RFC3339)
	in := Inputs{Now: testNow, FreeLimit: &LimitRecord{ResetAt: reset, ObservedAt: testNow.Format(time.RFC3339), Source: "job:fixture"}}
	d := Gate(in, Request{Profile: "devin-swe2"})
	if d.OK || d.ExitCode != Blocked || d.Unmeasurable || d.Reason != "free limit exceeded until "+reset {
		t.Fatalf("active exceed: %+v", d)
	}
	b, _ := json.Marshal(d)
	if !strings.Contains(string(b), "free limit exceeded until") {
		t.Fatalf("json wording: %s", b)
	}
	in.Now = testNow.Add(time.Hour)
	if d := Gate(in, Request{Profile: "devin-swe2"}); d.ExitCode != 0 || d.Reason[:len(FreeAvailable)] != FreeAvailable {
		t.Fatalf("expired exceed: %+v", d)
	}
	b, _ = json.Marshal(Gate(in, Request{Profile: "devin-swe2"}))
	if !strings.Contains(string(b), `"free_tag":"available (free)"`) {
		t.Fatalf("json wording: %s", b)
	}
}
func TestFreeLimitDoesNotAffectPaidDevin(t *testing.T) {
	in := Inputs{Now: testNow, Observations: []Observation{observation("devin", 100)}, FreeLimit: &LimitRecord{ResetAt: testNow.Add(time.Hour).Format(time.RFC3339)}}
	if d := Gate(in, Request{Profile: "devin-swe2"}); d.ExitCode != Blocked {
		t.Fatalf("exceed ignored: %+v", d)
	}
	cat := BundledCatalog()
	cat.Rows = []CatalogRow{{Profile: "devin-paid", Effort: "high", ModelID: "devin-paid-model", Pool: "devin", Grade: "A", Gate: "default", Placed: true}}
	in.Catalog = cat
	in.FreeLimit = nil
	if d := Gate(in, Request{Profile: "devin-paid", Effort: "high"}); d.ExitCode != Blocked || !strings.Contains(d.Reason, "quota exhausted") {
		t.Fatalf("weekly 100%% admitted a non-free rung: %+v", d)
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

func TestCreditRestrictedRungs(t *testing.T) {
	for _, gateName := range []string{"consult_only", "escalation", "unplaced"} {
		for _, role := range []string{"worker", "tester", "builder", ""} {
			t.Run(gateName+"/"+role, func(t *testing.T) {
				c := BundledCatalog()
				g := gateName
				placed := true
				if g == "unplaced" {
					g = "default"
					placed = false
				}
				c.Rows = []CatalogRow{{Profile: "pi-sonnet55", Effort: "high", ModelID: "anthropic/claude-sonnet-5-5", Pool: "anthropic-api", Grade: "A", Gate: g, Placed: placed}}
				r := Request{Profile: "pi-sonnet55", Effort: "high", Role: role, E6Arm: "pi-sonnet55@high"}
				if g == "consult_only" {
					r.OperatorRequest = "hk:task/1"
				}
				_, e := ResolveLaunch(c, Config{}, r)
				want := role == "worker" || role == "tester"
				if (e == nil) != want {
					t.Fatalf("error=%v allowed=%t", e, want)
				}
			})
		}
	}
}
func TestFreeLimitUnreadableResetFailsClosed(t *testing.T) {
	observed := testNow.Add(-10 * time.Minute)
	held := observed.Add(UnknownResetHold).Format(time.RFC3339)
	for _, reset := range []string{"", "not-a-time"} {
		in := Inputs{Now: testNow, FreeLimit: &LimitRecord{ResetAt: reset, ObservedAt: observed.Format(time.RFC3339)}}
		d := Gate(in, Request{Profile: "devin-swe2"})
		if d.OK || d.ExitCode != Blocked || d.Reason != "free limit exceeded until "+held {
			t.Fatalf("reset %q: %+v", reset, d)
		}
		in.Now = observed.Add(UnknownResetHold)
		if d := Gate(in, Request{Profile: "devin-swe2"}); !d.OK {
			t.Fatalf("reset %q: default hold never expired: %+v", reset, d)
		}
	}
	for _, r := range []LimitRecord{{}, {ResetAt: "bad", ObservedAt: "bad"}} {
		d := Gate(Inputs{Now: testNow, FreeLimit: &r}, Request{Profile: "devin-swe2"})
		if d.OK || d.ExitCode != Blocked || !strings.Contains(d.Reason, "free limit record unreadable") {
			t.Fatalf("%+v: %+v", r, d)
		}
	}
}
func TestActiveFreeLimitReadsDamagedRecordsFromDisk(t *testing.T) {
	isolate(t)
	observed := testNow.Add(-5 * time.Minute).Format(time.RFC3339)
	for _, r := range []LimitRecord{{ObservedAt: observed}, {}} {
		s := limitStore{Schema: "scopefuel.devin-free-limit.v1", Records: map[string]LimitRecord{limitIdentity("devin"): r}}
		if e := WriteJSON(limitPath("devin"), s); e != nil {
			t.Fatal(e)
		}
		hold := ActiveFreeLimit(testNow)
		if hold == nil {
			t.Fatalf("damaged record %+v dropped", r)
		}
		if d := Gate(Inputs{Now: testNow, FreeLimit: hold}, Request{Profile: "devin-swe2"}); d.OK {
			t.Fatalf("damaged record %+v admitted", r)
		}
	}
	// A readable default hold that has passed no longer applies.
	s := limitStore{Records: map[string]LimitRecord{limitIdentity("devin"): {ObservedAt: observed}}}
	_ = WriteJSON(limitPath("devin"), s)
	if ActiveFreeLimit(testNow.Add(2*time.Hour)) != nil {
		t.Fatal("expired default hold still active")
	}
}

// Fusion lead billing is judged on the configured billing account: an
// observation from any other login fails closed instead of admitting.
func TestFusionBillingAccountFailsClosed(t *testing.T) {
	c := BundledCatalog()
	c.Rows = []CatalogRow{{Profile: "devin-fusion-sol61", Effort: "high", ModelID: "fusion-model", Pool: "devin", Grade: "A", Gate: "default", Placed: true}}
	obs := observation("devin", 10)
	obs.AccountFP = "devin:0123456789abcdef"
	in := Inputs{Catalog: c, Now: testNow, Observations: []Observation{obs}, Config: Config{Pools: map[string]PoolPolicy{"devin": {}}}}
	for name, fp := range map[string]string{"unset": "", "other-account": "devin:fedcba9876543210"} {
		in.Config.Pools["devin"] = PoolPolicy{FusionBillingFP: fp}
		d := Gate(in, Request{Profile: "devin-fusion-sol61"})
		if d.ExitCode != Unmeasurable || d.Reason != "fusion_billing_account_unavailable" {
			t.Fatalf("%s: %+v", name, d)
		}
	}
	in.Config.Pools["devin"] = PoolPolicy{FusionBillingFP: obs.AccountFP}
	if d := Gate(in, Request{Profile: "devin-fusion-sol61"}); d.Reason == "fusion_billing_account_unavailable" {
		t.Fatalf("matching billing account refused: %+v", d)
	}
}
