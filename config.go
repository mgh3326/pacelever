package pacelever

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type PoolPolicy struct {
	Exploration        ExplorationPolicy  `toml:"exploration" json:"exploration"`
	Class              string             `toml:"class" json:"class,omitempty"`
	Until              string             `toml:"until" json:"until,omitempty"`
	Note               string             `toml:"note" json:"note,omitempty"`
	Boost              float64            `toml:"boost" json:"boost,omitempty"`
	Cutoff             float64            `toml:"cutoff" json:"cutoff,omitempty"`
	Subscribed         *bool              `toml:"subscribed" json:"subscribed,omitempty"`
	CapacityWeight     float64            `toml:"capacity_weight" json:"capacity_weight,omitempty"`
	PriceUSD           float64            `toml:"price_usd" json:"price_usd,omitempty"`
	Plan               string             `toml:"plan" json:"plan,omitempty"`
	FusionBillingFP    string             `toml:"fusion_billing_fp" json:"-"`
	InflightReservePct map[string]float64 `toml:"inflight_reserve_pct" json:"inflight_reserve_pct,omitempty"`
	CreditMaxAgeS      float64            `toml:"credit_max_age_s" json:"credit_max_age_s,omitempty"`
	OnExhaust          string             `toml:"on_exhaust" json:"on_exhaust,omitempty"`
	Bin                string             `toml:"bin" json:"bin,omitempty"`
	// OrgID is Devin's organization id for its read-only web quota endpoint.
	// It is only used when DEVIN_BEARER_TOKEN is also set.
	OrgID string `toml:"org_id" json:"org_id,omitempty"`
}
type ProfilePolicy struct {
	Subscribed *bool `toml:"subscribed" json:"subscribed,omitempty"`
}
type BenchConfig struct {
	Backend                  string  `toml:"backend"`
	CacheTTLS                float64 `toml:"cache_ttl_s"`
	CatalogTTLS              float64 `toml:"catalog_ttl_s"`
	CatalogStaleMaxS         float64 `toml:"catalog_stale_max_s"`
	AllowPlaintextURL        bool    `toml:"allow_plaintext_url"`
	AllowPlaintextCatalog    bool    `toml:"allow_plaintext_catalog"`
	AllowPlaintextReps       bool    `toml:"allow_plaintext_reps"`
	AllowPlaintextQuotaShare bool    `toml:"allow_plaintext_quota_share"`
}
type QuotaConfig struct {
	URL                string             `toml:"url"`
	CredentialEnv      string             `toml:"credential_env"`
	HostID             string             `toml:"host_id"`
	FreshTTLS          float64            `toml:"fresh_ttl_s"`
	PoolFreshTTLS      map[string]float64 `toml:"pool_fresh_ttl_s"`
	LeaseTTLS          int                `toml:"lease_ttl_s"`
	WaitS              float64            `toml:"wait_s"`
	PresenceEveryS     float64            `toml:"presence_every_s"`
	Accounts           map[string]string  `toml:"accounts"`
	MeasurableAccounts []string           `toml:"measurable_accounts"`
}
type Config struct {
	Quota    QuotaConfig              `toml:"quota"`
	Pools    map[string]PoolPolicy    `toml:"pools"`
	Profiles map[string]ProfilePolicy `toml:"profiles"`
	Bench    BenchConfig              `toml:"bench"`
	Settings map[string]float64       `toml:"settings"`
	Raw      map[string]any           `toml:"-"`
	Path     string                   `toml:"-"`
	Warnings []string                 `toml:"-"`
}

func configBase() string { return envPath("XDG_CONFIG_HOME", ".config") }
func cacheBase() string  { return envPath("XDG_CACHE_HOME", ".cache") }
func dataBase() string   { return envPath("XDG_DATA_HOME", ".local/share") }
func envPath(key, fallback string) string {
	if s := os.Getenv(key); s != "" {
		return s
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

// FirstEnv is the first non-blank variable among keys, trimmed. Callers list
// the pacelever name before its handoffkeep alias.
func FirstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// retiredConfigDir is the config directory of the retired predecessor. It is
// the only remaining reference to that tool besides the wire schema ids; see
// the one-release fallback in LoadConfig.
const retiredConfigDir = "scopefuel"

func ConfigPath() string {
	if p := os.Getenv("PACELEVER_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(configBase(), "pacelever", "config.toml")
}
func CacheDir() string {
	if p := os.Getenv("PACELEVER_CACHE"); p != "" {
		return filepath.Dir(p)
	}
	return filepath.Join(cacheBase(), "pacelever")
}
func CachePath() string {
	if p := os.Getenv("PACELEVER_CACHE"); p != "" {
		return p
	}
	return filepath.Join(CacheDir(), "snapshots.json")
}
func LoadConfig() (Config, error) {
	path := ConfigPath()
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) && os.Getenv("PACELEVER_CONFIG") == "" {
		// One-release exception: a running host whose config still lives in the
		// retired tool's directory keeps loading it until it is moved. Remove
		// this fallback in the next release.
		path = filepath.Join(configBase(), retiredConfigDir, "config.toml")
		b, e = os.ReadFile(path)
	}
	c := Config{Pools: map[string]PoolPolicy{}, Profiles: map[string]ProfilePolicy{}, Settings: map[string]float64{}, Raw: map[string]any{}, Path: path}
	if os.IsNotExist(e) {
		return c, nil
	}
	if e != nil {
		return c, fmt.Errorf("cannot read configuration")
	}
	// Decode through an untyped map first so TOML date values remain compatible.
	if e = toml.Unmarshal(b, &c.Raw); e != nil {
		return c, fmt.Errorf("invalid TOML configuration (values withheld)")
	}
	normalized := string(b)
	// Pool expiry may be an unquoted TOML date. Decode that separately below.
	type wire struct {
		Quota    QuotaConfig               `toml:"quota"`
		Pools    map[string]map[string]any `toml:"pools"`
		Profiles map[string]ProfilePolicy  `toml:"profiles"`
		Bench    BenchConfig               `toml:"bench"`
		Settings map[string]float64        `toml:"settings"`
	}
	var w wire
	if e = toml.Unmarshal([]byte(normalized), &w); e != nil {
		return c, fmt.Errorf("invalid configuration types (values withheld)")
	}
	c.Quota = w.Quota
	if e := c.Quota.Validate(); e != nil {
		return c, e
	}
	c.Profiles = w.Profiles
	c.Bench = w.Bench
	c.Settings = w.Settings
	for name, v := range w.Pools {
		if !knownPool(name) {
			c.Warnings = append(c.Warnings, fmt.Sprintf("unknown pool [%s] ignored", name))
			continue
		}
		if until, ok := v["until"]; ok {
			v["until"] = fmt.Sprint(until)
		}
		b, e := toml.Marshal(v)
		if e != nil {
			return c, fmt.Errorf("invalid pool %s", name)
		}
		var p PoolPolicy
		if e = toml.Unmarshal(b, &p); e != nil {
			return c, fmt.Errorf("invalid pool %s types (values withheld)", name)
		}
		if p.Class != "" && p.Class != "spend" && p.Class != "preserve" && p.Class != "exclude" {
			return c, fmt.Errorf("invalid pools.%s.class", name)
		}
		if e := p.Exploration.Validate(); e != nil {
			return c, fmt.Errorf("invalid pools.%s.exploration: %w", name, e)
		}
		if p.Cutoff < 0 || p.Cutoff > 100 {
			return c, fmt.Errorf("invalid pools.%s.cutoff", name)
		}
		for window, n := range p.InflightReservePct {
			if n < 0 || n > 100 {
				return c, fmt.Errorf("invalid pools.%s.inflight_reserve_pct.%s", name, window)
			}
		}
		if p.CreditMaxAgeS < 0 || math.IsNaN(p.CreditMaxAgeS) || math.IsInf(p.CreditMaxAgeS, 0) || p.CreditMaxAgeS > 1e9 {
			return c, fmt.Errorf("invalid pools.%s.credit_max_age_s", name)
		}
		c.Pools[name] = p
	}
	sort.Strings(c.Warnings)
	return c, nil
}
func (c Config) Subscribed(profile, pool string) bool {
	if p, ok := c.Profiles[profile]; ok && p.Subscribed != nil {
		return *p.Subscribed
	}
	if p, ok := c.Pools[pool]; ok && p.Subscribed != nil {
		return *p.Subscribed
	}
	return true
}
func (c Config) Pool(pool string, now time.Time) PoolPolicy {
	p := c.Pools[pool]
	if p.Class == "" {
		p.Class = "spend"
	}
	if p.Until != "" {
		until, e := time.Parse("2006-01-02", strings.Split(p.Until, "T")[0])
		if e != nil || now.UTC().Format("2006-01-02") > until.Format("2006-01-02") {
			p.Class = "spend"
			p.Boost = 0
		}
	}
	if p.Cutoff == 0 {
		p.Cutoff = 99
	}
	if p.CapacityWeight <= 0 {
		p.CapacityWeight = 1
	}
	return p
}
func (c Config) PaceThreshold() float64 {
	for _, k := range []string{"altlane_pace_multiple", "altlane_pace_threshold"} {
		if n := c.Settings[k]; n > 0 {
			return n
		}
	}
	return 1.2
}

// CreditMaxAge bounds how long a hand-entered credit balance is trusted before
// it is admitted with a staleness warning. It never substitutes a number.
func (c Config) CreditMaxAge(pool string) time.Duration {
	if s := c.Pools[pool].CreditMaxAgeS; s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	return 72 * time.Hour
}

// EditConfig preserves all unrecognized sections and keys. A config loaded from
// the retired directory is copied into the pacelever config; the old file is
// never changed.
func EditConfig(edit func(map[string]any) error) error {
	return WithFileLock(ConfigPath()+".lock", false, func() error {
		c, e := LoadConfig()
		if e != nil {
			return e
		}
		if e = edit(c.Raw); e != nil {
			return e
		}
		b, e := toml.Marshal(c.Raw)
		if e != nil {
			return fmt.Errorf("cannot encode configuration")
		}
		return AtomicWrite(ConfigPath(), b)
	})
}
