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
	t.Setenv("PACELEVER_HOST_ID", "")
	t.Setenv("HANDOFFKEEP_HOST_ID", "")
	f := &fakeHK{presenceDone: make(chan struct{}, 1)}
	var probes atomic.Int32
	var wg sync.WaitGroup
	out := make(chan Observation, 6)
	hosts := []string{"node-a", "node-b", "node-c", "node-d", "node-e", "node-f"}
	for i := 0; i < 6; i++ {
		c := sharedCollector(t, f)
		c.Config.Quota.HostID = hosts[i]
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
	c.Config.Quota.Accounts = map[string]string{"codex": "0123456789abcdef"}
	key, allowed = c.accountKey("codex")
	if allowed {
		t.Fatal("different login allowed to measure configured account")
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
func TestPresenceHostIDPrecedence(t *testing.T) {
	t.Setenv("PACELEVER_HOST_ID", "")
	t.Setenv("HANDOFFKEEP_HOST_ID", "")
	hashed := QuotaConfig{}.presenceHostID()
	if !strings.HasPrefix(hashed, "host-") {
		t.Fatalf("fallback %q", hashed)
	}
	q := QuotaConfig{HostID: "mac-work"}
	if got := q.presenceHostID(); got != "mac-work" {
		t.Fatalf("config host_id %q", got)
	}
	t.Setenv("HANDOFFKEEP_HOST_ID", "oci")
	if got := q.presenceHostID(); got != "oci" {
		t.Fatalf("env did not override config: %q", got)
	}
	t.Setenv("PACELEVER_HOST_ID", "rpi")
	if got := q.presenceHostID(); got != "rpi" {
		t.Fatalf("PACELEVER_HOST_ID %q", got)
	}
}
func TestQuotaHostIDFromConfigFile(t *testing.T) {
	isolate(t)
	t.Setenv("PACELEVER_HOST_ID", "")
	t.Setenv("HANDOFFKEEP_HOST_ID", "")
	path := filepath.Join(configBase(), "pacelever", "config.toml")
	if e := AtomicWrite(path, []byte("[quota]\nhost_id = \"mac-dev\"\n")); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PACELEVER_CONFIG", path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Quota.presenceHostID(); got != "mac-dev" {
		t.Fatalf("host_id %q", got)
	}
	if e := (QuotaConfig{HostID: "bad host/name"}).Validate(); e == nil || strings.Contains(e.Error(), "bad host") {
		t.Fatalf("invalid host_id accepted or echoed: %v", e)
	}
	if e := (QuotaConfig{HostID: strings.Repeat("a", 65)}).Validate(); e == nil {
		t.Fatal("overlong host_id accepted")
	}
}
func TestPresenceUsesConfiguredHostID(t *testing.T) {
	t.Setenv("PACELEVER_HOST_ID", "")
	t.Setenv("HANDOFFKEEP_HOST_ID", "")
	f := &fakeHK{presenceDone: make(chan struct{}, 1)}
	c := sharedCollector(t, f)
	c.Config.Quota.HostID = "desktop"
	collectShared(t, c)
	select {
	case <-f.presenceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("presence never ran")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.presence) != 1 || f.presence[0].HostID != "desktop" {
		t.Fatalf("presence %+v", f.presence)
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
}
