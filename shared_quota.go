package pacelever

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var accountKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}:[0-9a-f]{16}$`)
var accountFPRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (q QuotaConfig) Validate() error {
	if math.IsNaN(q.FreshTTLS) || math.IsInf(q.FreshTTLS, 0) || math.IsNaN(q.WaitS) || math.IsInf(q.WaitS, 0) || math.IsNaN(q.PresenceEveryS) || math.IsInf(q.PresenceEveryS, 0) || q.FreshTTLS < 0 || q.FreshTTLS > 86400 || q.LeaseTTLS < 0 || q.LeaseTTLS > 120 || q.WaitS < 0 || q.WaitS > 10 || q.PresenceEveryS < 0 || q.PresenceEveryS > 86400 {
		return fmt.Errorf("invalid shared quota TTLs")
	}
	if q.HostID != "" && safeHost(q.HostID) != q.HostID {
		return fmt.Errorf("invalid shared quota host_id")
	}
	for pool, ttl := range q.PoolFreshTTLS {
		if !knownPool(pool) || math.IsNaN(ttl) || math.IsInf(ttl, 0) || ttl <= 0 || ttl > 86400 {
			return fmt.Errorf("invalid shared quota TTLs")
		}
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
	ID         int64            `json:"id"`
	Kind       string           `json:"kind"`
	Window     string           `json:"window,omitempty"`
	MeasuredAt time.Time        `json:"measured_at"`
	ExpiresAt  *time.Time       `json:"expires_at,omitempty"`
	Plan       string           `json:"plan,omitempty"`
	Buckets    []SharedBucket   `json:"buckets"`
	FreeBucket *FreeBucketState `json:"free_bucket,omitempty"`
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

// PresenceRecord is host placement metadata. It is deliberately separate from
// shared quota observations, which carry no host identity.
type PresenceRecord struct {
	HostID     string    `json:"host_id"`
	AccountKey string    `json:"account_key"`
	LastSeen   time.Time `json:"last_seen"`
}
type PresenceSource interface {
	PublishPresence(context.Context, PresenceRecord) error
}

func presencePath() string { return "/v1/quota/presence" }
func (s HKObservationSource) PublishPresence(ctx context.Context, r PresenceRecord) error {
	return s.Client.Send(ctx, "PUT", presencePath(), "quota_share", r, nil)
}

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
	// The stored token and CF Access pair were loaded for b.URL. Reuse them only
	// when the quota endpoint is the same origin; otherwise they must not leave
	// for a different (or mistyped, or plaintext) host.
	if !sameOrigin(b.URL, c.Config.Quota.URL) {
		b.Token, b.CFClientID, b.CFClientSecret = "", "", ""
	}
	b.URL = c.Config.Quota.URL
	name := c.Config.Quota.CredentialEnv
	if name == "" {
		name = "HANDOFFKEEP_TOKEN"
	}
	// An unset or empty variable keeps the token that NewBenchClient loaded
	// from config.env (same-origin only, above); a non-empty one always wins.
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		b.Token = v
	}
	b.Client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return HKObservationSource{Client: b}
}

// sameOrigin reports whether two URLs share scheme, host and port. An empty or
// unparsable URL never matches.
func sameOrigin(a, b string) bool {
	origin := func(raw string) string {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Scheme == "" || u.Hostname() == "" {
			return ""
		}
		scheme, port := strings.ToLower(u.Scheme), u.Port()
		if port == "" {
			port = map[string]string{"https": "443", "http": "80"}[scheme]
		}
		return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
	}
	oa := origin(a)
	return oa != "" && oa == origin(b)
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
	case "devin":
		return devinAccountFP()
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
	if (pool == "claude" || pool == "codex" || pool == "devin") && local != fp {
		allowed = false
	}
	return key, allowed
}

// presenceHostID is stable per host and never the raw hostname when an explicit
// identifier is absent. Precedence: environment, then quota.host_id, then a hash.
func (q QuotaConfig) presenceHostID() string {
	if v := strings.TrimSpace(firstEnv("PACELEVER_HOST_ID", "HANDOFFKEEP_HOST_ID")); v != "" {
		return safeHost(v)
	}
	if v := strings.TrimSpace(q.HostID); v != "" {
		return safeHost(v)
	}
	host, _ := os.Hostname()
	if host == "" {
		return ""
	}
	return "host-" + fingerprint("host", host)
}

// publishPresence records where an account was measured. It is asynchronous and
// bounded, so it never adds latency to a collection or changes its verdict.
// onDone, when set, reports whether the publish succeeded.
func (c Collector) publishPresence(source ObservationSource, key string, at time.Time, onDone func(bool)) {
	publisher, ok := source.(PresenceSource)
	host := c.Config.Quota.presenceHostID()
	if !ok || !accountKeyRE.MatchString(key) || host == "" {
		if onDone != nil {
			onDone(false)
		}
		return
	}
	record := PresenceRecord{HostID: host, AccountKey: key, LastSeen: at}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := publisher.PublishPresence(ctx, record)
		if onDone != nil {
			onDone(err == nil)
		}
	}()
}
func holderToken() (string, error) {
	var b [16]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func (q QuotaConfig) presenceEvery() time.Duration {
	if q.PresenceEveryS > 0 {
		return time.Duration(q.PresenceEveryS * float64(time.Second))
	}
	return 10 * time.Minute
}

// presenceFile persists the last claimed presence publish per host+account so
// repeated CLI runs do not spam hk. Unreadable state counts as due; a state
// that cannot be persisted skips this run's publish rather than risking a
// burst. The lock is nonblocking: collection never waits on a presence slot.
func (c Collector) presenceFile() string { return filepath.Join(c.cacheDir(), "presence.json") }
func (c Collector) presenceDue(host, key string, now time.Time) (int64, bool) {
	ts := now.UnixNano()
	due := false
	e := WithFileLock(c.presenceFile()+".lock", true, func() error {
		state := map[string]int64{}
		_ = ReadJSON(c.presenceFile(), &state)
		if state == nil {
			state = map[string]int64{}
		}
		k := host + "|" + key
		if last := time.Unix(0, state[k]); state[k] > 0 && !last.After(now) && now.Sub(last) < c.Config.Quota.presenceEvery() {
			return nil
		}
		state[k] = ts
		if e := WriteJSON(c.presenceFile(), state); e != nil {
			return e
		}
		due = true
		return nil
	})
	return ts, e == nil && due
}

// presenceUndo releases a slot claimed by presenceDue when its publish failed,
// so a lost PUT does not suppress the host for the whole interval. A newer
// claim is never removed.
func (c Collector) presenceUndo(host, key string, ts int64) {
	_ = WithFileLock(c.presenceFile()+".lock", true, func() error {
		state := map[string]int64{}
		_ = ReadJSON(c.presenceFile(), &state)
		k := host + "|" + key
		if state[k] != ts {
			return nil
		}
		delete(state, k)
		return WriteJSON(c.presenceFile(), state)
	})
}

// maybePresence publishes presence for an observation served from the shared
// store. allowed is the accountKey verdict, so a host never publishes for an
// account whose local CLI identity does not resolve to the same key.
func (c Collector) maybePresence(source ObservationSource, key string, allowed bool, o Observation) {
	if !allowed || o.Stale || o.Degraded || o.Source != "handoffkeep" {
		return
	}
	host := c.Config.Quota.presenceHostID()
	if host == "" {
		return
	}
	ts, due := c.presenceDue(host, key, c.quotaNow())
	if !due {
		return
	}
	c.publishPresence(source, key, c.quotaNow(), func(ok bool) {
		if !ok {
			c.presenceUndo(host, key, ts)
		}
	})
}
func (q QuotaConfig) freshTTL(pool string) time.Duration {
	if ttl := q.PoolFreshTTLS[pool]; ttl > 0 {
		return time.Duration(ttl * float64(time.Second))
	}
	if q.FreshTTLS > 0 {
		return time.Duration(q.FreshTTLS * float64(time.Second))
	}
	if pool == "claude" {
		return 15 * time.Minute
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
func (c Collector) sharedLocalFallback(pool, key, kind, message string, entry CacheEntry, now time.Time, degraded bool) Observation {
	o := entry.Result
	fp, k := identity(o)
	if key == "" || k != "account" || pool+":"+fp != key {
		o = Observation{}
	} else {
		o.AccountFP = fp
		o.AccountFPKind = k
		o.AccountFPMatch = ptr(true)
	}
	o.ID = pool
	o.PoolClass = c.Config.Pool(pool, now).Class
	o.Stale = true
	o.Degraded = degraded
	o.ErrorKind = kind
	o.Error = message
	if entry.FetchedAt > 0 && (len(o.Buckets) > 0 || o.FreeBucket != nil) {
		fetched := time.UnixMilli(int64(entry.FetchedAt * 1000))
		age := now.Sub(fetched).Seconds()
		if age < 0 {
			age = 0
		}
		o.FetchedAt = entry.FetchedAt
		o.AgeS = ptr(age)
		o.Buckets = rolloverExpired(o.Buckets, now)
	}
	return o
}
func (c Collector) sharedFallback(pool, key, kind string, entry CacheEntry, now time.Time) Observation {
	return c.sharedLocalFallback(pool, key, kind, "shared quota unavailable", entry, now, true)
}

// rolloverNote marks a bucket whose reset passed between measurement and use:
// the window restarted, so the reading is 0% and the next reset is unknown.
const rolloverNote = "reset passed; window restarted at 0%, unmeasured"

// rolloverExpired rewrites buckets whose reset already passed in a stale
// last-good observation. The window restarted since the measurement, so the
// old reading no longer applies; the bucket stays as a 0% marker instead of
// invalidating the windows that still hold.
func rolloverExpired(buckets []Bucket, now time.Time) []Bucket {
	out := make([]Bucket, 0, len(buckets))
	for _, b := range buckets {
		if reset, e := ParseTime(b.ResetsAt); e == nil && !reset.After(now) {
			b.UsedPct = Pct(0)
			b.ResetsAt = ""
			b.RolledOver = true
			b.Note = rolloverNote
		}
		out = append(out, b)
	}
	return out
}

// sharedAuto converts a shared observation. With stale set (the last-good path
// after a failed probe), a bucket whose reset already passed is kept as a
// rolled-over 0% marker instead of invalidating the whole observation.
func sharedAuto(x *SharedObservation, pool, key string, now time.Time, ttl time.Duration, stale bool) (Observation, bool) {
	if x == nil || x.Kind != "auto" || x.MeasuredAt.IsZero() {
		return Observation{}, false
	}
	age := now.Sub(x.MeasuredAt)
	free, validFree := sharedFreeBucket(x.FreeBucket, pool, now)
	if age < -time.Minute || age > ttl || !validFree {
		return Observation{}, false
	}
	if len(x.Buckets) == 0 && free == nil {
		return Observation{}, false
	}
	if pool == "devin" && free == nil && !stale {
		return Observation{}, false
	}
	if age < 0 {
		age = 0
	}
	o := Observation{ID: pool, Plan: x.Plan, Source: "handoffkeep", FetchedAt: float64(x.MeasuredAt.UnixMilli()) / 1000, AgeS: ptr(age.Seconds()), AccountFP: strings.SplitN(key, ":", 2)[1], AccountFPKind: "account", AccountFPMatch: ptr(true)}
	o.FreeBucket = free
	if pool == "devin" {
		// Shared organization identity is for publication. Keeping it separate
		// from AccountFP preserves the existing host-local free-limit identity
		// when a shared reading is saved to the local cache.
		o.PublishFP, o.PublishFPKind = o.AccountFP, o.AccountFPKind
		o.AccountFP, o.AccountFPKind = "", ""
	}
	for _, b := range x.Buckets {
		if !validPct(&b.UsedPct) || (b.Scope != "account" && b.Scope != "model" && b.Scope != "group") {
			return Observation{}, false
		}
		bucket := Bucket{Label: b.Window, Window: b.Window, UsedPct: ptr(b.UsedPct), Scope: Scope{Kind: b.Scope, Name: b.Model}, Horizon: horizon(b.Window)}
		if b.ResetsAt != nil {
			if !b.ResetsAt.After(now) {
				if !stale {
					return Observation{}, false
				}
				bucket.UsedPct = Pct(0)
				bucket.RolledOver = true
				bucket.Note = rolloverNote
			} else {
				bucket.ResetsAt = b.ResetsAt.Format(time.RFC3339Nano)
			}
		}
		o.Buckets = append(o.Buckets, bucket)
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

// ClaudeBar keeps a rate limiter per account: a 429 suppresses another fetch
// for Retry-After (or a five-minute default), while the last good reading stays
// available as stale evidence. A legacy scopefuel backoff does not transfer to
// shared mode because it was not recorded against this account key.
func sharedRateLimitBackoffActive(entry CacheEntry, pool, key string, now time.Time) bool {
	if entry.legacy || entry.NegativeUntil <= float64(now.UnixMilli())/1000 || entry.LastError == nil || entry.LastError.ErrorKind != "rate_limited" {
		return false
	}
	fp, kind := identity(*entry.LastError)
	return kind == "account" && pool+":"+fp == key
}

func (c Collector) sharedOne(ctx context.Context, source ObservationSource, pool string, entry CacheEntry, now time.Time) Observation {
	key, allowed := c.accountKey(pool)
	latest := SharedLatest{}
	reachable := false
	failWith := func(kind, message string) Observation {
		if !reachable {
			return c.sharedFallback(pool, key, kind, entry, now)
		}
		if o, ok := sharedAuto(latest.Observation, pool, key, c.quotaNow(), 365*24*time.Hour, true); ok {
			o.ID = pool
			o.Stale = true
			o.ErrorKind = kind
			o.Error = message
			return o
		}
		if kind == "rate_limited" {
			return c.sharedLocalFallback(pool, key, kind, message, entry, now, false)
		}
		return Observation{ID: pool, Stale: true, ErrorKind: kind, Error: message}
	}
	fail := func(kind string) Observation { return failWith(kind, "shared measurement pending") }
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
		c.maybePresence(source, key, allowed, o)
		return o
	}
	fresh := func() (Observation, bool) {
		at := c.quotaNow()
		o, ok := sharedAuto(latest.Observation, pool, key, at, c.Config.Quota.freshTTL(pool), false)
		if ok && pool == "devin" && allowed && devinFreeNeedsPublish(o.FreeBucket, localDevinFreeBucket(at), at) {
			return Observation{}, false
		}
		return o, ok
	}
	if pool == "anthropic-api" {
		return finish(fail("manual_only"))
	}
	if o, ok := fresh(); ok {
		_ = updateSnapshot(c.cacheFile(), pool, o, time.UnixMilli(int64(o.FetchedAt*1000)), entry)
		return finish(o)
	}
	backingOff := sharedRateLimitBackoffActive(entry, pool, key, c.quotaNow())
	// Read-only hosts also wait briefly for an in-flight publisher.
	holder := ""
	if allowed && (!backingOff || pool == "devin") {
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
		if o, ok := fresh(); ok {
			return finish(o)
		}
		if backingOff {
			return finish(failWith("rate_limited", "provider backoff active"))
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
	if o, ok := fresh(); ok {
		return finish(o)
	}
	provider := c.Providers[pool]
	if provider == nil && pool != "devin" {
		return finish(fail("credentials"))
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(c.Config.Quota.leaseTTL())*time.Second-200*time.Millisecond)
	defer cancel()
	observed := Observation{}
	if provider != nil && !(pool == "devin" && backingOff) {
		observed = provider.Fetch(probeCtx)
	}
	fp, kind := identity(observed)
	if pool == "devin" && observed.Source == "usage-api" {
		// The optional web source selects its organization independently of
		// the CLI login. Require its attested identity; current request config
		// is only an additional constraint, not proof of what was measured.
		configuredFP := fingerprint("devin-account", strings.TrimSpace(c.Config.Pools[pool].OrgID))
		if fp == "" || kind != "account" || configuredFP != "" && configuredFP != fp {
			return finish(fail("account_mismatch"))
		}
	}
	if (fp != "" && (kind != "account" || pool+":"+fp != key)) || (pool == "codex" || pool == "claude") && fp == "" {
		return finish(fail("account_mismatch"))
	}
	if pool == "devin" {
		if localAccountFP(pool, c.Selectors) != strings.SplitN(key, ":", 2)[1] {
			return finish(fail("account_mismatch"))
		}
		// The free bucket is unmeasurable and independent of paid-credit
		// probes. A failed probe contributes no paid percentage or plan.
		if observed.Error != "" || observed.Warning != "" || observed.Stale {
			if observed.ErrorKind == "rate_limited" {
				observed.ID = pool
				observed.PublishFP, observed.PublishFPKind = strings.SplitN(key, ":", 2)[1], "account"
				if observed.RetryAfterS <= 0 {
					observed.RetryAfterS = 300
				}
				if err := updateSnapshot(c.cacheFile(), pool, observed, c.quotaNow(), entry); err != nil {
					return finish(fail("cache_error"))
				}
			}
			observed = Observation{}
		}
		var prior *FreeBucketState
		if latest.Observation != nil {
			prior = latest.Observation.FreeBucket
		}
		at := c.quotaNow()
		observed.FreeBucket = mergeDevinFreeBucket(prior, localDevinFreeBucket(at), at)
	}
	if observed.Error != "" || observed.Warning != "" || observed.Stale || len(observed.Buckets) == 0 && observed.FreeBucket == nil {
		kind := observed.ErrorKind
		if kind == "" {
			kind = "probe_failed"
		}
		if kind == "rate_limited" {
			failedAt := c.quotaNow()
			if fp == "" {
				observed.PublishFP = strings.SplitN(key, ":", 2)[1]
				observed.PublishFPKind = "account"
			}
			if observed.RetryAfterS <= 0 {
				observed.RetryAfterS = 300
			}
			observed.ID = pool
			if e := updateSnapshot(c.cacheFile(), pool, observed, failedAt, entry); e != nil {
				return finish(fail("cache_error"))
			}
			message := observed.Error
			if message == "" {
				message = "provider rate limited"
			}
			return finish(failWith(kind, message))
		}
		return finish(fail(kind))
	}
	at := c.quotaNow()
	x := SharedObservation{Kind: "auto", MeasuredAt: at, Plan: observed.Plan, Buckets: []SharedBucket{}, FreeBucket: observed.FreeBucket}
	for _, b := range observed.Buckets {
		if pool == "devin" && devinFreeTag(b) {
			continue
		}
		// A bucket the provider reports without a value (for example Claude's
		// per-model weekly bucket with a null utilization) is unmeasured, not
		// invalid: leave it out instead of discarding the whole observation.
		// A value outside 0..100 still fails the probe.
		if b.UsedPct == nil {
			continue
		}
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
	o, ok := sharedAuto(&x, pool, key, at, c.Config.Quota.freshTTL(pool), false)
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
