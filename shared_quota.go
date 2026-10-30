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
	"sync"
	"time"
)

var accountKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}:[0-9a-f]{16}$`)
var accountFPRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ValidateAccountKey checks the pool and fingerprint of an explicit shared key.
func ValidateAccountKey(key string) error {
	pool, _, _ := strings.Cut(key, ":")
	if !accountKeyRE.MatchString(key) || !knownPool(pool) {
		return deny(Invalid, "invalid quota account key")
	}
	return nil
}

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

// accountKey resolves the shared account this host may measure. Pools with a
// derivable login (claude, codex, devin) take their key from the local
// credential; a configured fingerprint for them is ignored, so a stale or
// mismatched entry never blocks measurement, presence or publish. Other pools
// use the configured fingerprint and may measure only when the key is listed
// in measurable_accounts.
func (c Collector) accountKey(pool string) (string, bool) {
	if local := localAccountFP(pool, c.Selectors); local != "" {
		if !accountFPRE.MatchString(local) {
			return "", false
		}
		return pool + ":" + local, true
	}
	switch pool {
	case "claude", "codex", "devin":
		return "", false
	}
	fp := c.Config.Quota.Accounts[pool]
	if !accountFPRE.MatchString(fp) {
		return "", false
	}
	key := pool + ":" + fp
	for _, v := range c.Config.Quota.MeasurableAccounts {
		if v == key {
			return key, true
		}
	}
	return key, false
}

// presenceHostID is stable per host and never the raw hostname when an explicit
// identifier is absent. Precedence: environment, then quota.host_id, then a hash.
func (q QuotaConfig) presenceHostID() string {
	if v := strings.TrimSpace(FirstEnv("PACELEVER_HOST_ID", "HANDOFFKEEP_HOST_ID")); v != "" {
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

// PublishLog tracks asynchronous measurement publishes so the caller can wait
// for them and report failures before exit instead of letting them go missing
// silently.
type PublishLog struct {
	wg   sync.WaitGroup
	mu   sync.Mutex
	errs []error
}

func (l *PublishLog) start() {
	if l != nil {
		l.wg.Add(1)
	}
}
func (l *PublishLog) done() {
	if l != nil {
		l.wg.Done()
	}
}
func (l *PublishLog) add(err error) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, err)
}

// Wait blocks until every tracked publish has finished.
func (l *PublishLog) Wait() {
	if l != nil {
		l.wg.Wait()
	}
}

// Errors returns the recorded failures in arrival order.
func (l *PublishLog) Errors() []error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]error(nil), l.errs...)
}

// publishPresence records where an account was measured. It is asynchronous and
// bounded, so it never adds latency to a collection or changes its verdict, but
// it is tracked by PublishLog so the caller can wait for it and see its error.
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
	c.PublishLog.start()
	go func() {
		defer c.PublishLog.done()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := publisher.PublishPresence(ctx, record)
		if err != nil {
			c.PublishLog.add(fmt.Errorf("presence publish failed for %s", key))
		}
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
	if !allowed || o.Stale || o.Degraded || o.Source != SourceHandoffkeep {
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
	if key == "" || k != FPKindAccount || pool+":"+fp != key {
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
	o := Observation{ID: pool, Plan: x.Plan, Source: SourceHandoffkeep, FetchedAt: float64(x.MeasuredAt.UnixMilli()) / 1000, AgeS: ptr(age.Seconds()), AccountFP: accountKeyFP(key), AccountFPKind: FPKindAccount, AccountFPMatch: ptr(true)}
	o.FreeBucket = free
	if pool == "devin" {
		// Shared organization identity is for publication. Keeping it separate
		// from AccountFP preserves the existing host-local free-limit identity
		// when a shared reading is saved to the local cache.
		o.PublishFP, o.PublishFPKind = o.AccountFP, o.AccountFPKind
		o.AccountFP, o.AccountFPKind = "", ""
	}
	for _, b := range x.Buckets {
		if !validPct(&b.UsedPct) || (b.Scope != ScopeAccount && b.Scope != ScopeModel && b.Scope != ScopeGroup) {
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

// AccountObservation reads only the named account's fresh automatic evidence.
// Placement callers must never probe, publish presence, or fall back to the
// invoking host's cache, manual observations or login for another account.
func (c Collector) AccountObservation(ctx context.Context, key string) (Observation, error) {
	pool, _, _ := strings.Cut(key, ":")
	if err := ValidateAccountKey(key); err != nil {
		return Observation{}, err
	}
	source := c.sharedSource()
	if source == nil {
		return Observation{}, deny(Unmeasurable, "shared quota unavailable for account")
	}
	latest, err := source.Latest(ctx, key)
	if err != nil {
		return Observation{}, deny(Unmeasurable, "shared quota unavailable for account")
	}
	now := c.quotaNow()
	o, ok := sharedAuto(latest.Observation, pool, key, now, c.Config.Quota.freshTTL(pool), false)
	if !ok {
		// Missing, expired or rolled-over readings are stale; a present reading
		// that is invalid at any age is malformed and stays a plain refusal.
		if latest.Observation != nil {
			if _, wellFormed := sharedAuto(latest.Observation, pool, key, now, math.MaxInt64, true); !wellFormed {
				return Observation{}, deny(Unmeasurable, msgAccountMalformed)
			}
		}
		return Observation{}, deny(Unmeasurable, msgAccountStale)
	}
	o.PoolClass = c.Config.Pool(pool, c.quotaNow()).Class
	return o, nil
}

// IsAccountStale reports whether err is AccountObservation's refusal for a
// missing or expired automatic reading, as opposed to an unreachable store.
func IsAccountStale(err error) bool {
	e, ok := err.(*PolicyError)
	return ok && e.Code == Unmeasurable && e.Message == msgAccountStale
}

// RefreshAccount is the placement fallback for a stale named account. Only a
// host that holds the same account (its derived key equals key) and may measure
// it re-measures, publishes under the shared lease and returns the fresh
// reading; any other host is refused with the reason. A failed refresh refuses
// with the same code as a stale read and the failure kind, never provider text.
func (c Collector) RefreshAccount(ctx context.Context, key string) (Observation, error) {
	if err := ValidateAccountKey(key); err != nil {
		return Observation{}, err
	}
	pool, _, _ := strings.Cut(key, ":")
	own, allowed := c.accountKey(pool)
	switch {
	case own != key:
		return Observation{}, deny(Unmeasurable, msgAccountStale+"; this host does not hold the account")
	case !allowed:
		return Observation{}, deny(Unmeasurable, msgAccountStale+"; this host may not measure the account (measurable_accounts)")
	}
	observed, err := c.Collect(ctx, []string{pool})
	if err != nil || len(observed) != 1 {
		return Observation{}, deny(Unmeasurable, msgAccountStale+"; local refresh failed")
	}
	o := observed[0]
	fp := o.PublishFP
	if fp == "" {
		fp = o.AccountFP
	}
	if o.Stale || o.Degraded || o.Error != "" || o.Source != SourceHandoffkeep || pool+":"+fp != key {
		reason := msgAccountStale + "; local refresh failed"
		if o.ErrorKind != "" {
			reason += " (" + o.ErrorKind + ")"
		}
		return Observation{}, deny(Unmeasurable, reason)
	}
	return o, nil
}

func sharedManual(xs []SharedObservation, pool string) []ManualObservation {
	out := []ManualObservation{}
	for _, x := range xs {
		if x.Kind != "manual" || x.ExpiresAt == nil || len(x.Buckets) != 1 || x.Buckets[0].Scope != ScopeAccount || x.Buckets[0].Window != x.Window {
			continue
		}
		b := x.Buckets[0]
		m := ManualObservation{ID: fmt.Sprintf("hk-%d", x.ID), Pool: pool, Event: "set", Window: x.Window, UsedPct: Pct(b.UsedPct), MeasuredAt: x.MeasuredAt.Format(time.RFC3339Nano), ExpiresAt: x.ExpiresAt.Format(time.RFC3339Nano), Source: SourceOperator, SourceVerification: "unverified", SourceLabel: "shared manual observation"}
		if b.ResetsAt != nil {
			m.ResetsAt = b.ResetsAt.Format(time.RFC3339Nano)
		}
		out = append(out, m)
	}
	return out
}
func (c Collector) quotaNow() time.Time { return ClockNow(c.Now) }

// ClaudeBar keeps a rate limiter per account: a 429 suppresses another fetch
// for Retry-After (or a five-minute default), while the last good reading stays
// available as stale evidence. A backoff recorded against another account key
// does not apply here.
func sharedRateLimitBackoffActive(entry CacheEntry, pool, key string, now time.Time) bool {
	if entry.NegativeUntil <= float64(now.UnixMilli())/1000 || entry.LastError == nil || entry.LastError.ErrorKind != ErrKindRateLimited {
		return false
	}
	fp, kind := identity(*entry.LastError)
	return kind == FPKindAccount && pool+":"+fp == key
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
		if kind == ErrKindRateLimited {
			return c.sharedLocalFallback(pool, key, kind, message, entry, now, false)
		}
		return Observation{ID: pool, Stale: true, ErrorKind: kind, Error: message}
	}
	fail := func(kind string) Observation { return failWith(kind, "shared measurement pending") }
	if key == "" {
		return fail(ErrKindAccountUnknown)
	}
	var err error
	latest, err = source.Latest(ctx, key)
	if err != nil {
		reachable = false
		return fail(ErrKindSharedUnavailable)
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
		return finish(fail(ErrKindManualOnly))
	}
	if o, ok := fresh(); ok {
		_ = updateSnapshot(c.cacheFile(), pool, o, time.UnixMilli(int64(o.FetchedAt*1000)), entry)
		return finish(o)
	}
	backingOff := sharedRateLimitBackoffActive(entry, pool, key, c.quotaNow())
	// A measuring host that loses the lease waits for the in-flight publisher
	// until that holder's lease expires, then takes over the expired slot once:
	// a failed or killed publication never strands the account on "pending".
	// Read-only hosts keep the short wait and re-read.
	holder := ""
	publisherGone := false
	for attempts := 0; attempts < 2 && holder == "" && allowed && (!backingOff || pool == "devin"); attempts++ {
		holder, err = holderToken()
		if err != nil {
			return fail(ErrKindSharedUnavailable)
		}
		lease, e := source.Lease(ctx, key, holder, c.Config.Quota.leaseTTL())
		if e != nil {
			reachable = false
			return fail(ErrKindSharedUnavailable)
		}
		publisherGone = false
		waitedOut := false
		// Wall clock pacing: each window ends at the holder's lease expiry and
		// the whole wait is bounded by ctx and by two lease windows, so the
		// loop terminates even if the server keeps reporting new contention.
		deadline := time.Now().Add(2 * time.Duration(c.Config.Quota.leaseTTL()) * time.Second)
		for !lease.Acquired || lease.Holder != holder {
			if !lease.ExpiresAt.After(time.Now()) {
				publisherGone = true
				break
			}
			if time.Now().After(deadline) {
				waitedOut = true
				break
			}
			wait := c.Config.Quota.waitTime()
			if rem := time.Until(lease.ExpiresAt); rem < wait {
				wait = rem
			}
			if waitQuota(ctx, wait) != nil {
				return fail(ErrKindSharedWait)
			}
			latest, err = source.Latest(ctx, key)
			if err != nil {
				reachable = false
				return fail(ErrKindSharedUnavailable)
			}
			if o, ok := fresh(); ok {
				return finish(o)
			}
			// A holder that finishes or fails early releases its lease;
			// retrying each poll ends the wait on release, not on expiry. A
			// failed retry keeps the wait on the last seen expiry.
			if retry, e := source.Lease(ctx, key, holder, c.Config.Quota.leaseTTL()); e == nil {
				lease = retry
			}
		}
		if !lease.Acquired || lease.Holder != holder {
			holder = ""
		}
		if waitedOut {
			break
		}
	}
	if holder == "" {
		if waitQuota(ctx, c.Config.Quota.waitTime()) != nil {
			return fail(ErrKindSharedWait)
		}
		latest, err = source.Latest(ctx, key)
		if err != nil {
			reachable = false
			return fail(ErrKindSharedUnavailable)
		}
		if o, ok := fresh(); ok {
			return finish(o)
		}
		if backingOff {
			return finish(failWith(ErrKindRateLimited, "provider backoff active"))
		}
		if publisherGone {
			return finish(failWith(ErrKindSharedFailed, "shared measurement failed"))
		}
		return finish(fail(ErrKindSharedPending))
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
		return fail(ErrKindSharedUnavailable)
	}
	if o, ok := fresh(); ok {
		return finish(o)
	}
	provider := c.Providers[pool]
	if provider == nil && pool != "devin" {
		return finish(fail(ErrKindCredentials))
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(c.Config.Quota.leaseTTL())*time.Second-200*time.Millisecond)
	defer cancel()
	observed := Observation{}
	if provider != nil && !(pool == "devin" && backingOff) {
		observed = provider.Fetch(probeCtx)
	}
	fp, kind := identity(observed)
	if pool == "devin" && observed.Source == SourceUsageAPI {
		// The optional web source selects its organization independently of
		// the CLI login. Require its attested identity; current request config
		// is only an additional constraint, not proof of what was measured.
		configuredFP := accountFP("devin", strings.TrimSpace(c.Config.Pools[pool].OrgID))
		if fp == "" || kind != FPKindAccount || configuredFP != "" && configuredFP != fp {
			return finish(fail(ErrKindAccountMismatch))
		}
	}
	if (fp != "" && (kind != FPKindAccount || pool+":"+fp != key)) || (pool == "codex" || pool == "claude") && fp == "" {
		return finish(fail(ErrKindAccountMismatch))
	}
	if pool == "devin" {
		// The free bucket is unmeasurable and independent of paid-credit
		// probes. A failed probe contributes no paid percentage or plan.
		if observed.Error != "" || observed.Warning != "" || observed.Stale {
			if observed.ErrorKind == ErrKindRateLimited {
				observed.ID = pool
				observed.publishAccount(accountKeyFP(key))
				if observed.RetryAfterS <= 0 {
					observed.RetryAfterS = 300
				}
				if err := updateSnapshot(c.cacheFile(), pool, observed, c.quotaNow(), entry); err != nil {
					return finish(fail(ErrKindCache))
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
			kind = ErrKindProbeFailed
		}
		if kind == ErrKindRateLimited {
			failedAt := c.quotaNow()
			if fp == "" {
				observed.publishAccount(accountKeyFP(key))
			}
			if observed.RetryAfterS <= 0 {
				observed.RetryAfterS = 300
			}
			observed.ID = pool
			if e := updateSnapshot(c.cacheFile(), pool, observed, failedAt, entry); e != nil {
				return finish(fail(ErrKindCache))
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
			return finish(fail(ErrKindProbeFailed))
		}
		v := SharedBucket{Window: b.Window, UsedPct: *b.UsedPct, Scope: b.Scope.Kind, Model: b.Scope.Name}
		if b.ResetsAt != "" {
			reset, e := ParseTime(b.ResetsAt)
			if e != nil {
				return finish(fail(ErrKindProbeFailed))
			}
			v.ResetsAt = &reset
		}
		x.Buckets = append(x.Buckets, v)
	}
	if source.Publish(ctx, key, holder, x) != nil {
		reachable = false
		return finish(fail(ErrKindSharedPublishFailed))
	}
	o, ok := sharedAuto(&x, pool, key, at, c.Config.Quota.freshTTL(pool), false)
	if !ok {
		return finish(fail(ErrKindProbeFailed))
	}
	if releaseQuota(source, key, holder) != nil {
		reachable = false
		return finish(fail(ErrKindSharedReleaseFailed))
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
	b := SharedBucket{Window: m.Window, UsedPct: *m.UsedPct, Scope: ScopeAccount}
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
