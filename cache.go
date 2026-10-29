package pacelever

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Provider interface {
	Fetch(context.Context) Observation
}
type ProviderFunc func(context.Context) Observation

func (f ProviderFunc) Fetch(ctx context.Context) Observation { return f(ctx) }

type CacheEntry struct {
	FetchedAt     float64      `json:"fetched_at"`
	Result        Observation  `json:"result"`
	LastError     *Observation `json:"last_error,omitempty"`
	LastErrorAt   float64      `json:"last_error_at,omitempty"`
	NegativeUntil float64      `json:"negative_until,omitempty"`
}
type Collector struct {
	Shared          ObservationSource
	Selectors       map[string]string
	CachePath       string
	LegacyCachePath string
	Config          Config
	Providers       map[string]Provider
	Now             func() time.Time
	TTL             time.Duration
	NoCache         bool
}

func LoadSnapshots() (map[string]CacheEntry, error) {
	return loadSnapshots(CachePath(), legacyCachePath())
}
func loadSnapshots(ownPath, legacyPath string) (map[string]CacheEntry, error) {
	merged := map[string]CacheEntry{}
	for _, path := range []string{legacyPath, ownPath} {
		if path == "" {
			continue
		}
		var raw map[string]json.RawMessage
		if e := ReadJSON(path, &raw); e != nil {
			if os.IsNotExist(e) {
				continue
			}
			return nil, e
		}
		for pool, data := range raw {
			if !knownPool(pool) {
				continue
			}
			var entry CacheEntry
			if e := json.Unmarshal(data, &entry); e != nil {
				return nil, fmt.Errorf("invalid cache entry for pool %s", pool)
			}
			previous := merged[pool]
			if entry.FetchedAt <= 0 && previous.FetchedAt > 0 {
				entry.FetchedAt = previous.FetchedAt
				entry.Result = previous.Result
			}
			merged[pool] = entry
		}
	}
	return merged, nil
}

// Legacy scopefuel records failed-probe text as a string; native entries may
// carry a structured observation. Both forms preserve the last good snapshot.
func (c *CacheEntry) UnmarshalJSON(data []byte) error {
	type plain CacheEntry
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(data, &fields); e != nil {
		return fmt.Errorf("invalid cache entry")
	}
	failed := fields["last_error"]
	delete(fields, "last_error")
	body, e := json.Marshal(fields)
	if e != nil {
		return e
	}
	var value plain
	if e = json.Unmarshal(body, &value); e != nil {
		return fmt.Errorf("invalid cache entry fields")
	}
	if len(failed) > 0 && string(failed) != "null" {
		var message string
		if json.Unmarshal(failed, &message) == nil {
			value.LastError = &Observation{Error: message, ErrorKind: "legacy_failure"}
		} else {
			var observation Observation
			if json.Unmarshal(failed, &observation) != nil {
				return fmt.Errorf("invalid cache failure metadata")
			}
			value.LastError = &observation
		}
	}
	*c = CacheEntry(value)
	return nil
}

func updateSnapshot(ownPath, pool string, obs Observation, now time.Time, previous CacheEntry) error {
	return WithFileLock(ownPath+".lock", false, func() error {
		entries := map[string]CacheEntry{}
		if e := ReadJSON(ownPath, &entries); e != nil && !os.IsNotExist(e) {
			return e
		}
		entry := entries[pool]
		if entry.FetchedAt <= 0 && previous.FetchedAt > 0 {
			entry.FetchedAt = previous.FetchedAt
			entry.Result = previous.Result
		}
		if obs.Error == "" && obs.Warning == "" && len(obs.Buckets) > 0 {
			entry = CacheEntry{FetchedAt: float64(now.UnixMilli()) / 1000, Result: obs}
		} else if obs.ErrorKind != "probe_in_progress" {
			entry.LastError = &obs
			entry.LastErrorAt = float64(now.UnixMilli()) / 1000
			entry.NegativeUntil = entry.LastErrorAt + 30
			if obs.ErrorKind == "rate_limited" || obs.ErrorKind == "network" {
				entry.NegativeUntil = entry.LastErrorAt + 60
				if obs.RetryAfterS > 0 {
					entry.NegativeUntil = entry.LastErrorAt + obs.RetryAfterS
				}
			}
		}
		entries[pool] = entry
		return WriteJSON(ownPath, entries)
	})
}
func ttlFor(pool string) time.Duration {
	switch pool {
	case "claude":
		return 900 * time.Second
	case "devin", "commandcode":
		return 180 * time.Second
	}
	return 60 * time.Second
}
func (c Collector) Collect(ctx context.Context, pools []string) ([]Observation, error) {
	own, legacy := c.CachePath, c.LegacyCachePath
	if own == "" {
		own = CachePath()
		legacy = legacyCachePath()
	}
	entries, e := loadSnapshots(own, legacy)
	if e != nil {
		if c.sharedSource() == nil {
			return nil, e
		}
		entries = map[string]CacheEntry{}
	}
	results := make([]Observation, len(pools))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for i, pool := range pools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = Observation{ID: pool, Error: "collection deadline", ErrorKind: "timeout"}
				return
			}
			results[i] = c.one(ctx, pool, entries[pool])
		}()
	}
	wg.Wait()
	return results, nil
}
func (c Collector) one(ctx context.Context, pool string, entry CacheEntry) Observation {
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now()
	}
	p := c.Config.Pool(pool, now)
	fallback := func(kind, text string) Observation {
		o := entry.Result
		o.ID = pool
		o.PoolClass = p.Class
		o.ErrorKind = kind
		o.Error = text
		o.Stale = true
		if entry.FetchedAt > 0 {
			o.FetchedAt = entry.FetchedAt
			o.AgeS = ptr(float64(now.UnixMilli())/1000 - entry.FetchedAt)
		}
		return o
	}
	if p.Class == "exclude" {
		return Observation{ID: pool, Error: "pool excluded", ErrorKind: "excluded", PoolClass: p.Class}
	}
	// A profile override may enable one profile on an unsubscribed pool.
	enabled := c.Config.Subscribed("", pool)
	if !enabled {
		for name, profile := range c.Config.Profiles {
			if profile.Subscribed != nil && *profile.Subscribed {
				for _, r := range BundledCatalog().Rows {
					if r.Profile == name && r.Pool == pool {
						enabled = true
					}
				}
			}
		}
	}
	if !enabled {
		return Observation{ID: pool, Error: "pool unsubscribed", ErrorKind: "unsubscribed", PoolClass: p.Class}
	}
	if source := c.sharedSource(); source != nil {
		return c.sharedOne(ctx, source, pool, entry, now)
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = ttlFor(pool)
	}
	age := float64(now.UnixMilli())/1000 - entry.FetchedAt
	if !c.NoCache && entry.FetchedAt > 0 && age >= 0 && age <= ttl.Seconds() {
		o := entry.Result
		o.ID = pool
		o.FetchedAt = entry.FetchedAt
		o.AgeS = &age
		o.Stale = false
		o.PoolClass = p.Class
		return o
	}
	if entry.NegativeUntil > float64(now.UnixMilli())/1000 {
		kind := "backoff"
		if entry.LastError != nil && entry.LastError.ErrorKind != "" {
			kind = entry.LastError.ErrorKind
		}
		result := fallback(kind, "provider backoff active")
		return currentFailureIdentity(result, entry.LastError, entry.Result.AccountFP)
	}
	provider := c.Providers[pool]
	if provider == nil {
		return fallback("credentials", "provider not configured")
	}
	var obs Observation
	e := WithFileLock(filepath.Join(c.cacheDir(), "collect-"+pool+".lock"), true, func() error {
		obs = provider.Fetch(ctx)
		obs.ID = pool
		obs.PoolClass = p.Class
		return updateSnapshot(c.cacheFile(), pool, obs, now, entry)
	})
	if e == ErrBusy {
		return fallback("probe_in_progress", "probe already in progress")
	}
	if e != nil {
		return fallback("cache_error", "cannot persist provider observation")
	}
	if obs.Error != "" || obs.Warning != "" {
		result := fallback(obs.ErrorKind, obs.Error)
		return currentFailureIdentity(result, &obs, entry.Result.AccountFP)
	}
	obs.FetchedAt = float64(now.UnixMilli()) / 1000
	obs.AgeS = ptr(0.0)
	return obs
}
func (c Collector) Refresh(ctx context.Context, pool string) (Observation, error) {
	if !knownPool(pool) || pool == "anthropic-api" {
		return Observation{}, fmt.Errorf("unknown automatic pool")
	}
	c.NoCache = true
	r, e := c.Collect(ctx, []string{pool})
	if e != nil {
		return Observation{}, e
	}
	if len(r) != 1 {
		return Observation{}, fmt.Errorf("refresh unavailable")
	}
	return r[0], nil
}

func (c Collector) cacheFile() string {
	if c.CachePath != "" {
		return c.CachePath
	}
	return CachePath()
}
func (c Collector) cacheDir() string { return filepath.Dir(c.cacheFile()) }

func currentFailureIdentity(result Observation, current *Observation, cachedFP string) Observation {
	if current == nil || current.AccountFP == "" {
		return result
	}
	matches := cachedFP != "" && current.AccountFP == cachedFP
	result.AccountFPMatch = &matches
	result.AccountFP = current.AccountFP
	result.AccountFPKind = current.AccountFPKind
	if !matches {
		result.Buckets = nil
		result.PublishFP = current.PublishFP
		result.PublishFPKind = current.PublishFPKind
	}
	return result
}
