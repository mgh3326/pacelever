package pacelever

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var accountKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}:[0-9a-f]{16}$`)
var accountFPRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (q QuotaConfig) Validate() error {
	if math.IsNaN(q.FreshTTLS) || math.IsInf(q.FreshTTLS, 0) || math.IsNaN(q.WaitS) || math.IsInf(q.WaitS, 0) || q.FreshTTLS < 0 || q.FreshTTLS > 86400 || q.LeaseTTLS < 0 || q.LeaseTTLS > 120 || q.WaitS < 0 || q.WaitS > 10 {
		return fmt.Errorf("invalid shared quota TTLs")
	}
	for pool, fp := range q.Accounts {
		if !knownPool(pool) || !accountFPRE.MatchString(fp) {
			return fmt.Errorf("invalid shared quota account fingerprint")
		}
	}
	for _, key := range q.MeasurableAccounts {
		if !accountKeyRE.MatchString(key) {
			return fmt.Errorf("invalid measurable account key")
		}
	}
	return nil
}

// Wire types intentionally contain only allowlisted measurement fields.
type SharedBucket struct {
	Window   string     `json:"window"`
	UsedPct  float64    `json:"used_pct"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	Scope    string     `json:"scope"`
	Model    string     `json:"model,omitempty"`
}
type SharedObservation struct {
	ID         int64          `json:"id"`
	Kind       string         `json:"kind"`
	Window     string         `json:"window,omitempty"`
	MeasuredAt time.Time      `json:"measured_at"`
	ExpiresAt  *time.Time     `json:"expires_at,omitempty"`
	Plan       string         `json:"plan,omitempty"`
	Buckets    []SharedBucket `json:"buckets"`
}
type SharedLatest struct {
	Observation *SharedObservation  `json:"observation"`
	Manual      []SharedObservation `json:"manual"`
}
type SharedLease struct {
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expires_at"`
	Acquired  bool      `json:"acquired"`
}
type ObservationSource interface {
	Latest(context.Context, string) (SharedLatest, error)
	Lease(context.Context, string, string, int) (SharedLease, error)
	Publish(context.Context, string, string, SharedObservation) error
	Release(context.Context, string, string) error
}
type HKObservationSource struct{ Client BenchClient }

func quotaPath(key, suffix string) string { return "/v1/quota/accounts/" + key + "/" + suffix }
func (s HKObservationSource) Latest(ctx context.Context, key string) (SharedLatest, error) {
	var out SharedLatest
	err := s.Client.Send(ctx, "GET", quotaPath(key, "latest"), "quota_share", nil, &out)
	return out, err
}
func (s HKObservationSource) Lease(ctx context.Context, key, holder string, ttl int) (SharedLease, error) {
	var out SharedLease
	err := s.Client.Send(ctx, "POST", quotaPath(key, "lease"), "quota_share", map[string]any{"holder": holder, "ttl_s": ttl}, &out)
	return out, err
}
func (s HKObservationSource) Publish(ctx context.Context, key, holder string, x SharedObservation) error {
	return s.Client.Send(ctx, "PUT", quotaPath(key, "observation"), "quota_share", map[string]any{"holder": holder, "observation": x}, nil)
}
func (s HKObservationSource) Release(ctx context.Context, key, holder string) error {
	return s.Client.Send(ctx, "DELETE", quotaPath(key, "lease"), "quota_share", map[string]any{"holder": holder}, nil)
}
func (c Collector) sharedSource() ObservationSource {
	if c.Shared != nil {
		return c.Shared
	}
	if c.Config.Quota.URL == "" {
		return nil
	}
	b := NewBenchClient(c.Config)
	b.URL = c.Config.Quota.URL
	name := c.Config.Quota.CredentialEnv
	if name == "" {
		name = "HANDOFFKEEP_TOKEN"
	}
	b.Token = os.Getenv(name)
	b.Client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return HKObservationSource{Client: b}
}

// Resolve identity before any provider request, using exactly the provider hash.
func localAccountFP(pool string, selectors map[string]string) string {
	home, _ := os.UserHomeDir()
	switch pool {
	case "codex":
		dir := selectors["CODEX_HOME"]
		if dir == "" {
			dir = os.Getenv("CODEX_HOME")
		}
		if dir == "" {
			dir = filepath.Join(home, ".codex")
		}
		return fingerprint("codex-account", str(object(readObject(filepath.Join(dir, "auth.json"))["tokens"])["account_id"]))
	case "claude":
		_, identity, _, shareable := claudeLocations(home, selectors)
		if !shareable {
			return ""
		}
		return fingerprint("claude-account", str(object(readObject(identity)["oauthAccount"])["accountUuid"]))
	}
	return ""
}
func (c Collector) accountKey(pool string) (string, bool) {
	local := localAccountFP(pool, c.Selectors)
	fp := c.Config.Quota.Accounts[pool]
	if fp == "" {
		fp = local
	}
	if !accountFPRE.MatchString(fp) {
		return "", false
	}
	key := pool + ":" + fp
	allowed := false
	for _, v := range c.Config.Quota.MeasurableAccounts {
		if v == key {
			allowed = true
		}
	}
	if (pool == "claude" || pool == "codex") && local != fp {
		allowed = false
	}
	return key, allowed
}
func holderToken() (string, error) {
	var b [16]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func (q QuotaConfig) freshTTL() time.Duration {
	if q.FreshTTLS > 0 {
		return time.Duration(q.FreshTTLS * float64(time.Second))
	}
	return 5 * time.Minute
}
func (q QuotaConfig) leaseTTL() int {
	if q.LeaseTTLS > 0 {
		return q.LeaseTTLS
	}
	return 120
}
func (q QuotaConfig) waitTime() time.Duration {
	if q.WaitS > 0 {
		return time.Duration(q.WaitS * float64(time.Second))
	}
	return time.Second
}
func waitQuota(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func releaseQuota(source ObservationSource, key, holder string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return source.Release(ctx, key, holder)
}
func (c Collector) sharedFallback(pool, key, kind string, entry CacheEntry, now time.Time) Observation {
	o := entry.Result
	fp, k := identity(o)
	if key == "" || k != "account" || pool+":"+fp != key {
		o = Observation{}
	}
	o.ID = pool
	o.PoolClass = c.Config.Pool(pool, now).Class
	o.Stale = true
	o.Degraded = true
	o.ErrorKind = kind
	o.Error = "shared quota unavailable"
	if entry.FetchedAt > 0 && len(o.Buckets) > 0 {
		o.FetchedAt = entry.FetchedAt
		o.AgeS = ptr(now.Sub(time.UnixMilli(int64(entry.FetchedAt * 1000))).Seconds())
	}
	return o
}
func sharedAuto(x *SharedObservation, pool, key string, now time.Time, ttl time.Duration) (Observation, bool) {
	if x == nil || x.Kind != "auto" || x.MeasuredAt.IsZero() {
		return Observation{}, false
	}
	age := now.Sub(x.MeasuredAt)
	if age < -time.Minute || age > ttl || len(x.Buckets) == 0 {
		return Observation{}, false
	}
	if age < 0 {
		age = 0
	}
	o := Observation{ID: pool, Plan: x.Plan, Source: "handoffkeep", FetchedAt: float64(x.MeasuredAt.UnixMilli()) / 1000, AgeS: ptr(age.Seconds()), AccountFP: strings.SplitN(key, ":", 2)[1], AccountFPKind: "account", AccountFPMatch: ptr(true)}
	for _, b := range x.Buckets {
		if !validPct(&b.UsedPct) || (b.Scope != "account" && b.Scope != "model") {
			return Observation{}, false
		}
		reset := ""
		if b.ResetsAt != nil {
			if !b.ResetsAt.After(now) {
				return Observation{}, false
			}
			reset = b.ResetsAt.Format(time.RFC3339Nano)
		}
		o.Buckets = append(o.Buckets, Bucket{Label: b.Window, Window: b.Window, UsedPct: ptr(b.UsedPct), ResetsAt: reset, Scope: Scope{Kind: b.Scope, Name: b.Model}, Horizon: horizon(b.Window)})
	}
	return o, true
}
func sharedManual(xs []SharedObservation, pool string) []ManualObservation {
	out := []ManualObservation{}
	for _, x := range xs {
		if x.Kind != "manual" || x.ExpiresAt == nil || len(x.Buckets) != 1 || x.Buckets[0].Scope != "account" || x.Buckets[0].Window != x.Window {
			continue
		}
		b := x.Buckets[0]
		m := ManualObservation{ID: fmt.Sprintf("hk-%d", x.ID), Pool: pool, Event: "set", Window: x.Window, UsedPct: Pct(b.UsedPct), MeasuredAt: x.MeasuredAt.Format(time.RFC3339Nano), ExpiresAt: x.ExpiresAt.Format(time.RFC3339Nano), Source: "operator", SourceVerification: "unverified", SourceLabel: "shared manual observation"}
		if b.ResetsAt != nil {
			m.ResetsAt = b.ResetsAt.Format(time.RFC3339Nano)
		}
		out = append(out, m)
	}
	return out
}
func (c Collector) quotaNow() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}
func (c Collector) sharedOne(ctx context.Context, source ObservationSource, pool string, entry CacheEntry, now time.Time) Observation {
	key, allowed := c.accountKey(pool)
	latest := SharedLatest{}
	reachable := false
	fail := func(kind string) Observation {
		if !reachable {
			return c.sharedFallback(pool, key, kind, entry, now)
		}
		o, _ := sharedAuto(latest.Observation, pool, key, c.quotaNow(), 365*24*time.Hour)
		o.ID = pool
		o.Stale = true
		o.ErrorKind = kind
		o.Error = "shared measurement pending"
		return o
	}
	if key == "" {
		return fail("account_unknown")
	}
	var err error
	latest, err = source.Latest(ctx, key)
	if err != nil {
		reachable = false
		return fail("shared_unavailable")
	}
	reachable = true
	finish := func(o Observation) Observation {
		o.PoolClass = c.Config.Pool(pool, now).Class
		o.SharedManual = sharedManual(latest.Manual, pool)
		return o
	}
	if pool == "anthropic-api" {
		return finish(fail("manual_only"))
	}
	if o, ok := sharedAuto(latest.Observation, pool, key, c.quotaNow(), c.Config.Quota.freshTTL()); ok {
		_ = updateSnapshot(c.cacheFile(), pool, o, time.UnixMilli(int64(o.FetchedAt*1000)), entry)
		return finish(o)
	}
	// Read-only hosts also wait briefly for an in-flight publisher.
	holder := ""
	if allowed {
		holder, err = holderToken()
		if err != nil {
			return fail("shared_unavailable")
		}
		lease, e := source.Lease(ctx, key, holder, c.Config.Quota.leaseTTL())
		if e != nil {
			reachable = false
			return fail("shared_unavailable")
		}
		if !lease.Acquired || lease.Holder != holder {
			holder = ""
		}
	}
	if holder == "" {
		if waitQuota(ctx, c.Config.Quota.waitTime()) != nil {
			return fail("shared_wait")
		}
		latest, err = source.Latest(ctx, key)
		if err != nil {
			reachable = false
			return fail("shared_unavailable")
		}
		if o, ok := sharedAuto(latest.Observation, pool, key, c.quotaNow(), c.Config.Quota.freshTTL()); ok {
			return finish(o)
		}
		return finish(fail("shared_pending"))
	}
	released := false
	defer func() {
		if !released {
			_ = releaseQuota(source, key, holder)
		}
	}()
	// Re-read after acquiring: another holder may have published between GET and POST.
	latest, err = source.Latest(ctx, key)
	if err != nil {
		reachable = false
		return fail("shared_unavailable")
	}
	if o, ok := sharedAuto(latest.Observation, pool, key, c.quotaNow(), c.Config.Quota.freshTTL()); ok {
		return finish(o)
	}
	provider := c.Providers[pool]
	if provider == nil {
		return finish(fail("credentials"))
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(c.Config.Quota.leaseTTL())*time.Second-200*time.Millisecond)
	defer cancel()
	observed := provider.Fetch(probeCtx)
	fp, kind := identity(observed)
	if (fp != "" && (kind != "account" || pool+":"+fp != key)) || (pool == "codex" || pool == "claude") && fp == "" {
		return finish(fail("account_mismatch"))
	}
	if observed.Error != "" || observed.Warning != "" || observed.Stale || len(observed.Buckets) == 0 {
		return finish(fail("probe_failed"))
	}
	at := c.quotaNow()
	x := SharedObservation{Kind: "auto", MeasuredAt: at, Plan: observed.Plan, Buckets: []SharedBucket{}}
	for _, b := range observed.Buckets {
		if !validPct(b.UsedPct) {
			return finish(fail("probe_failed"))
		}
		v := SharedBucket{Window: b.Window, UsedPct: *b.UsedPct, Scope: b.Scope.Kind, Model: b.Scope.Name}
		if b.ResetsAt != "" {
			reset, e := ParseTime(b.ResetsAt)
			if e != nil {
				return finish(fail("probe_failed"))
			}
			v.ResetsAt = &reset
		}
		x.Buckets = append(x.Buckets, v)
	}
	if source.Publish(ctx, key, holder, x) != nil {
		reachable = false
		return finish(fail("shared_publish_failed"))
	}
	o, ok := sharedAuto(&x, pool, key, at, c.Config.Quota.freshTTL())
	if !ok {
		return finish(fail("probe_failed"))
	}
	if releaseQuota(source, key, holder) != nil {
		reachable = false
		return finish(fail("shared_release_failed"))
	}
	released = true
	_ = updateSnapshot(c.cacheFile(), pool, o, at, entry)
	return finish(o)
}

// ManualInputs discards host-local manual data in shared mode. Only observations
// returned by hk participate, including while the automatic probe is unavailable.
func (c Collector) ManualInputs(observations []Observation, local ManualStore) ManualStore {
	if c.sharedSource() == nil {
		return local
	}
	out := ManualStore{Schema: "pacelever.manual.v1", History: []ManualObservation{}, Latest: map[string]string{}}
	for _, o := range observations {
		if o.Degraded {
			continue
		}
		for _, m := range o.SharedManual {
			out.History = append(out.History, m)
			out.Latest[m.Pool+":"+m.Window] = m.ID
		}
	}
	return out
}

// Manual writes use the same lease and table as automatic readings. Free-form
// local audit notes are deliberately omitted from the shared measurement wire.
func (c Collector) AddManual(ctx context.Context, m ManualObservation, ttl time.Duration, now time.Time) (ManualObservation, error) {
	source := c.sharedSource()
	if source == nil {
		return AddManual(m, ttl, now)
	}
	m, err := prepareManual(m, ttl, now)
	if err != nil {
		return m, err
	}
	key, _ := c.accountKey(m.Pool)
	if key == "" {
		return m, fmt.Errorf("shared account fingerprint required")
	}
	expiry, _ := ParseTime(m.ExpiresAt)
	at, _ := ParseTime(m.MeasuredAt)
	b := SharedBucket{Window: m.Window, UsedPct: *m.UsedPct, Scope: "account"}
	if m.ResetsAt != "" {
		reset, _ := ParseTime(m.ResetsAt)
		b.ResetsAt = &reset
	}
	x := SharedObservation{Kind: "manual", Window: m.Window, MeasuredAt: at, ExpiresAt: &expiry, Buckets: []SharedBucket{b}}
	err = c.withQuotaLease(ctx, source, key, func(holder string) error { return source.Publish(ctx, key, holder, x) })
	return m, err
}
func (c Collector) withQuotaLease(ctx context.Context, source ObservationSource, key string, fn func(string) error) error {
	holder, err := holderToken()
	if err != nil {
		return fmt.Errorf("shared lease unavailable")
	}
	lease, err := source.Lease(ctx, key, holder, c.Config.Quota.leaseTTL())
	if err != nil {
		return fmt.Errorf("shared lease unavailable")
	}
	if !lease.Acquired {
		if waitQuota(ctx, c.Config.Quota.waitTime()) != nil {
			return fmt.Errorf("shared lease wait cancelled")
		}
		lease, err = source.Lease(ctx, key, holder, c.Config.Quota.leaseTTL())
	}
	if err != nil || !lease.Acquired || lease.Holder != holder {
		return fmt.Errorf("shared account busy; retry manual entry")
	}
	defer releaseQuota(source, key, holder)
	if fn(holder) != nil {
		return fmt.Errorf("shared manual write failed")
	}
	return nil
}
func (c Collector) ReadManual(ctx context.Context) (ManualStore, error) {
	source := c.sharedSource()
	if source == nil {
		return LoadManual()
	}
	out := ManualStore{Schema: "pacelever.manual.v1", History: []ManualObservation{}, Latest: map[string]string{}}
	for _, pool := range append(append([]string{}, ProviderIDs...), "anthropic-api") {
		key, _ := c.accountKey(pool)
		if key == "" {
			continue
		}
		latest, err := source.Latest(ctx, key)
		if err != nil {
			return out, fmt.Errorf("shared manual observations unavailable")
		}
		for _, m := range sharedManual(latest.Manual, pool) {
			out.History = append(out.History, m)
			out.Latest[m.Pool+":"+m.Window] = m.ID
		}
	}
	return out, nil
}
func (c Collector) ClearManual(ctx context.Context, pool string) error {
	source := c.sharedSource()
	if source == nil {
		return ClearManual(pool)
	}
	key, _ := c.accountKey(pool)
	if key == "" {
		return fmt.Errorf("shared account fingerprint required")
	}
	return c.withQuotaLease(ctx, source, key, func(holder string) error {
		latest, err := source.Latest(ctx, key)
		if err != nil {
			return err
		}
		now := c.quotaNow()
		for _, x := range latest.Manual {
			if x.MeasuredAt.After(now) {
				now = x.MeasuredAt
			}
			x.ID = 0
			x.MeasuredAt = now
			x.ExpiresAt = &now
			x.Buckets = []SharedBucket{}
			if err = source.Publish(ctx, key, holder, x); err != nil {
				return err
			}
		}
		return nil
	})
}
