package pacelever

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type BenchClient struct {
	URL            string
	Token          string
	CFClientID     string
	CFClientSecret string
	Config         Config
	Client         HTTPDoer
	Now            func() time.Time
}

func NewBenchClient(cfg Config) BenchClient {
	b := BenchClient{Config: cfg}
	envOverride := FirstEnv("HANDOFFKEEP_URL", "HANDOFFKEEP_TOKEN") != ""
	values := map[string]string{}
	if !envOverride {
		path := os.Getenv("HANDOFFKEEP_CONFIG")
		if path == "" {
			path = filepath.Join(configBase(), "handoffkeep", "config.env")
		}
		data, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if ok {
				values[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), "\"'")
			}
		}
	}
	read := func(k string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		if !envOverride {
			return values[k]
		}
		return ""
	}
	b.URL = read("HANDOFFKEEP_URL")
	b.Token = read("HANDOFFKEEP_TOKEN")
	b.CFClientID = read("HANDOFFKEEP_CF_ACCESS_CLIENT_ID")
	b.CFClientSecret = read("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET")
	return b
}
func (b BenchClient) now() time.Time { return ClockNow(b.Now) }
func (b BenchClient) endpointID() string {
	s := sha256.Sum256([]byte(b.URL))
	return hex.EncodeToString(s[:8])
}
func (b BenchClient) remote(use string) bool {
	if b.Config.Bench.Backend == "local" || b.URL == "" || b.Token == "" {
		return false
	}
	return b.allowed(use) == nil
}
func (b BenchClient) allowed(use string) error {
	u, e := url.Parse(b.URL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid backend endpoint (value withheld)")
	}
	if (b.CFClientID == "") != (b.CFClientSecret == "") {
		return fmt.Errorf("CF Access credential pair incomplete")
	}
	if u.Scheme == "https" {
		return nil
	}
	allow := b.Config.Bench.AllowPlaintextURL
	switch use {
	case "catalog":
		allow = allow || b.Config.Bench.AllowPlaintextCatalog
	case "reps":
		allow = allow || b.Config.Bench.AllowPlaintextReps
	case "quota_share":
		allow = allow || b.Config.Bench.AllowPlaintextQuotaShare
	}
	if u.Scheme != "http" || !allow {
		return fmt.Errorf("backend requires HTTPS or explicit plaintext opt-in for %s", use)
	}
	return nil
}

// Send suppresses URLs, credentials, response bodies and underlying transport
// errors. It refuses redirects so authentication never follows another origin.
func (b BenchClient) Send(ctx context.Context, method, path, use string, body any, out any) error {
	if b.URL == "" || b.Token == "" {
		return fmt.Errorf("handoffkeep credentials unavailable")
	}
	if e := b.allowed(use); e != nil {
		return e
	}
	var data []byte
	var e error
	if body != nil {
		data, e = json.Marshal(body)
		if e != nil {
			return fmt.Errorf("invalid request JSON")
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.URL, "/")+path, bytes.NewReader(data))
	if e != nil {
		return fmt.Errorf("invalid backend request")
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pacelever")
	if b.CFClientID != "" {
		req.Header.Set("CF-Access-Client-Id", b.CFClientID)
		req.Header.Set("CF-Access-Client-Secret", b.CFClientSecret)
	}
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, e := client.Do(req)
	if e != nil {
		return fmt.Errorf("handoffkeep request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &BackendError{Status: resp.StatusCode}
	}
	if out != nil {
		if e = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); e != nil {
			return fmt.Errorf("handoffkeep response is not valid JSON")
		}
	}
	return nil
}

type BackendError struct{ Status int }

func (e *BackendError) Error() string {
	return fmt.Sprintf("handoffkeep HTTP %d (response withheld)", e.Status)
}

type benchCache struct {
	FetchedAt  string           `json:"fetched_at"`
	EndpointID string           `json:"endpoint_id"`
	Rows       []map[string]any `json:"rows"`
}

func (b BenchClient) cachePath(scope string) string {
	return filepath.Join(CacheDir(), "bench", b.endpointID()+"-"+scope+".json")
}
func (b BenchClient) ReadRows(ctx context.Context, scope string) ([]map[string]any, string, error) {
	use := "catalog"
	if scope == "reps" {
		use = "reps"
	}
	if !b.remote(use) {
		if b.Config.Bench.Backend == "handoffkeep" {
			return nil, BenchUnavailable, fmt.Errorf("configured handoffkeep backend unavailable")
		}
		return nil, BenchLocal, nil
	}
	var cache benchCache
	_ = ReadJSON(b.cachePath(scope), &cache)
	ttl := b.Config.Bench.CacheTTLS
	if ttl <= 0 {
		ttl = 86400
	}
	if scope == "catalog" || scope == "area-exceptions" {
		ttl = b.Config.Bench.CatalogTTLS
		if ttl <= 0 {
			ttl = 3600
		}
	}
	stamp, _ := ParseTime(cache.FetchedAt)
	age := b.now().Sub(stamp).Seconds()
	if cache.EndpointID == b.endpointID() && cache.Rows != nil && age >= 0 && age < ttl {
		return cache.Rows, BenchCache, nil
	}
	var payload map[string]json.RawMessage
	query := ""
	if scope == "catalog" {
		query = "?include_retired=1"
	}
	if scope == "reps" {
		query = "?limit=5000"
	}
	e := b.Send(ctx, "GET", "/v1/bench/"+scope+query, use, nil, &payload)
	key := scope
	if scope == "area-exceptions" {
		key = "exceptions"
	}
	var rows []map[string]any
	if e == nil {
		decoder := json.NewDecoder(bytes.NewReader(payload[key]))
		decoder.UseNumber()
		if e = decoder.Decode(&rows); e == nil && rows != nil {
			if scope == "catalog" {
				_, e = decodeCatalog(rows)
			}
		} else {
			e = fmt.Errorf("missing backend row array")
		}
	}
	if e == nil {
		cache = benchCache{FetchedAt: b.now().Format(time.RFC3339Nano), EndpointID: b.endpointID(), Rows: rows}
		if err := WriteJSON(b.cachePath(scope), cache); err != nil {
			return rows, BenchServer, fmt.Errorf("cannot persist bench cache")
		}
		return rows, BenchServer, nil
	}
	if cache.EndpointID == b.endpointID() && cache.Rows != nil {
		stamp, _ := ParseTime(cache.FetchedAt)
		age = b.now().Sub(stamp).Seconds()
		max := b.Config.Bench.CatalogStaleMaxS
		if max <= 0 {
			max = 86400
		}
		source := BenchCache
		if age < 0 || age >= max {
			source = BenchCacheStale
		}
		return cache.Rows, source, nil
	}
	return nil, BenchUnavailable, e
}
func decodeCatalog(rows []map[string]any) ([]CatalogRow, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("empty catalog")
	}
	base := BundledCatalog()
	var out []CatalogRow
	seen := map[string]bool{}
	for _, v := range rows {
		data, _ := json.Marshal(v)
		var r CatalogRow
		if json.Unmarshal(data, &r) != nil {
			return nil, fmt.Errorf("invalid catalog row types")
		}
		if r.Profile == "" || r.ModelID == "" || gradeRank(r.Grade) == len(Grades) || (r.Gate != "default" && r.Gate != "escalation" && r.Gate != "consult_only") {
			return nil, fmt.Errorf("catalog row lacks valid identity, grade or gate")
		}
		if !knownPool(r.Pool) {
			continue
		}
		key := r.Profile + "@" + r.Effort
		if seen[key] {
			return nil, fmt.Errorf("duplicate catalog rung")
		}
		seen[key] = true
		r.Placed = true
		for _, old := range base.Rows {
			if old.Profile == r.Profile && old.Effort == r.Effort {
				r.Billing = old.Billing
				r.Family = old.Family
				r.Group = old.Group
				if !old.Placed && r.Grade == old.Grade {
					r.Placed = false
				}
				break
			}
		}
		// A remote agy row without a bundled base (a new profile) still
		// meters against its account group, inferred from the model id;
		// an unrecognised id stays ungrouped and so fails closed.
		if r.Pool == "agy" && r.Group == "" {
			if g := agyGroup(r.ModelID, ""); g != "unknown" {
				r.Group = g
			}
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("catalog contains no supported pool rows")
	}
	if e := ValidateMonotonic(out); e != nil {
		return nil, e
	}
	return out, nil
}
func (b BenchClient) ReadCatalog(ctx context.Context) (Catalog, error) {
	base := BundledCatalog()
	if !b.remote("catalog") {
		if b.Config.Bench.Backend == "handoffkeep" {
			base.Info.Stale = true
			return base, fmt.Errorf("configured catalog backend unavailable")
		}
		return base, nil
	}
	rows, source, e := b.ReadRows(ctx, "catalog")
	if e != nil {
		base.Info.Stale = true
		return base, e
	}
	decoded, e := decodeCatalog(rows)
	if e != nil {
		base.Info.Stale = true
		return base, e
	}
	profiles := map[string]bool{}
	for _, r := range decoded {
		profiles[r.Profile] = true
	}
	for _, r := range base.Rows {
		if !profiles[r.Profile] {
			decoded = append(decoded, r)
		}
	}
	base.Rows = decoded
	base.Info = CatalogInfo{Source: source, Stale: source == BenchCacheStale}
	var cache benchCache
	if ReadJSON(b.cachePath("catalog"), &cache) == nil {
		if at, e := ParseTime(cache.FetchedAt); e == nil {
			base.Info.AgeS = ptr(b.now().Sub(at).Seconds())
		}
	}
	return base, nil
}
func CatalogWire(rows []CatalogRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		v := map[string]any{"profile": r.Profile, "effort": r.Effort, "model_id": r.ModelID, "pool": r.Pool, "grade": r.Grade, "score": r.Score, "gate": r.Gate, "gate_reason": r.GateReason, "boundary_version": r.BoundaryVersion, "deviation_ref": r.DeviationRef, "decided_at": r.DecidedAt, "decided_by": r.DecidedBy, "retired_at": r.RetiredAt}
		out = append(out, v)
	}
	return out
}
func (b BenchClient) PushCatalog(ctx context.Context, rows []CatalogRow) error {
	if e := ValidateMonotonic(rows); e != nil {
		return e
	}
	for _, r := range rows {
		if r.DecidedBy == "" || r.DeviationRef == "" {
			return fmt.Errorf("catalog provenance required")
		}
		if _, e := ParseTime(r.DecidedAt); e != nil {
			return fmt.Errorf("catalog decided_at must include timezone")
		}
	}
	return b.Send(ctx, "PUT", "/v1/bench/catalog", "catalog", map[string]any{"catalog": CatalogWire(rows)}, nil)
}
func ValidateMonotonic(rows []CatalogRow) error {
	byProfile := map[string][]CatalogRow{}
	for _, r := range rows {
		if r.RetiredAt == nil && r.Effort != "" {
			byProfile[r.Profile] = append(byProfile[r.Profile], r)
		}
	}
	// Profiles are checked in name order so the reported violation is stable.
	for _, profile := range slices.Sorted(maps.Keys(byProfile)) {
		rr := byProfile[profile]
		sort.Slice(rr, func(i, j int) bool { return effortRank(rr[i].Effort) < effortRank(rr[j].Effort) })
		for i := 1; i < len(rr); i++ {
			if gradeRank(rr[i].Grade) > gradeRank(rr[i-1].Grade) {
				return fmt.Errorf("catalog effort grades are not monotonic for %s", rr[i].Profile)
			}
		}
	}
	return nil
}
func mapInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}
