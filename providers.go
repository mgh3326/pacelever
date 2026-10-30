package pacelever

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Providers only return allowlisted measurement fields. Response bodies and
// credentials never enter Observation or a diagnostic.
type HTTPProvider struct {
	ID      string
	URL     string
	Headers map[string]string
	Client  HTTPDoer
	Now     func() time.Time
	// RateLimitWaitS is the negative-cache wait applied when a 429 carries no
	// usable Retry-After. Zero falls back to the collector's default backoff.
	RateLimitWaitS float64
	// AuthKind overrides the error kind for 401/403. AuthHint replaces the
	// generic HTTP message without including the credential.
	AuthKind string
	AuthHint string
}

func (p HTTPProvider) Fetch(ctx context.Context) Observation {
	o := Observation{ID: p.ID, Source: SourceUsageAPI}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	req, e := http.NewRequestWithContext(ctx, "GET", p.URL, nil)
	if e != nil {
		o.Error = "invalid provider endpoint"
		return o
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "pacelever")
	resp, e := client.Do(req)
	if e != nil {
		o.fail(ErrKindNetwork, msgRequestFailed)
		return o
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return statusObservation(p.ID, resp.StatusCode, resp.Header, p.now(), p.AuthKind, p.AuthHint, p.RateLimitWaitS)
	}
	var raw map[string]json.RawMessage
	if e = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&raw); e != nil {
		o.fail(ErrKindParse, msgInvalidJSON)
		return o
	}
	switch p.ID {
	case "claude":
		parseClaudeAPI(&o, raw)
	case "codex":
		parseCodexAPI(&o, raw, resp.Header, p.now())
	case "clinepass":
		parseClinePass(&o, raw)
	}
	if len(o.Buckets) == 0 {
		o.fail(ErrKindParse, msgNoWindows)
	}
	return o
}
func (p HTTPProvider) now() time.Time { return ClockNow(p.Now) }

// retryAfterSeconds reads a Retry-After header as delay-seconds or an
// HTTP-date. Missing, zero, negative and unparseable values return 0 so the
// caller applies its own default wait.
func retryAfterSeconds(header string, now time.Time) float64 {
	v := strings.TrimSpace(header)
	if v == "" {
		return 0
	}
	if n, ok := ParseFinite(v); ok {
		if n <= 0 {
			return 0
		}
		return math.Min(n, 86400)
	}
	if at, e := http.ParseTime(v); e == nil {
		if d := at.Sub(now).Seconds(); d > 0 {
			return math.Min(d, 86400)
		}
	}
	return 0
}

// statusObservation is the single mapping from an HTTP status to a safe
// observation. Response bodies and request URLs never enter the result.
func statusObservation(id string, status int, header http.Header, now time.Time, authKind, authHint string, rateWait float64) Observation {
	o := Observation{ID: id, Source: SourceUsageAPI}
	o.fail(ErrKindHTTP, fmt.Sprintf("usage HTTP %d", status))
	switch status {
	case 401, 403:
		o.ErrorKind = authKind
		if o.ErrorKind == "" {
			o.ErrorKind = ErrKindAuth
		}
		if authHint != "" {
			o.Error = authHint
		}
	case 429:
		o.ErrorKind = ErrKindRateLimited
		if header != nil {
			o.RetryAfterS = retryAfterSeconds(header.Get("Retry-After"), now)
		}
		if o.RetryAfterS <= 0 {
			o.RetryAfterS = rateWait
		}
	}
	return o
}

type httpJSON struct {
	Status     int
	Header     http.Header
	Body       map[string]any
	ParseError bool
}

// doJSON performs one allowlisted request. It never returns the endpoint, a
// credential or a response body in an error.
func doJSON(ctx context.Context, client HTTPDoer, method, endpoint string, headers map[string]string, payload []byte) (httpJSON, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, body)
	if e != nil {
		return httpJSON{}, fmt.Errorf("invalid provider request")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "pacelever")
	}
	resp, e := client.Do(req)
	if e != nil {
		return httpJSON{}, fmt.Errorf("usage request failed")
	}
	defer resp.Body.Close()
	out := httpJSON{Status: resp.StatusCode, Header: resp.Header}
	// Decode every status so a caller can tell an API error from an HTML edge
	// rejection. The body is never retained in an observation or an error.
	if e = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out.Body); e != nil {
		out.ParseError = true
	}
	return out, nil
}

func fingerprint(prefix, value string) string {
	if value == "" {
		return ""
	}
	b := sha256.Sum256([]byte(prefix + "|" + value))
	return hex.EncodeToString(b[:8])
}
func DefaultProviders(cfg Config) map[string]Provider { return DefaultProvidersFor(cfg, nil) }
func DefaultProvidersFor(cfg Config, selectors map[string]string) map[string]Provider {
	return DefaultProvidersAt(cfg, selectors, nil)
}

// DefaultProvidersAt wires every provider to one clock; nil means the wall
// clock in UTC.
func DefaultProvidersAt(cfg Config, selectors map[string]string, now func() time.Time) map[string]Provider {
	clock := clockFunc(now)
	home, _ := os.UserHomeDir()
	result := map[string]Provider{}
	result["claude"] = ProviderFunc(func(ctx context.Context) Observation {
		credentialPath, identityPath, service, shareable := claudeLocations(home, selectors)
		oauth := object(readObject(credentialPath)["claudeAiOauth"])
		if runtime.GOOS == "darwin" {
			out, e := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-w").Output()
			if e == nil {
				var data map[string]any
				if json.Unmarshal(out, &data) == nil {
					oauth = chooseOAuth(oauth, object(data["claudeAiOauth"]), clock())
				}
			}
		}
		token := str(oauth["accessToken"])
		if token == "" {
			return failed("claude", ErrKindCredentials, msgNoCredentials)
		}
		if expires, ok := finiteNumber(oauth["expiresAt"]); ok && expires/1000 < float64(clock().Unix()) {
			return failed("claude", ErrKindTokenExpired, "token expired")
		}
		provider := ClaudeProvider{
			API: HTTPProvider{ID: "claude", URL: "https://api.anthropic.com/api/oauth/usage", Headers: map[string]string{"Authorization": "Bearer " + token, "anthropic-beta": "oauth-2025-04-20"}, RateLimitWaitS: 300, Now: clock},
			CLI: ClaudeCLIProvider{Binary: ResolveBinary("claude", cfg), Env: selectors, Now: clock},
		}
		return claudeAccountFingerprint(provider, identityPath, shareable).Fetch(ctx)
	})
	result["codex"] = ProviderFunc(func(ctx context.Context) Observation {
		return fetchCodex(ctx, home, selectors, cfg, nil, clock)
	})
	result["clinepass"] = ProviderFunc(func(ctx context.Context) Observation {
		token := clineToken(home)
		if token == "" {
			return failed("clinepass", ErrKindCredentials, msgNoCredentials)
		}
		return (HTTPProvider{ID: "clinepass", URL: "https://api.cline.bot/api/v1/users/me/plan/usage-limits", Headers: map[string]string{"Authorization": "Bearer " + token}, Now: clock}).Fetch(ctx)
	})
	result["commandcode"] = ProviderFunc(func(ctx context.Context) Observation {
		return fetchCommandCode(ctx, commandCodeToken(home), nil, clock)
	})
	result["grok"] = ProviderFunc(func(ctx context.Context) Observation {
		record, ok := selectGrokRecord(grokRecords(home, selectors), cfg.Quota.Accounts["grok"], clock())
		if !ok {
			return failed("grok", ErrKindCredentials, msgNoCredentials)
		}
		o := fetchGrokRecord(ctx, record, nil, clock)
		o.publishAccount(record.Fingerprint)
		return o
	})
	agy := newAgyProvider(home, cfg, nil)
	agy.now = clock
	result["agy"] = agy
	// Devin keeps its CLI probe for the per-model Free tags; the read-only web
	// quota is tried first when its environment token and org setting are set.
	result["devin"] = PTYProvider{ID: "devin", Binary: ResolveBinary("devin", cfg), Timeout: 35 * time.Second, Env: selectors, Preferred: devinWebProvider(cfg, nil, clock), Now: clock}
	for _, pool := range []string{"kimi", "kiro"} {
		result[pool] = PTYProvider{ID: pool, Binary: ResolveBinary(pool, cfg), Timeout: 35 * time.Second, Env: selectors, Now: clock}
	}
	return result
}
