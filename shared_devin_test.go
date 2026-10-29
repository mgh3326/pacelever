package pacelever

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func devinSharedCollector(t *testing.T, f *fakeHK) Collector {
	t.Helper()
	isolate(t)
	t.Setenv("PACELEVER_HOST_ID", "")
	t.Setenv("HANDOFFKEEP_HOST_ID", "")
	fp := writeDevinLogin(t)
	return Collector{CachePath: CachePath(), Shared: f, Now: func() time.Time { return testNow }, Config: Config{Quota: QuotaConfig{HostID: "fixture-host", MeasurableAccounts: []string{"devin:" + fp}, WaitS: .001}}}
}

func collectDevinShared(t *testing.T, c Collector) Observation {
	t.Helper()
	out, err := c.Collect(context.Background(), []string{"devin"})
	if err != nil {
		t.Fatal(err)
	}
	return out[0]
}

func TestDevinSharedPublishesFreeOnlyAndPresence(t *testing.T) {
	for _, failedProbe := range []bool{false, true} {
		t.Run(map[bool]string{false: "no paid provider", true: "paid probe fails"}[failedProbe], func(t *testing.T) {
			f := &fakeHK{presenceDone: make(chan struct{}, 1)}
			c := devinSharedCollector(t, f)
			if failedProbe {
				c.Providers = map[string]Provider{"devin": ProviderFunc(func(context.Context) Observation {
					return Observation{Error: "paid probe unavailable", ErrorKind: "probe_error", Buckets: observation("devin", 77).Buckets}
				})}
			}
			o := collectDevinShared(t, c)
			if o.Stale || o.Degraded || o.Error != "" || o.Source != "handoffkeep" || o.FreeBucket == nil || o.FreeBucket.State != FreeBucketAvailable || len(o.Buckets) != 0 || f.published != 1 {
				t.Fatalf("free-only observation: %+v, publications=%d", o, f.published)
			}
			select {
			case <-f.presenceDone:
			case <-time.After(time.Second):
				t.Fatal("no presence published")
			}
			if len(f.presence) != 1 || f.presence[0].AccountKey != "devin:"+fingerprint("devin-account", "org-fixture") {
				t.Fatalf("presence: %+v", f.presence)
			}
			data, err := json.Marshal(f.latest.Observation)
			if err != nil || !strings.Contains(string(data), `"free_bucket":{"state":"available"}`) || !strings.Contains(string(data), `"buckets":[]`) {
				t.Fatalf("wire: %s, %v", data, err)
			}
			for _, forbidden := range []string{"used_pct", "fixture-token", "org-fixture", "fixture-host", "job:"} {
				if strings.Contains(string(data), forbidden) {
					t.Fatalf("free-state wire contains %q", forbidden)
				}
			}
			// The free-only shared result is readable and cached as a success.
			var cache map[string]CacheEntry
			if err := ReadJSON(c.CachePath, &cache); err != nil || cache["devin"].LastError != nil || cache["devin"].Result.FreeBucket == nil {
				t.Fatalf("cache: %+v, %v", cache, err)
			}
			reader := c
			reader.Config.Quota.MeasurableAccounts = nil
			reader.Providers = map[string]Provider{"devin": ProviderFunc(func(context.Context) Observation { t.Fatal("fresh free state triggered probe"); return Observation{} })}
			if got := collectDevinShared(t, reader); got.Stale || got.FreeBucket.State != FreeBucketAvailable || f.published != 1 {
				t.Fatalf("shared reader: %+v", got)
			}
		})
	}
}

func TestDevinSharedKeepsPaidBucketsAndDropsFreePercentages(t *testing.T) {
	f := &fakeHK{}
	c := devinSharedCollector(t, f)
	c.Config.Pools = map[string]PoolPolicy{"devin": {OrgID: "org-fixture"}}
	c.Providers = map[string]Provider{"devin": ProviderFunc(func(context.Context) Observation {
		o := observation("devin", 42)
		o.Source = "usage-api"
		o.PublishFP, o.PublishFPKind = fingerprint("devin-account", "org-fixture"), "account"
		o.Buckets = append(o.Buckets, ParseDevinModels("swe-2-high Free\nswe-2-medium Free\nswe-2-max Free")...)
		return o
	})}
	o := collectDevinShared(t, c)
	if o.Stale || o.FreeBucket.State != FreeBucketAvailable || o.Plan != "plus" || len(o.Buckets) != 2 || len(f.latest.Observation.Buckets) != 2 {
		t.Fatalf("paid and free observation: %+v", o)
	}
	for _, b := range f.latest.Observation.Buckets {
		if b.UsedPct != 42 || b.Scope != "account" || b.ResetsAt == nil || b.Window == "" {
			t.Fatalf("paid bucket changed: %+v", b)
		}
	}
}

func TestDevinSharedRejectsDifferentWebOrganization(t *testing.T) {
	for _, tc := range []struct {
		name, configuredOrg, providerOrg string
	}{
		{"provider selected another org", "org-fixture", "org-other-fixture"},
		{"request config conflicts with provider", "org-other-fixture", "org-fixture"},
		{"provider identity absent", "org-fixture", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHK{}
			c := devinSharedCollector(t, f)
			c.Config.Pools = map[string]PoolPolicy{"devin": {OrgID: tc.configuredOrg}}
			provider := Provider(ProviderFunc(func(context.Context) Observation {
				o := observation("devin", 42)
				o.Source = "usage-api"
				return o
			}))
			if tc.providerOrg != "" {
				t.Setenv("DEVIN_BEARER_TOKEN", "fixture-token")
				provider = devinWebProvider(Config{Pools: map[string]PoolPolicy{"devin": {OrgID: tc.providerOrg}}}, doFunc(func(*http.Request) (*http.Response, error) {
					return response(200, `{"weekly_percentage":42}`), nil
				}), fixedNow)
			}
			c.Providers = map[string]Provider{"devin": provider}
			if got := collectDevinShared(t, c); got.ErrorKind != "account_mismatch" || f.published != 0 || f.holder != "" {
				t.Fatalf("unattested or different web organization published under local key: %+v", got)
			}
		})
	}
}

func TestDevinSharedExceedOverridesFreshAvailableAndPreservesGate(t *testing.T) {
	f := &fakeHK{}
	c := devinSharedCollector(t, f)
	// Capture the legacy local identity before the first shared snapshot.
	before := limitIdentity("devin")
	collectDevinShared(t, c)
	if after := limitIdentity("devin"); after != before {
		t.Fatal("shared organization key moved local free-limit identity")
	}
	reset := testNow.Add(time.Minute)
	r := LimitRecord{ResetAt: reset.Format(time.RFC3339), ObservedAt: testNow.Format(time.RFC3339), Source: "job:fixture", Rung: "devin-swe2"}
	if err := ObserveLimit("devin", r, false, "", testNow); err != nil {
		t.Fatal(err)
	}
	o := collectDevinShared(t, c)
	if o.Stale || o.FreeBucket.State != FreeBucketExhausted || !o.FreeBucket.ResetsAt.Equal(reset) || f.published != 2 {
		t.Fatalf("exceed not published over fresh available: %+v, publications=%d", o, f.published)
	}
	for _, profile := range []string{"devin-swe2", "devin-swe2-medium", "devin-swe2-max"} {
		d := Gate(Inputs{Now: testNow, Observations: []Observation{o}, FreeLimit: ActiveFreeLimit(testNow)}, Request{Profile: profile})
		if d.ExitCode != Blocked || d.Reason != "free limit exceeded until "+r.ResetAt {
			t.Fatalf("gate changed for %s: %+v", profile, d)
		}
	}
	// Readers need only the shared state, never the local audit record.
	reader := c
	reader.Config.Quota.MeasurableAccounts = nil
	if got := collectDevinShared(t, reader); got.FreeBucket.State != FreeBucketExhausted || !got.FreeBucket.ResetsAt.Equal(reset) {
		t.Fatalf("reader lost shared hold: %+v", got)
	}
	c.Now = func() time.Time { return reset }
	if got := collectDevinShared(t, c); got.Stale || got.FreeBucket.State != FreeBucketAvailable || got.FreeBucket.ResetsAt != nil {
		t.Fatalf("reset boundary: %+v", got)
	}
	if d := Gate(Inputs{Now: reset, FreeLimit: ActiveFreeLimit(reset)}, Request{Profile: "devin-swe2"}); d.ExitCode != Admitted || d.FreeTag != FreeAvailable {
		t.Fatalf("gate after reset: %+v", d)
	}
}

func TestDevinSharedPreservesOtherHostsExceedOnRefresh(t *testing.T) {
	reset := testNow.Add(time.Hour)
	f := &fakeHK{latest: SharedLatest{Observation: &SharedObservation{Kind: "auto", MeasuredAt: testNow.Add(-10 * time.Minute), FreeBucket: &FreeBucketState{State: FreeBucketExhausted, ResetsAt: &reset}}}}
	c := devinSharedCollector(t, f)
	if ActiveFreeLimit(testNow) != nil {
		t.Fatal("fixture unexpectedly has a local limit")
	}
	o := collectDevinShared(t, c)
	if o.Stale || o.FreeBucket.State != FreeBucketExhausted || !o.FreeBucket.ResetsAt.Equal(reset) || f.published != 1 {
		t.Fatalf("other host cleared active shared hold: %+v", o)
	}
}

func TestDevinSharedDamagedLocalRecordCannotReplaceActiveSharedExceed(t *testing.T) {
	for _, age := range []time.Duration{time.Minute, 10 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			reset := testNow.Add(time.Hour)
			f := &fakeHK{latest: SharedLatest{Observation: &SharedObservation{Kind: "auto", MeasuredAt: testNow.Add(-age), FreeBucket: &FreeBucketState{State: FreeBucketExhausted, ResetsAt: &reset}}}}
			c := devinSharedCollector(t, f)
			if err := WriteJSON(limitPath("devin"), limitStore{Records: map[string]LimitRecord{limitIdentity("devin"): {}}}); err != nil {
				t.Fatal(err)
			}
			o := collectDevinShared(t, c)
			if o.Stale || o.FreeBucket.State != FreeBucketExhausted || !o.FreeBucket.ResetsAt.Equal(reset) {
				t.Fatalf("damaged local record replaced known shared exceed: %+v", o)
			}
			wantPublishes := 0
			if age > c.Config.Quota.freshTTL("devin") {
				wantPublishes = 1
			}
			if f.published != wantPublishes || f.latest.Observation.FreeBucket.State != FreeBucketExhausted {
				t.Fatalf("publications=%d, want %d; shared=%+v", f.published, wantPublishes, f.latest.Observation)
			}
		})
	}
}

func TestDevinSharedFirstPublishPreservesExistingLimit(t *testing.T) {
	for _, accountScoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "host-local", true: "existing account cache"}[accountScoped], func(t *testing.T) {
			f := &fakeHK{}
			c := devinSharedCollector(t, f)
			if accountScoped {
				o := observation("devin", 42)
				o.AccountFP, o.AccountFPKind = fingerprint("devin-account", "org-fixture"), "account"
				if err := updateSnapshot(c.CachePath, "devin", o, testNow, CacheEntry{}); err != nil {
					t.Fatal(err)
				}
			}
			identity := limitIdentity("devin")
			r := LimitRecord{ResetAt: testNow.Add(time.Hour).Format(time.RFC3339), ObservedAt: testNow.Format(time.RFC3339), Source: "job:fixture"}
			if err := ObserveLimit("devin", r, false, "", testNow); err != nil {
				t.Fatal(err)
			}
			if got := collectDevinShared(t, c); got.FreeBucket.State != FreeBucketExhausted || limitIdentity("devin") != identity || ActiveFreeLimit(testNow) == nil {
				t.Fatalf("first publish lost local limit: %+v", got)
			}
			if err := ObserveLimit("devin", LimitRecord{}, true, "", testNow); err != nil {
				t.Fatal(err)
			}
			if d := Gate(Inputs{Now: testNow, FreeLimit: ActiveFreeLimit(testNow)}, Request{Profile: "devin-swe2"}); d.ExitCode != Admitted {
				t.Fatalf("clear no longer admits locally: %+v", d)
			}
		})
	}
}

func TestDevinSharedFreePublishHonorsPaidProbeBackoff(t *testing.T) {
	f := &fakeHK{}
	c := devinSharedCollector(t, f)
	probes := 0
	c.Providers = map[string]Provider{"devin": ProviderFunc(func(context.Context) Observation {
		probes++
		return Observation{Error: "paid rate limited", ErrorKind: "rate_limited", RetryAfterS: 3600}
	})}
	for _, elapsed := range []time.Duration{0, time.Minute, 10 * time.Minute, 20 * time.Minute} {
		at := testNow.Add(elapsed)
		c.Now = func() time.Time { return at }
		if got := collectDevinShared(t, c); got.Stale || got.FreeBucket.State != FreeBucketAvailable {
			t.Fatalf("free state during paid backoff: %+v", got)
		}
	}
	if probes != 1 || f.published != 3 {
		t.Fatalf("paid probes=%d, free publications=%d", probes, f.published)
	}
	c.Now = func() time.Time { return testNow.Add(time.Hour) }
	collectDevinShared(t, c)
	if probes != 2 {
		t.Fatal("paid probe did not resume after Retry-After")
	}
}

func TestDevinSharedDamagedLimitState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit LimitRecord
		state string
	}{
		{"fallback reset", LimitRecord{ObservedAt: testNow.Format(time.RFC3339)}, FreeBucketExhausted},
		{"unreadable times", LimitRecord{}, FreeBucketUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHK{}
			c := devinSharedCollector(t, f)
			if err := WriteJSON(limitPath("devin"), limitStore{Records: map[string]LimitRecord{limitIdentity("devin"): tc.limit}}); err != nil {
				t.Fatal(err)
			}
			o := collectDevinShared(t, c)
			if o.Stale || o.FreeBucket.State != tc.state {
				t.Fatalf("damaged record: %+v", o)
			}
			if tc.state == FreeBucketExhausted && !o.FreeBucket.ResetsAt.Equal(testNow.Add(UnknownResetHold)) {
				t.Fatalf("fallback hold: %+v", o.FreeBucket)
			}
			if d := Gate(Inputs{Now: testNow, FreeLimit: ActiveFreeLimit(testNow)}, Request{Profile: "devin-swe2"}); d.ExitCode != Blocked {
				t.Fatalf("damaged limit no longer blocks gate: %+v", d)
			}
		})
	}
}

func TestDevinSharedRejectRetainsExistingFailurePath(t *testing.T) {
	f := &fakeHK{fail: "publish"}
	c := devinSharedCollector(t, f)
	o := collectDevinShared(t, c)
	if !o.Stale || !o.Degraded || o.ErrorKind != "shared_publish_failed" || o.FreeBucket != nil || len(f.presence) != 0 || f.holder != "" {
		t.Fatalf("rejected schema did not use existing failure path: %+v", o)
	}
	if d := Gate(Inputs{Now: testNow, Observations: []Observation{o}}, Request{Profile: "devin-swe2"}); d.ExitCode != Admitted || d.FreeTag != FreeAvailable {
		t.Fatalf("hk rejection changed local gate: %+v", d)
	}
}

func TestDevinSharedOutageKeepsFreeSnapshotStaleWithAge(t *testing.T) {
	f := &fakeHK{}
	c := devinSharedCollector(t, f)
	collectDevinShared(t, c)
	f.fail = "read"
	c.Now = func() time.Time { return testNow.Add(10 * time.Minute) }
	if got := collectDevinShared(t, c); !got.Stale || !got.Degraded || got.FreeBucket == nil || got.AgeS == nil || *got.AgeS != 600 {
		t.Fatalf("free-only fallback lost age or degraded status: %+v", got)
	}
}

func TestDevinSharedHTTPWireAndLegacyRejection(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		reject      bool
	}{
		{"available", FreeBucketAvailable, false},
		{"exhausted", FreeBucketExhausted, false},
		{"legacy schema", FreeBucketAvailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := devinSharedCollector(t, &fakeHK{})
			if tc.state == FreeBucketExhausted {
				r := LimitRecord{ResetAt: testNow.Add(time.Hour).Format(time.RFC3339), ObservedAt: testNow.Format(time.RFC3339), Source: "job:fixture"}
				if err := ObserveLimit("devin", r, false, "", testNow); err != nil {
					t.Fatal(err)
				}
			}
			published := make(chan SharedObservation, 1)
			presence := make(chan PresenceRecord, 1)
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET":
					_ = json.NewEncoder(w).Encode(SharedLatest{})
				case r.Method == "POST":
					var lease struct {
						Holder string `json:"holder"`
					}
					if err := json.NewDecoder(r.Body).Decode(&lease); err != nil {
						t.Error(err)
					}
					_ = json.NewEncoder(w).Encode(SharedLease{Holder: lease.Holder, Acquired: true})
				case r.Method == "PUT" && r.URL.Path == presencePath():
					var p PresenceRecord
					if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
						t.Error(err)
					}
					presence <- p
					w.WriteHeader(http.StatusNoContent)
				case r.Method == "PUT":
					var input struct {
						Holder      string            `json:"holder"`
						Observation SharedObservation `json:"observation"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if input.Holder == "" || r.URL.Path != quotaPath("devin:"+fingerprint("devin-account", "org-fixture"), "observation") {
						t.Error("wrong account or missing lease holder")
					}
					published <- input.Observation
					if tc.reject {
						http.Error(w, "invalid_quota_request", http.StatusBadRequest)
					} else {
						w.WriteHeader(http.StatusCreated)
					}
				case r.Method == "DELETE":
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Error("unexpected quota request")
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer h.Close()
			c.Shared = HKObservationSource{Client: BenchClient{URL: h.URL, Token: "fixture-token", Config: Config{Bench: BenchConfig{AllowPlaintextQuotaShare: true}}, Client: h.Client()}}
			o := collectDevinShared(t, c)
			x := <-published
			if x.Kind != "auto" || x.FreeBucket == nil || x.FreeBucket.State != tc.state || len(x.Buckets) != 0 {
				t.Fatalf("HTTP wire: %+v", x)
			}
			if tc.reject {
				if !o.Stale || !o.Degraded || o.ErrorKind != "shared_publish_failed" {
					t.Fatalf("HTTP rejection: %+v", o)
				}
				return
			}
			if o.Stale || o.FreeBucket.State != tc.state {
				t.Fatalf("HTTP publication: %+v", o)
			}
			select {
			case p := <-presence:
				if p.AccountKey != "devin:"+fingerprint("devin-account", "org-fixture") {
					t.Fatal("wrong presence account")
				}
			case <-time.After(time.Second):
				t.Fatal("HTTP presence missing")
			}
		})
	}
}

func TestDevinSharedFreeStateValidation(t *testing.T) {
	reset := testNow.Add(time.Hour)
	for _, tc := range []struct {
		name, pool string
		free       *FreeBucketState
		valid      bool
	}{
		{"available", "devin", &FreeBucketState{State: FreeBucketAvailable}, true},
		{"exhausted", "devin", &FreeBucketState{State: FreeBucketExhausted, ResetsAt: &reset}, true},
		{"unknown", "devin", &FreeBucketState{State: FreeBucketUnknown}, true},
		{"missing free state", "devin", nil, false},
		{"missing reset", "devin", &FreeBucketState{State: FreeBucketExhausted}, false},
		{"unexpected reset", "devin", &FreeBucketState{State: FreeBucketAvailable, ResetsAt: &reset}, false},
		{"invalid state", "devin", &FreeBucketState{State: "unrecognized"}, false},
		{"wrong pool", "grok", &FreeBucketState{State: FreeBucketAvailable}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := &SharedObservation{Kind: "auto", MeasuredAt: testNow, FreeBucket: tc.free}
			if _, ok := sharedAuto(x, tc.pool, tc.pool+":0123456789abcdef", testNow, time.Minute, false); ok != tc.valid {
				t.Fatalf("valid=%v, want %v", ok, tc.valid)
			}
		})
	}
}
