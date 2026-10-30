package pacelever

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sharedTestKey = "grok:0123456789abcdef"

type fakeHK struct {
	mu        sync.Mutex
	latest    SharedLatest
	holder    string
	expires   time.Time
	calls     []string
	fail      string
	published int
	presence  []PresenceRecord
	onRead    func(int) *SharedObservation
	reads     int
	// presenceBlock holds the async publish open; presenceDone signals it ran.
	presenceBlock chan struct{}
	presenceDone  chan struct{}
}

func (f *fakeHK) PublishPresence(ctx context.Context, r PresenceRecord) error {
	if f.presenceBlock != nil {
		<-f.presenceBlock
	}
	f.mu.Lock()
	f.presence = append(f.presence, r)
	done := f.presenceDone
	f.mu.Unlock()
	if done != nil {
		done <- struct{}{}
	}
	return nil
}

func (f *fakeHK) Latest(ctx context.Context, key string) (SharedLatest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "read")
	f.reads++
	if f.fail == "read" || f.fail == "reread" && f.reads > 1 {
		return SharedLatest{}, errors.New("offline")
	}
	if f.onRead != nil {
		if o := f.onRead(f.reads); o != nil {
			f.latest.Observation = o
		}
	}
	return f.latest, nil
}
func (f *fakeHK) Lease(ctx context.Context, key, holder string, ttl int) (SharedLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "lease")
	if f.fail == "lease" {
		return SharedLease{}, errors.New("offline")
	}
	acquired := f.holder == "" || !f.expires.After(time.Now()) || f.holder == holder
	if acquired {
		f.holder = holder
		f.expires = time.Now().Add(time.Duration(ttl) * time.Second)
	}
	return SharedLease{Holder: f.holder, ExpiresAt: f.expires, Acquired: acquired}, nil
}
func (f *fakeHK) Publish(ctx context.Context, key, holder string, x SharedObservation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "publish")
	if f.fail == "publish" || f.holder != holder || !f.expires.After(time.Now()) {
		return errors.New("refused")
	}
	f.published++
	x.ID = int64(f.published)
	if x.Kind == "auto" {
		f.latest.Observation = &x
	} else {
		for i, prior := range f.latest.Manual {
			if prior.Window == x.Window {
				f.latest.Manual = append(f.latest.Manual[:i], f.latest.Manual[i+1:]...)
				break
			}
		}
		f.latest.Manual = append(f.latest.Manual, x)
	}
	return nil
}
func (f *fakeHK) Release(ctx context.Context, key, holder string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "release")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if f.fail == "release" {
		return errors.New("offline")
	}
	if f.holder == holder {
		f.holder = ""
	}
	return nil
}
func sampleShared(at time.Time) *SharedObservation {
	return &SharedObservation{Kind: "auto", MeasuredAt: at, Buckets: []SharedBucket{{Window: "7d", UsedPct: 24, Scope: "account"}}}
}
func sharedCollector(t *testing.T, f *fakeHK) Collector {
	t.Helper()
	return Collector{CachePath: filepath.Join(t.TempDir(), "cache.json"), Shared: f, Config: Config{Quota: QuotaConfig{Accounts: map[string]string{"grok": "0123456789abcdef"}, MeasurableAccounts: []string{sharedTestKey}, WaitS: .06}}, Providers: map[string]Provider{"grok": ProviderFunc(func(context.Context) Observation {
		return Observation{Buckets: []Bucket{{Window: "7d", UsedPct: Pct(24), Scope: Scope{Kind: "account"}}}}
	})}}
}
func collectShared(t *testing.T, c Collector) Observation {
	t.Helper()
	out, err := c.Collect(context.Background(), []string{"grok"})
	if err != nil {
		t.Fatal(err)
	}
	return out[0]
}
func TestSharedQuotaFreshBeforeProbe(t *testing.T) {
	f := &fakeHK{latest: SharedLatest{Observation: sampleShared(time.Now().Add(-time.Minute))}}
	c := sharedCollector(t, f)
	c.NoCache = true
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation { t.Fatal("fresh hk triggered probe"); return Observation{} })
	o := collectShared(t, c)
	if o.Stale || o.Degraded || o.Source != "handoffkeep" || strings.Join(f.calls, ",") != "read" {
		t.Fatalf("%+v %v", o, f.calls)
	}
	// hk failure cannot turn a just-written local copy into fresh data.
	f.fail = "read"
	o = collectShared(t, c)
	if !o.Stale || !o.Degraded || len(o.Buckets) != 1 {
		t.Fatalf("fallback %+v", o)
	}
	c.Config.Quota.Accounts["grok"] = "fedcba9876543210"
	o = collectShared(t, c)
	if len(o.Buckets) != 0 {
		t.Fatal("wrong account cache reused")
	}
}
func TestSharedQuotaFleetSingleProbe(t *testing.T) {
	clearHostEnv(t)
	f := &fakeHK{presenceDone: make(chan struct{}, 1)}
	var probes atomic.Int32
	var wg sync.WaitGroup
	out := make(chan Observation, 6)
	hosts := []string{"node-a", "node-b", "node-c", "node-d", "node-e", "node-f"}
	for i := 0; i < 6; i++ {
		c := sharedCollector(t, f)
		c.Config.Quota.NodeEnv = writeNodeEnv(t, hosts[i])
		c.Providers["grok"] = ProviderFunc(func(context.Context) Observation {
			probes.Add(1)
			time.Sleep(15 * time.Millisecond)
			return Observation{Buckets: []Bucket{{Window: "7d", UsedPct: Pct(24), Scope: Scope{Kind: "account"}}}}
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			obs, e := c.Collect(context.Background(), []string{"grok"})
			if e != nil {
				t.Error(e)
				return
			}
			out <- obs[0]
		}()
	}
	wg.Wait()
	close(out)
	for o := range out {
		if o.Stale || o.Degraded {
			t.Fatalf("fleet %+v", o)
		}
	}
	if probes.Load() != 1 || f.published != 1 || f.holder != "" {
		t.Fatalf("probes=%d published=%d holder=%s", probes.Load(), f.published, f.holder)
	}
	// Every logged-in host records presence: one prober plus five shared readers.
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.presence)
		f.mu.Unlock()
		if n == 6 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.presence) != 6 {
		t.Fatalf("presence %+v", f.presence)
	}
	seen := map[string]bool{}
	for _, p := range f.presence {
		if p.AccountKey != sharedTestKey || p.HostID == "" || p.LastSeen.IsZero() {
			t.Fatalf("presence %+v", f.presence)
		}
		seen[p.HostID] = true
	}
	if len(seen) != 6 {
		t.Fatalf("expected 6 distinct host IDs: %+v", f.presence)
	}
}

// A logged-in host that reads a fresh shared observation still publishes
// presence, so placement sees hosts that never probe.
func TestPresencePublishedOnSharedRead(t *testing.T) {
	f := &fakeHK{latest: SharedLatest{Observation: sampleShared(time.Now())}, presenceDone: make(chan struct{}, 1)}
	c := sharedCollector(t, f)
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation { t.Fatal("fresh hk triggered probe"); return Observation{} })
	o := collectShared(t, c)
	if o.Stale || o.Degraded {
		t.Fatalf("%+v", o)
	}
	select {
	case <-f.presenceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shared read did not publish presence")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.presence) != 1 || f.presence[0].AccountKey != sharedTestKey {
		t.Fatalf("presence %+v", f.presence)
	}
}

// A host whose local identity does not resolve to the measured key never
// publishes presence for it, even when it reads fresh shared data.
func TestPresenceSkippedWhenNotLoggedIn(t *testing.T) {
	f := &fakeHK{latest: SharedLatest{Observation: sampleShared(time.Now())}, presenceDone: make(chan struct{}, 1)}
	c := sharedCollector(t, f)
	c.Config.Quota.MeasurableAccounts = nil
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation { t.Fatal("read-only host probed"); return Observation{} })
	o := collectShared(t, c)
	if o.Stale || o.Degraded {
		t.Fatalf("%+v", o)
	}
	select {
	case <-f.presenceDone:
		t.Fatal("read-only host published presence")
	case <-time.After(200 * time.Millisecond):
	}
}

// Presence publishes at most once per interval per host+account, and the
// record survives across CLI runs via the cache dir.
func TestPresenceRateLimitAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	f := &fakeHK{onRead: func(int) *SharedObservation { return sampleShared(now) }, presenceDone: make(chan struct{}, 2)}
	run := func() Observation {
		c := sharedCollector(t, f)
		c.CachePath = filepath.Join(dir, "cache.json")
		c.Now = func() time.Time { return now }
		c.Providers["grok"] = ProviderFunc(func(context.Context) Observation { t.Fatal("fresh hk triggered probe"); return Observation{} })
		return collectShared(t, c)
	}
	if o := run(); o.Stale || o.Degraded {
		t.Fatalf("%+v", o)
	}
	select {
	case <-f.presenceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not publish presence")
	}
	if o := run(); o.Stale || o.Degraded {
		t.Fatalf("%+v", o)
	}
	select {
	case <-f.presenceDone:
		t.Fatal("second run inside the interval published again")
	case <-time.After(200 * time.Millisecond):
	}
	now = now.Add(11 * time.Minute)
	if o := run(); o.Stale || o.Degraded {
		t.Fatalf("%+v", o)
	}
	select {
	case <-f.presenceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("run past the interval did not publish presence")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.presence) != 2 {
		t.Fatalf("presence %+v", f.presence)
	}
}

func TestPresencePublishDoesNotBlockCollection(t *testing.T) {
	f := &fakeHK{presenceBlock: make(chan struct{}), presenceDone: make(chan struct{}, 1)}
	c := sharedCollector(t, f)
	done := make(chan Observation, 1)
	go func() {
		obs, err := c.Collect(context.Background(), []string{"grok"})
		if err != nil {
			done <- Observation{Error: err.Error()}
			return
		}
		done <- obs[0]
	}()
	select {
	case o := <-done:
		if o.Error != "" {
			close(f.presenceBlock)
			t.Fatal(o.Error)
		}
	case <-time.After(500 * time.Millisecond):
		close(f.presenceBlock)
		t.Fatal("collection waited for the presence publish")
	}
	close(f.presenceBlock)
	select {
	case <-f.presenceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("presence never ran")
	}
}
func TestSharedQuotaRereadAndReadOnly(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		f := &fakeHK{onRead: func(n int) *SharedObservation {
			if n == 2 {
				return sampleShared(time.Now())
			}
			return nil
		}}
		c := sharedCollector(t, f)
		if readOnly {
			c.Config.Quota.MeasurableAccounts = nil
		}
		c.Providers["grok"] = ProviderFunc(func(context.Context) Observation { t.Fatal("reread missed fresh result"); return Observation{} })
		o := collectShared(t, c)
		if o.Stale || f.published != 0 {
			t.Fatalf("%+v", o)
		}
		if readOnly && strings.Contains(strings.Join(f.calls, ","), "lease") {
			t.Fatal("read-only host acquired lease")
		}
	}
}
func TestSharedQuotaFailuresReleaseAndDegrade(t *testing.T) {
	for _, stage := range []string{"read", "reread", "lease", "publish", "release"} {
		t.Run(stage, func(t *testing.T) {
			f := &fakeHK{fail: stage}
			c := sharedCollector(t, f)
			o := collectShared(t, c)
			if !o.Stale || !o.Degraded {
				t.Fatalf("%+v", o)
			}
			if (stage == "reread" || stage == "publish") && f.holder != "" {
				t.Fatal("lease leaked")
			}
		})
	}
	f := &fakeHK{}
	c := sharedCollector(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation { cancel(); return Observation{Error: "cancelled"} })
	_, _ = c.Collect(ctx, []string{"grok"})
	if f.holder != "" {
		t.Fatal("caller cancellation leaked lease")
	}
}

// A 429 recorded in shared mode suppresses the next collector run before the
// lease. With no usable shared observation, the same-account local last-good
// snapshot is served stale and expired resets roll over.
func TestSharedQuotaRateLimitBackoffUsesLocalLastGood(t *testing.T) {
	now := testNow
	f := &fakeHK{}
	c := sharedCollector(t, f)
	c.Now = func() time.Time { return now }
	reset := now.Add(-time.Minute).Format(time.RFC3339)
	lastGood := Observation{ID: "grok", AccountFP: "0123456789abcdef", AccountFPKind: "account", Buckets: []Bucket{{Window: "7d", UsedPct: Pct(24), ResetsAt: reset, Scope: Scope{Kind: "account"}}}}
	if e := WriteJSON(c.CachePath, map[string]CacheEntry{"grok": {FetchedAt: float64(now.Add(-time.Hour).Unix()), Result: lastGood}}); e != nil {
		t.Fatal(e)
	}
	var probes atomic.Int32
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation {
		probes.Add(1)
		return Observation{Error: "usage HTTP 429", ErrorKind: "rate_limited", PublishFP: "0123456789abcdef", PublishFPKind: "account"}
	})
	first := collectShared(t, c)
	if !first.Stale || first.Degraded || first.ErrorKind != "rate_limited" || first.Error != "usage HTTP 429" || first.AgeS == nil || *first.AgeS != 3600 {
		t.Fatalf("first fallback %+v", first)
	}
	if len(first.Buckets) != 1 || !first.Buckets[0].RolledOver || first.Buckets[0].UsedPct == nil || *first.Buckets[0].UsedPct != 0 || first.Buckets[0].ResetsAt != "" {
		t.Fatalf("expired local bucket not rolled over: %+v", first.Buckets)
	}
	entries, e := loadSnapshots(c.CachePath)
	if e != nil || entries["grok"].NegativeUntil-entries["grok"].LastErrorAt < 299 || entries["grok"].NegativeUntil-entries["grok"].LastErrorAt > 300 {
		t.Fatalf("default rate-limit backoff not persisted: %+v %v", entries["grok"], e)
	}
	callsAfterFirst := len(f.calls)
	second := collectShared(t, c)
	if probes.Load() != 1 || strings.Join(f.calls[callsAfterFirst:], ",") != "read,read" {
		t.Fatalf("backoff allowed another probe or lease: probes=%d calls=%v", probes.Load(), f.calls)
	}
	if !second.Stale || second.ErrorKind != "rate_limited" || second.Error != "provider backoff active" || second.AgeS == nil || *second.AgeS != 3600 || len(second.Buckets) != 1 {
		t.Fatalf("second fallback %+v", second)
	}
	if second.AccountFPMatch == nil || !*second.AccountFPMatch {
		t.Fatalf("same-account fallback lost identity: %+v", second)
	}
}

func TestSharedQuotaRateLimitHonoursRetryAfter(t *testing.T) {
	now := testNow
	f := &fakeHK{}
	c := sharedCollector(t, f)
	c.Now = func() time.Time { return now }
	var probes atomic.Int32
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation {
		probes.Add(1)
		return Observation{Error: "usage HTTP 429", ErrorKind: "rate_limited", RetryAfterS: 720}
	})
	collectShared(t, c)
	entries, e := loadSnapshots(c.CachePath)
	if e != nil || entries["grok"].NegativeUntil-entries["grok"].LastErrorAt < 719 || entries["grok"].NegativeUntil-entries["grok"].LastErrorAt > 720 {
		t.Fatalf("Retry-After backoff not persisted: %+v %v", entries["grok"], e)
	}
	if entries["grok"].LastError == nil || entries["grok"].LastError.PublishFP != "0123456789abcdef" || entries["grok"].LastError.PublishFPKind != "account" {
		t.Fatalf("rate-limit backoff not bound to account key: %+v", entries["grok"].LastError)
	}
	now = now.Add(6 * time.Minute)
	o := collectShared(t, c)
	if probes.Load() != 1 || o.Error != "provider backoff active" || o.ErrorKind != "rate_limited" {
		t.Fatalf("Retry-After backoff ignored: probes=%d obs=%+v", probes.Load(), o)
	}
}

func TestSharedQuotaRateLimitBackoffIsAccountScoped(t *testing.T) {
	now := testNow
	f := &fakeHK{}
	c := sharedCollector(t, f)
	c.Now = func() time.Time { return now }
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation {
		return Observation{Error: "usage HTTP 429", ErrorKind: "rate_limited"}
	})
	collectShared(t, c)
	c.Config.Quota.Accounts["grok"] = "fedcba9876543210"
	if o := collectShared(t, c); o.ErrorKind != "shared_pending" {
		t.Fatalf("previous account's backoff suppressed a different key: %+v", o)
	}
}

func TestSharedQuotaBackoffStillRereadsShared(t *testing.T) {
	now := testNow
	f := &fakeHK{onRead: func(n int) *SharedObservation {
		if n == 4 {
			return sampleShared(now)
		}
		return nil
	}}
	c := sharedCollector(t, f)
	c.Now = func() time.Time { return now }
	var probes atomic.Int32
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation {
		probes.Add(1)
		return Observation{Error: "usage HTTP 429", ErrorKind: "rate_limited"}
	})
	collectShared(t, c)
	callsAfterFirst := len(f.calls)
	o := collectShared(t, c)
	if probes.Load() != 1 || o.Stale || o.Source != "handoffkeep" || strings.Join(f.calls[callsAfterFirst:], ",") != "read,read" {
		t.Fatalf("backoff skipped the shared reread: probes=%d calls=%v obs=%+v", probes.Load(), f.calls, o)
	}
}

func TestSharedQuotaOtherFailuresDoNotBackoff(t *testing.T) {
	f := &fakeHK{}
	c := sharedCollector(t, f)
	lastGood := Observation{ID: "grok", AccountFP: "0123456789abcdef", AccountFPKind: "account", Buckets: []Bucket{{Window: "7d", UsedPct: Pct(24), Scope: Scope{Kind: "account"}}}}
	if e := WriteJSON(c.CachePath, map[string]CacheEntry{"grok": {FetchedAt: float64(testNow.Unix()), Result: lastGood}}); e != nil {
		t.Fatal(e)
	}
	var probes atomic.Int32
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation {
		probes.Add(1)
		return Observation{Error: "usage request failed", ErrorKind: "network"}
	})
	for i := 0; i < 2; i++ {
		if o := collectShared(t, c); o.ErrorKind != "network" || len(o.Buckets) != 0 {
			t.Fatalf("run %d: %+v", i, o)
		}
	}
	if probes.Load() != 2 {
		t.Fatalf("non-rate-limit failure was cached as backoff: probes=%d", probes.Load())
	}
}

func TestSharedQuotaManualFleet(t *testing.T) {
	f := &fakeHK{}
	writer := sharedCollector(t, f)
	now := time.Now().UTC()
	m := ManualObservation{Pool: "grok", Window: "7d", UsedPct: Pct(33), MeasuredAt: now.Format(time.RFC3339Nano), Reason: "local audit note"}
	if _, e := writer.AddManual(context.Background(), m, time.Hour, now); e != nil {
		t.Fatal(e)
	}
	reader := sharedCollector(t, f)
	reader.Config.Quota.MeasurableAccounts = nil
	o := collectShared(t, reader)
	manual := reader.ManualInputs([]Observation{o}, ManualStore{})
	if len(manual.Valid("grok", now)) != 1 {
		t.Fatalf("manual %+v obs %+v", manual, o)
	}
	displayed := DisplayObservation(o, reader.Config, manual, now)
	if displayed.Stale || displayed.Source != "operator" {
		t.Fatalf("display %+v", displayed)
	}
	if e := writer.ClearManual(context.Background(), "grok"); e != nil {
		t.Fatal(e)
	}
	o = collectShared(t, reader)
	if len(reader.ManualInputs([]Observation{o}, ManualStore{}).Valid("grok", now)) != 0 {
		t.Fatal("clear did not propagate")
	}
	f.fail = "read"
	o = collectShared(t, reader)
	if !o.Degraded {
		t.Fatal("not degraded")
	}
	if len(reader.ManualInputs([]Observation{o}, manual).Valid("grok", now)) != 0 {
		t.Fatal("host manual masked outage")
	}
}
func TestSharedQuotaIdentityBeforeMeasurement(t *testing.T) {
	dir := t.TempDir()
	account := "opaque-test-account"
	if e := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(fmt.Sprintf(`{"tokens":{"account_id":%q,"access_token":"never-send-this"}}`, account)), 0600); e != nil {
		t.Fatal(e)
	}
	fp := fingerprint("codex-account", account)
	c := Collector{Selectors: map[string]string{"CODEX_HOME": dir}, Config: Config{Quota: QuotaConfig{MeasurableAccounts: []string{"codex:" + fp}}}}
	key, allowed := c.accountKey("codex")
	if key != "codex:"+fp || !allowed {
		t.Fatalf("%s %v", key, allowed)
	}
	// A configured fingerprint that disagrees with the derived login never
	// blocks the local credential's own key.
	c.Config.Quota.Accounts = map[string]string{"codex": "0123456789abcdef"}
	key, allowed = c.accountKey("codex")
	if !allowed || key != "codex:"+fp {
		t.Fatalf("configured mismatch blocked derived key: %s %v", key, allowed)
	}
	if strings.Contains(key, account) {
		t.Fatal("raw identity leaked")
	}
}
func TestSharedQuotaHTTPWire(t *testing.T) {
	var methods []string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Header.Get("Authorization") != "Bearer fake-token" || r.URL.Path != quotaPath(sharedTestKey, map[string]string{"GET": "latest", "POST": "lease", "PUT": "observation", "DELETE": "lease"}[r.Method]) {
			t.Errorf("invalid request")
		}
		var body map[string]any
		if r.Method != "GET" {
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
				t.Error(e)
			}
		}
		switch r.Method {
		case "GET":
			_ = json.NewEncoder(w).Encode(SharedLatest{})
		case "POST":
			_ = json.NewEncoder(w).Encode(SharedLease{Holder: body["holder"].(string), Acquired: true, ExpiresAt: time.Now().Add(time.Minute)})
		case "DELETE":
			w.WriteHeader(204)
		case "PUT":
			w.WriteHeader(201)
		}
	}))
	defer h.Close()
	source := HKObservationSource{Client: BenchClient{URL: h.URL, Token: "fake-token", Config: Config{Bench: BenchConfig{AllowPlaintextQuotaShare: true}}, Client: h.Client()}}
	ctx := context.Background()
	holder, _ := holderToken()
	if _, e := source.Latest(ctx, sharedTestKey); e != nil {
		t.Fatal(e)
	}
	if _, e := source.Lease(ctx, sharedTestKey, holder, 120); e != nil {
		t.Fatal(e)
	}
	if e := source.Publish(ctx, sharedTestKey, holder, *sampleShared(time.Now())); e != nil {
		t.Fatal(e)
	}
	if e := source.Release(ctx, sharedTestKey, holder); e != nil {
		t.Fatal(e)
	}
	if strings.Join(methods, ",") != "GET,POST,PUT,DELETE" {
		t.Fatal(methods)
	}
}

func TestSharedQuotaClockSkew(t *testing.T) {
	now := time.Now().UTC()
	key := sharedTestKey
	if o, ok := sharedAuto(sampleShared(now.Add(20*time.Second)), "grok", key, now, 5*time.Minute, false); !ok || o.Stale || o.AgeS == nil || *o.AgeS != 0 {
		t.Fatal("small clock skew invalidated fresh observation")
	}
	if _, ok := sharedAuto(sampleShared(now.Add(2*time.Minute)), "grok", key, now, 5*time.Minute, false); ok {
		t.Fatal("unbounded future observation accepted")
	}
}

func writeHKConfigEnv(t *testing.T) {
	t.Helper()
	isolate(t)
	path := filepath.Join(configBase(), "handoffkeep", "config.env")
	body := "HANDOFFKEEP_URL=https://stored.example.invalid\nHANDOFFKEEP_TOKEN=stored-placeholder\nHANDOFFKEEP_CF_ACCESS_CLIENT_ID=stored-id\nHANDOFFKEEP_CF_ACCESS_CLIENT_SECRET=stored-secret\n"
	if e := AtomicWrite(path, []byte(body)); e != nil {
		t.Fatal(e)
	}
	t.Setenv("HANDOFFKEEP_CONFIG", path)
	t.Setenv("HANDOFFKEEP_URL", "")
	t.Setenv("HANDOFFKEEP_TOKEN", "")
}
func sharedClient(t *testing.T, c Collector) BenchClient {
	t.Helper()
	source, ok := c.sharedSource().(HKObservationSource)
	if !ok {
		t.Fatal("no handoffkeep source")
	}
	return source.Client
}
func TestSharedSourceKeepsStoredTokenWhenEnvEmpty(t *testing.T) {
	for _, name := range []string{"", "PACELEVER_TEST_QUOTA_TOKEN"} {
		writeHKConfigEnv(t)
		t.Setenv("PACELEVER_TEST_QUOTA_TOKEN", "  ")
		c := Collector{Config: Config{Quota: QuotaConfig{URL: "https://STORED.example.invalid:443/v1", CredentialEnv: name}}}
		b := sharedClient(t, c)
		if b.Token != "stored-placeholder" || b.CFClientID != "stored-id" || b.CFClientSecret != "stored-secret" || b.URL != "https://STORED.example.invalid:443/v1" {
			t.Fatalf("stored credentials lost for credential_env %q", name)
		}
	}
}
func TestSharedSourceEnvTokenWins(t *testing.T) {
	for _, quotaURL := range []string{"https://stored.example.invalid", "https://other.example.invalid"} {
		writeHKConfigEnv(t)
		t.Setenv("PACELEVER_TEST_QUOTA_TOKEN", "env-placeholder")
		c := Collector{Config: Config{Quota: QuotaConfig{URL: quotaURL, CredentialEnv: "PACELEVER_TEST_QUOTA_TOKEN"}}}
		if b := sharedClient(t, c); b.Token != "env-placeholder" {
			t.Fatalf("explicit credential env did not win for %s", quotaURL)
		}
	}
}
func TestSharedSourceWithholdsStoredCredentialsFromOtherOrigin(t *testing.T) {
	for _, quotaURL := range []string{
		"https://other.example.invalid",       // different host
		"https://stored.example.invalid:8443", // different port
		"http://stored.example.invalid",       // plaintext downgrade
		"not a url",                           // unparsable
	} {
		for _, name := range []string{"", "PACELEVER_TEST_QUOTA_TOKEN"} {
			writeHKConfigEnv(t)
			t.Setenv("PACELEVER_TEST_QUOTA_TOKEN", "")
			c := Collector{Config: Config{Quota: QuotaConfig{URL: quotaURL, CredentialEnv: name}}}
			b := sharedClient(t, c)
			if b.Token != "" || b.CFClientID != "" || b.CFClientSecret != "" || b.URL != quotaURL {
				t.Fatalf("stored credentials leaked to %s (credential_env %q)", quotaURL, name)
			}
		}
	}
}
func TestSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"https://h.example.invalid", "https://H.example.invalid:443/x", true},
		{"http://h.example.invalid", "http://h.example.invalid:80", true},
		{"https://h.example.invalid", "http://h.example.invalid", false},
		{"https://h.example.invalid", "https://h.example.invalid:444", false},
		{"", "", false},
		{"https://h.example.invalid", "", false},
	} {
		if got := sameOrigin(tc.a, tc.b); got != tc.want {
			t.Fatalf("sameOrigin(%q,%q)=%v", tc.a, tc.b, got)
		}
	}
}

// writeNodeEnv writes a panewire-style node env file holding id and a token
// placeholder, and returns its path.
func writeNodeEnv(t *testing.T, id string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub-node.env")
	if e := os.WriteFile(path, []byte("HUB_MACHINE_ID="+id+"\nHUB_TOKEN=fixture-token\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	return path
}

// clearHostEnv blanks the explicit host id variables and points HOME at an empty
// directory, so the default node env path never reads the developer's own file.
func clearHostEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PACELEVER_HOST_ID", "")
	t.Setenv("HANDOFFKEEP_HOST_ID", "")
	t.Setenv("HOME", t.TempDir())
}
func TestPresenceHostIDPrecedence(t *testing.T) {
	clearHostEnv(t)
	node := writeNodeEnv(t, "mac-work")
	for _, tc := range []struct {
		name    string
		q       QuotaConfig
		env     map[string]string
		want    string
		wantErr error
	}{
		{name: "node id alone", q: QuotaConfig{NodeEnv: node}, want: "mac-work"},
		{name: "config host_id equal", q: QuotaConfig{NodeEnv: node, HostID: "mac-work"}, want: "mac-work"},
		{name: "env equal", q: QuotaConfig{NodeEnv: node}, env: map[string]string{"PACELEVER_HOST_ID": "mac-work"}, want: "mac-work"},
		{name: "legacy env equal", q: QuotaConfig{NodeEnv: node}, env: map[string]string{"HANDOFFKEEP_HOST_ID": "mac-work"}, want: "mac-work"},
		{name: "all three equal", q: QuotaConfig{NodeEnv: node, HostID: "mac-work"}, env: map[string]string{"PACELEVER_HOST_ID": "mac-work"}, want: "mac-work"},
		{name: "config host_id differs", q: QuotaConfig{NodeEnv: node, HostID: "oci"}, wantErr: errHostMismatch},
		{name: "env differs", q: QuotaConfig{NodeEnv: node}, env: map[string]string{"PACELEVER_HOST_ID": "oci"}, wantErr: errHostMismatch},
		{name: "legacy env differs", q: QuotaConfig{NodeEnv: node}, env: map[string]string{"HANDOFFKEEP_HOST_ID": "oci"}, wantErr: errHostMismatch},
		{name: "env equal but config differs", q: QuotaConfig{NodeEnv: node, HostID: "oci"}, env: map[string]string{"PACELEVER_HOST_ID": "mac-work"}, wantErr: errHostMismatch},
		{name: "config equal but env differs", q: QuotaConfig{NodeEnv: node, HostID: "mac-work"}, env: map[string]string{"PACELEVER_HOST_ID": "oci"}, wantErr: errHostMismatch},
		{name: "explicit id without a node env", q: QuotaConfig{NodeEnv: filepath.Join(t.TempDir(), "absent.env"), HostID: "mac-work"}, wantErr: errNoNodeID},
		{name: "explicit env without a node env", q: QuotaConfig{NodeEnv: filepath.Join(t.TempDir(), "absent.env")}, env: map[string]string{"PACELEVER_HOST_ID": "mac-work"}, wantErr: errNoNodeID},
		{name: "no id anywhere, no hash fallback", q: QuotaConfig{NodeEnv: filepath.Join(t.TempDir(), "absent.env")}, wantErr: errNoNodeID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := tc.q.presenceHostID()
			if err != tc.wantErr || got != tc.want {
				t.Fatalf("got %q, %v; want %q, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
func TestNodeIDParsing(t *testing.T) {
	clearHostEnv(t)
	write := func(body string) QuotaConfig {
		path := filepath.Join(t.TempDir(), "node.env")
		if e := os.WriteFile(path, []byte(body), 0o600); e != nil {
			t.Fatal(e)
		}
		return QuotaConfig{NodeEnv: path}
	}
	for _, tc := range []struct {
		name, body, want string
	}{
		{"plain", "HUB_MACHINE_ID=node-a\nHUB_TOKEN=x\n", "node-a"},
		{"comments and blanks", "# note\n\nHUB_TOKEN=x\nHUB_MACHINE_ID=node-a\n", "node-a"},
		{"export prefix", "export HUB_MACHINE_ID=node-a\n", "node-a"},
		{"quoted", "HUB_MACHINE_ID=\"node-a\"\n", "node-a"},
		{"last assignment wins", "HUB_MACHINE_ID=node-a\nHUB_MACHINE_ID=node-b\n", "node-b"},
		{"spaces around", "  HUB_MACHINE_ID = node-a  \n", "node-a"},
		{"missing key", "HUB_TOKEN=x\n", ""},
		{"empty value", "HUB_MACHINE_ID=\n", ""},
		{"operator is a valid id but never inferred", "HUB_MACHINE_ID=operator\n", "operator"},
		{"uppercase", "HUB_MACHINE_ID=Node-A\n", ""},
		{"underscore", "HUB_MACHINE_ID=node_a\n", ""},
		{"too long", "HUB_MACHINE_ID=" + strings.Repeat("a", 33) + "\n", ""},
		{"max length", "HUB_MACHINE_ID=" + strings.Repeat("a", 32) + "\n", strings.Repeat("a", 32)},
		{"malformed line", "HUB_MACHINE_ID=node-a\nnot a pair\n", ""},
		{"bad quoting", "HUB_MACHINE_ID=\"node-a\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := write(tc.body).nodeID()
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("got %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
	if _, ok := (QuotaConfig{NodeEnv: t.TempDir()}).nodeID(); ok {
		t.Fatal("a directory was read as a node env file")
	}
}
func TestNodeEnvPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, tc := range []struct{ configured, want string }{
		{"", filepath.Join(home, ".config", "panewire", "hub-node.env")},
		{"~/n/hub.env", filepath.Join(home, "n", "hub.env")},
		{"/etc/panewire/node.env", "/etc/panewire/node.env"},
	} {
		if got := (QuotaConfig{NodeEnv: tc.configured}).nodeEnvPath(); got != tc.want {
			t.Fatalf("node_env %q resolved to %q, want %q", tc.configured, got, tc.want)
		}
	}
}
func TestNodeIDFromDefaultPath(t *testing.T) {
	clearHostEnv(t)
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".config", "panewire")
	if e := os.MkdirAll(dir, 0o700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "hub-node.env"), []byte("HUB_MACHINE_ID=default-node\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	if got, err := (QuotaConfig{}).presenceHostID(); err != nil || got != "default-node" {
		t.Fatalf("default path gave %q, %v", got, err)
	}
}
func TestQuotaHostIDFromConfigFile(t *testing.T) {
	isolate(t)
	clearHostEnv(t)
	node := writeNodeEnv(t, "mac-dev")
	path := filepath.Join(configBase(), "pacelever", "config.toml")
	if e := AtomicWrite(path, []byte("[quota]\nhost_id = \"mac-dev\"\nnode_env = \""+node+"\"\n")); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PACELEVER_CONFIG", path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Quota.NodeEnv != node {
		t.Fatalf("node_env %q", cfg.Quota.NodeEnv)
	}
	if got, err := cfg.Quota.presenceHostID(); err != nil || got != "mac-dev" {
		t.Fatalf("host_id %q, %v", got, err)
	}
	if e := (QuotaConfig{HostID: "bad host/name"}).Validate(); e == nil || strings.Contains(e.Error(), "bad host") {
		t.Fatalf("invalid host_id accepted or echoed: %v", e)
	}
	if e := (QuotaConfig{HostID: strings.Repeat("a", 65)}).Validate(); e == nil {
		t.Fatal("overlong host_id accepted")
	}
}

// presenceCollector is a shared collector whose node env holds nodeID ("" for
// no node env at all).
func presenceCollector(t *testing.T, f *fakeHK, nodeID string) Collector {
	t.Helper()
	clearHostEnv(t)
	c := sharedCollector(t, f)
	c.Config.Quota.NodeEnv = filepath.Join(t.TempDir(), "absent.env")
	if nodeID != "" {
		c.Config.Quota.NodeEnv = writeNodeEnv(t, nodeID)
	}
	c.PublishLog = &PublishLog{}
	return c
}
func presenceSeen(f *fakeHK, wait bool) []PresenceRecord {
	if wait {
		select {
		case <-f.presenceDone:
		case <-time.After(2 * time.Second):
		}
	} else {
		time.Sleep(100 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PresenceRecord(nil), f.presence...)
}
func TestPresenceUsesNodeID(t *testing.T) {
	f := &fakeHK{presenceDone: make(chan struct{}, 1)}
	c := presenceCollector(t, f, "desktop")
	collectShared(t, c)
	if got := presenceSeen(f, true); len(got) != 1 || got[0].HostID != "desktop" {
		t.Fatalf("presence %+v", got)
	}
}
func TestPresenceAcceptsEqualExplicitHostID(t *testing.T) {
	f := &fakeHK{presenceDone: make(chan struct{}, 1)}
	c := presenceCollector(t, f, "desktop")
	c.Config.Quota.HostID = "desktop"
	t.Setenv("PACELEVER_HOST_ID", "desktop")
	collectShared(t, c)
	if got := presenceSeen(f, true); len(got) != 1 || got[0].HostID != "desktop" {
		t.Fatalf("presence %+v", got)
	}
	c.PublishLog.Wait()
	if errs := c.PublishLog.Errors(); len(errs) != 0 {
		t.Fatalf("unexpected warnings %v", errs)
	}
}

// A configured id that is not the node's refuses presence, warns once, and
// leaves the shared read and the verdict alone.
func TestPresenceRefusedOnMismatch(t *testing.T) {
	f := &fakeHK{latest: SharedLatest{Observation: sampleShared(time.Now().Add(-time.Minute))}}
	c := presenceCollector(t, f, "desktop")
	c.NoCache = true
	c.Config.Quota.HostID = "laptop"
	o := collectShared(t, c)
	if o.Stale || o.Degraded || o.Source != "handoffkeep" || len(o.Buckets) == 0 {
		t.Fatalf("read changed by a presence refusal: %+v", o)
	}
	if got := presenceSeen(f, false); len(got) != 0 {
		t.Fatalf("published despite mismatch: %+v", got)
	}
	c.PublishLog.Wait()
	errs := c.PublishLog.Errors()
	if len(errs) != 1 || errs[0] != errHostMismatch {
		t.Fatalf("warnings %v", errs)
	}
	if strings.Contains(errs[0].Error(), "laptop") || strings.Contains(errs[0].Error(), "desktop") {
		t.Fatalf("warning echoes an id: %v", errs[0])
	}
}

// With no node env and no explicit id nothing is published, a warning is
// recorded once, and reads still work.
func TestPresenceNotPublishedWithoutNodeID(t *testing.T) {
	f := &fakeHK{latest: SharedLatest{Observation: sampleShared(time.Now().Add(-time.Minute))}}
	c := presenceCollector(t, f, "")
	c.NoCache = true
	o := collectShared(t, c)
	if o.Stale || o.Degraded || o.Source != "handoffkeep" || len(o.Buckets) == 0 {
		t.Fatalf("read changed by missing node id: %+v", o)
	}
	if got := presenceSeen(f, false); len(got) != 0 {
		t.Fatalf("published without a node id: %+v", got)
	}
	c.PublishLog.Wait()
	if errs := c.PublishLog.Errors(); len(errs) != 1 || errs[0] != errNoNodeID {
		t.Fatalf("warnings %v", errs)
	}
	// The explicit id alone is no substitute for the node id.
	c.Config.Quota.HostID = "desktop"
	collectShared(t, c)
	if got := presenceSeen(f, false); len(got) != 0 {
		t.Fatalf("explicit host_id published without a node id: %+v", got)
	}
}
func TestPublishLogAddOnce(t *testing.T) {
	var l *PublishLog
	l.addOnce(errNoNodeID)
	l = &PublishLog{}
	l.addOnce(errNoNodeID)
	l.addOnce(errNoNodeID)
	l.addOnce(errHostMismatch)
	if errs := l.Errors(); len(errs) != 2 {
		t.Fatalf("errors %v", errs)
	}
}

// A stale shared observation keeps windows whose reset is still in the future;
// a window whose reset already passed restarted and is kept as a marked 0%
// rollover instead of dropping the whole last-good reading.
func TestSharedAutoStaleKeepsUnexpiredBuckets(t *testing.T) {
	now := testNow
	past := now.Add(-time.Hour)
	future := now.Add(120 * time.Hour)
	key := "claude:0123456789abcdef"
	x := &SharedObservation{Kind: "auto", MeasuredAt: now.Add(-2 * time.Hour), Buckets: []SharedBucket{
		{Window: "5h", UsedPct: 55, ResetsAt: &past, Scope: "account"},
		{Window: "7d", UsedPct: 8, ResetsAt: &future, Scope: "account"},
	}}
	if _, ok := sharedAuto(x, "claude", key, now, 365*24*time.Hour, false); ok {
		t.Fatal("fresh path accepted an expired reset")
	}
	o, ok := sharedAuto(x, "claude", key, now, 365*24*time.Hour, true)
	if !ok || len(o.Buckets) != 2 {
		t.Fatalf("stale path dropped unexpired buckets: %+v", o)
	}
	rolled := o.Buckets[0]
	if rolled.Window != "5h" || !rolled.RolledOver || rolled.UsedPct == nil || *rolled.UsedPct != 0 || rolled.ResetsAt != "" || rolled.Note == "" {
		t.Fatalf("expired 5h window not marked rolled over: %+v", rolled)
	}
	kept := o.Buckets[1]
	if kept.Window != "7d" || kept.RolledOver || kept.UsedPct == nil || *kept.UsedPct != 8 || kept.ResetsAt == "" {
		t.Fatalf("valid 7d window changed: %+v", kept)
	}
	// With every reset still in the future the stale path is unchanged.
	future5h := now.Add(3 * time.Hour)
	x.Buckets[0].ResetsAt = &future5h
	o, ok = sharedAuto(x, "claude", key, now, 365*24*time.Hour, true)
	if !ok || len(o.Buckets) != 2 || o.Buckets[0].RolledOver || o.Buckets[1].RolledOver || *o.Buckets[0].UsedPct != 55 {
		t.Fatalf("all-valid observation changed: %+v", o)
	}
}

// The shared fail path after a rate-limited probe returns the last good
// reading marked stale, with the expired window rolled over, and the gate
// accepts it for a pool far from its limit.
func TestSharedQuotaStaleKeepsUnexpiredBuckets(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(120 * time.Hour)
	f := &fakeHK{latest: SharedLatest{Observation: &SharedObservation{Kind: "auto", MeasuredAt: now.Add(-2 * time.Hour), Buckets: []SharedBucket{
		{Window: "5h", UsedPct: 55, ResetsAt: &past, Scope: "account"},
		{Window: "7d", UsedPct: 8, ResetsAt: &future, Scope: "account"},
	}}}}
	c := sharedCollector(t, f)
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation {
		return Observation{Error: "usage HTTP 429", ErrorKind: "rate_limited"}
	})
	o := collectShared(t, c)
	if !o.Stale || o.ErrorKind != "rate_limited" || len(o.Buckets) != 2 {
		t.Fatalf("stale observation %+v", o)
	}
	if !o.Buckets[0].RolledOver || *o.Buckets[0].UsedPct != 0 || o.Buckets[0].ResetsAt != "" {
		t.Fatalf("expired 5h window not rolled over: %+v", o.Buckets[0])
	}
	if o.Buckets[1].RolledOver || *o.Buckets[1].UsedPct != 8 || o.Buckets[1].ResetsAt == "" {
		t.Fatalf("valid 7d window lost: %+v", o.Buckets[1])
	}
	d := Gate(Inputs{Now: now, Observations: []Observation{o}}, Request{Profile: "grok-hi"})
	if !d.OK || !d.StaleAccepted {
		t.Fatalf("gate refused a pool far from its limit: %+v", d)
	}
}

// A required window that rolled over satisfies stale acceptance; a window
// missing entirely still does not.
func TestGateStaleRolledOverWindowAccepted(t *testing.T) {
	o := observation("claude", 8)
	o.Buckets[0].UsedPct = Pct(0)
	o.Buckets[0].ResetsAt = ""
	o.Buckets[0].RolledOver = true
	o.Buckets[0].Note = rolloverNote
	o.Stale = true
	o.Error = "usage HTTP 429"
	o.ErrorKind = "rate_limited"
	o.AccountFPMatch = ptr(true)
	o.AgeS = ptr(60.0)
	d := Gate(Inputs{Now: testNow, Observations: []Observation{o}}, Request{Profile: "sonnet"})
	if !d.OK || !d.StaleAccepted {
		t.Fatalf("rolled-over 5h refused: %+v", d)
	}
	o.Buckets = o.Buckets[1:]
	if d = Gate(Inputs{Now: testNow, Observations: []Observation{o}}, Request{Profile: "sonnet"}); d.ExitCode != Unmeasurable {
		t.Fatalf("missing required window accepted: %+v", d)
	}
	// An expired reset without the rollover mark does not satisfy the window:
	// its stale reading must not stand in for a restarted one.
	o = observation("claude", 90)
	o.Buckets[0].ResetsAt = testNow.Add(-time.Hour).Format(time.RFC3339)
	o.Stale = true
	o.Error = "usage HTTP 429"
	o.ErrorKind = "rate_limited"
	o.AccountFPMatch = ptr(true)
	o.AgeS = ptr(60.0)
	if d = Gate(Inputs{Now: testNow, Observations: []Observation{o}}, Request{Profile: "sonnet"}); d.ExitCode != Unmeasurable {
		t.Fatalf("unmarked expired reset satisfied a required window: %+v", d)
	}
}

func TestSharedQuotaSkipsUnmeasuredBucket(t *testing.T) {
	f := &fakeHK{}
	c := sharedCollector(t, f)
	c.NoCache = true
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation {
		return Observation{Buckets: []Bucket{
			{Window: "7d", UsedPct: Pct(24), Scope: Scope{Kind: "account"}},
			{Window: "7d", Scope: Scope{Kind: "model", Name: "Fable"}},
		}}
	})
	o := collectShared(t, c)
	if o.Degraded || len(o.Buckets) != 1 || f.published != 1 || len(f.latest.Observation.Buckets) != 1 {
		t.Fatalf("nil bucket must be skipped, not fail the probe: %+v published=%d", o, f.published)
	}
	bad := 120.0
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation {
		return Observation{Buckets: []Bucket{{Window: "7d", UsedPct: &bad, Scope: Scope{Kind: "account"}}}}
	})
	f.latest = SharedLatest{}
	o = collectShared(t, c)
	if !o.Stale || o.ErrorKind != "probe_failed" {
		t.Fatalf("out-of-range value must still fail: %+v", o)
	}
}

// A contended lease whose publisher never lands is waited out until the
// holder's lease expires, then taken over: a failed or killed measurement
// cannot pin the account on "pending".
func TestSharedQuotaDeadPublisherTakenOver(t *testing.T) {
	f := &fakeHK{holder: "dead-host", expires: time.Now().Add(150 * time.Millisecond)}
	c := sharedCollector(t, f)
	o := collectShared(t, c)
	if o.Stale || o.Degraded || o.Error != "" || len(o.Buckets) != 1 || f.published != 1 || f.holder != "" {
		t.Fatalf("takeover %+v published=%d holder=%q", o, f.published, f.holder)
	}
}

// While the lease is contended the waiter polls until the in-flight
// publication lands instead of reporting pending after one short wait.
func TestSharedQuotaMidWindowPublishServed(t *testing.T) {
	f := &fakeHK{holder: "busy-host", expires: time.Now().Add(2 * time.Second)}
	f.onRead = func(n int) *SharedObservation {
		if n >= 3 {
			return sampleShared(time.Now())
		}
		return nil
	}
	c := sharedCollector(t, f)
	o := collectShared(t, c)
	if o.Stale || o.Degraded || o.Error != "" || len(o.Buckets) != 1 || f.published != 0 {
		t.Fatalf("mid-window %+v published=%d", o, f.published)
	}
}

// leaseDeniedHK never grants the lease but keeps answering reads with the
// current holder's short window, like a publisher that keeps failing. The
// deadline is fixed, so each reported window does close.
type leaseDeniedHK struct {
	inner    *fakeHK
	deadline time.Time
}

func (l leaseDeniedHK) Latest(ctx context.Context, key string) (SharedLatest, error) {
	return l.inner.Latest(ctx, key)
}
func (l leaseDeniedHK) Lease(context.Context, string, string, int) (SharedLease, error) {
	return SharedLease{Holder: "unlucky-host", ExpiresAt: l.deadline, Acquired: false}, nil
}
func (l leaseDeniedHK) Publish(ctx context.Context, key, holder string, x SharedObservation) error {
	return l.inner.Publish(ctx, key, holder, x)
}
func (l leaseDeniedHK) Release(ctx context.Context, key, holder string) error {
	return l.inner.Release(ctx, key, holder)
}

// A measuring host that waits out publisher windows that never produce a
// reading reports the measurement as failed, distinct from an in-flight
// pending lease and from an unreachable store.
func TestSharedQuotaClosedWindowReportsFailed(t *testing.T) {
	f := &fakeHK{}
	c := sharedCollector(t, f)
	c.Shared = leaseDeniedHK{inner: f, deadline: time.Now().Add(50 * time.Millisecond)}
	o := collectShared(t, c)
	if o.ErrorKind != ErrKindSharedFailed || f.published != 0 {
		t.Fatalf("closed window %+v published=%d", o, f.published)
	}
}

// A holder whose probe fails releases the lease before returning, so the
// next caller can take over immediately instead of waiting out the TTL.
func TestSharedQuotaFailedMeasurementReleasesLease(t *testing.T) {
	f := &fakeHK{}
	c := sharedCollector(t, f)
	c.Providers["grok"] = ProviderFunc(func(context.Context) Observation {
		return Observation{Error: "probe blew up", ErrorKind: ErrKindProbe}
	})
	o := collectShared(t, c)
	if o.ErrorKind != ErrKindProbe || f.holder != "" || f.published != 0 {
		t.Fatalf("failed measurement %+v published=%d holder=%q", o, f.published, f.holder)
	}
}

// A holder that cannot publish releases the lease too; a stranded lease is
// worse than a lost observation because every other host waits behind it.
func TestSharedQuotaFailedPublishReleasesLease(t *testing.T) {
	f := &fakeHK{fail: "publish"}
	c := sharedCollector(t, f)
	o := collectShared(t, c)
	if o.ErrorKind != ErrKindSharedPublishFailed || f.holder != "" {
		t.Fatalf("failed publish %+v holder=%q", o, f.holder)
	}
}
