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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
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
	o := Observation{ID: p.ID, Source: "usage-api"}
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
		o.Error = "usage request failed"
		o.ErrorKind = "network"
		return o
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return statusObservation(p.ID, resp.StatusCode, resp.Header, p.now(), p.AuthKind, p.AuthHint, p.RateLimitWaitS)
	}
	var raw map[string]json.RawMessage
	if e = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&raw); e != nil {
		o.Error = "invalid usage JSON"
		o.ErrorKind = "parse_error"
		return o
	}
	switch p.ID {
	case "claude":
		for _, axis := range []struct{ key, window string }{{"five_hour", "5h"}, {"seven_day", "7d"}} {
			var w struct {
				Utilization *float64 `json:"utilization"`
				ResetsAt    string   `json:"resets_at"`
			}
			if json.Unmarshal(raw[axis.key], &w) == nil {
				o.Buckets = append(o.Buckets, Bucket{Label: axis.window, Window: axis.window, UsedPct: validated(w.Utilization), ResetsAt: w.ResetsAt, Scope: Scope{Kind: "account"}, Horizon: horizon(axis.window)})
			}
		}
		var limits []struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
			Scope       struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		}
		_ = json.Unmarshal(raw["limits"], &limits)
		for _, w := range limits {
			if w.Scope.Model.DisplayName != "" {
				o.Buckets = append(o.Buckets, Bucket{Label: w.Scope.Model.DisplayName, Window: "7d", UsedPct: validated(w.Utilization), ResetsAt: w.ResetsAt, Scope: Scope{Kind: "model", Name: w.Scope.Model.DisplayName}, Horizon: "week"})
			}
		}
	case "codex":
		_ = json.Unmarshal(raw["plan_type"], &o.Plan)
		var limit struct {
			Primary   *codexWindow `json:"primary_window"`
			Secondary *codexWindow `json:"secondary_window"`
		}
		_ = json.Unmarshal(raw["rate_limit"], &limit)
		for _, w := range []*codexWindow{limit.Primary, limit.Secondary} {
			if w != nil {
				o.Buckets = append(o.Buckets, w.bucket(Scope{Kind: "account"}))
			}
		}
		var extra []struct {
			Name  string `json:"limit_name"`
			Limit struct {
				Primary   *codexWindow `json:"primary_window"`
				Secondary *codexWindow `json:"secondary_window"`
			} `json:"rate_limit"`
		}
		_ = json.Unmarshal(raw["additional_rate_limits"], &extra)
		for _, x := range extra {
			for _, w := range []*codexWindow{x.Limit.Primary, x.Limit.Secondary} {
				if w != nil {
					o.Buckets = append(o.Buckets, w.bucket(Scope{Kind: "model", Name: x.Name}))
				}
			}
		}
	case "clinepass":
		var data struct {
			Limits []struct {
				Type  string   `json:"type"`
				Used  *float64 `json:"percentUsed"`
				Reset string   `json:"resetsAt"`
			} `json:"limits"`
		}
		_ = json.Unmarshal(raw["data"], &data)
		for _, w := range data.Limits {
			window := map[string]string{"five_hour": "5h", "weekly": "7d", "monthly": "30d"}[w.Type]
			if window != "" {
				o.Buckets = append(o.Buckets, Bucket{Label: w.Type, Window: window, UsedPct: validated(w.Used), ResetsAt: w.Reset, Scope: Scope{Kind: "account"}, Horizon: horizon(window)})
			}
		}
	}
	if len(o.Buckets) == 0 {
		o.Error = "usage response has no recognized windows"
		o.ErrorKind = "parse_error"
	}
	return o
}
func (p HTTPProvider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// retryAfterSeconds reads a Retry-After header as delay-seconds or an
// HTTP-date. Missing, zero, negative and unparseable values return 0 so the
// caller applies its own default wait.
func retryAfterSeconds(header string, now time.Time) float64 {
	v := strings.TrimSpace(header)
	if v == "" {
		return 0
	}
	if n, e := strconv.ParseFloat(v, 64); e == nil {
		if n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
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
	o := Observation{ID: id, Source: "usage-api"}
	o.Error = fmt.Sprintf("usage HTTP %d", status)
	o.ErrorKind = "http"
	switch status {
	case 401, 403:
		o.ErrorKind = authKind
		if o.ErrorKind == "" {
			o.ErrorKind = "auth"
		}
		if authHint != "" {
			o.Error = authHint
		}
	case 429:
		o.ErrorKind = "rate_limited"
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
	if resp.StatusCode != 200 {
		return out, nil
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out.Body); e != nil {
		out.ParseError = true
	}
	return out, nil
}

func fetchCommandCode(ctx context.Context, token string, client HTTPDoer, now func() time.Time) Observation {
	if token == "" {
		return Observation{ID: "commandcode", Error: "credentials unavailable", ErrorKind: "credentials"}
	}
	// The API's edge rejects generic clients; use the CLI's own user agent.
	headers := map[string]string{"Authorization": "Bearer " + token, "User-Agent": "cmd/1.88.1"}
	who, e := doJSON(ctx, client, "GET", "https://api.commandcode.ai/alpha/whoami?limits=1", headers, nil)
	if e != nil {
		return Observation{ID: "commandcode", Error: "usage request failed", ErrorKind: "network"}
	}
	if who.Status != 200 {
		return statusObservation("commandcode", who.Status, who.Header, now(), "auth_failed", "commandcode authentication failed; run cmd login or set COMMAND_CODE_API_KEY", 0)
	}
	if who.ParseError {
		return Observation{ID: "commandcode", Error: "invalid usage JSON", ErrorKind: "parse_error"}
	}
	identity := object(who.Body["data"])
	if len(identity) == 0 {
		identity = who.Body
	}
	orgID := str(object(identity["org"])["id"])
	endpoint := "https://api.commandcode.ai/alpha/billing/credits"
	if orgID != "" {
		endpoint += "?orgId=" + url.QueryEscape(orgID)
	}
	billing, e := doJSON(ctx, client, "GET", endpoint, headers, nil)
	if e != nil {
		return Observation{ID: "commandcode", Error: "usage request failed", ErrorKind: "network"}
	}
	if billing.Status != 200 {
		return statusObservation("commandcode", billing.Status, billing.Header, now(), "auth_failed", "commandcode authentication failed; run cmd login or set COMMAND_CODE_API_KEY", 0)
	}
	if billing.ParseError {
		return Observation{ID: "commandcode", Error: "invalid usage JSON", ErrorKind: "parse_error"}
	}
	o := parseCommandCode(billing.Body, now())
	account := str(object(identity["user"])["id"])
	if account == "" {
		account = orgID
	}
	if fp := fingerprint("commandcode-account", account); fp != "" {
		o.PublishFP = fp
		o.PublishFPKind = "account"
	}
	return o
}

func parseCommandCode(raw map[string]any, now time.Time) Observation {
	o := Observation{ID: "commandcode", Source: "usage-api"}
	data := object(raw["data"])
	if len(data) == 0 {
		data = raw
	}
	limits := object(data["windowLimits"])
	for _, axis := range []struct{ key, window string }{{"fiveHour", "5h"}, {"weekly", "7d"}} {
		w := object(limits[axis.key])
		capacity, capacityOK := finiteNumber(w["cap"])
		if !capacityOK || capacity <= 0 {
			continue
		}
		used, _ := finiteNumber(w["used"])
		o.Buckets = append(o.Buckets, Bucket{Label: axis.window, Window: axis.window, UsedPct: Pct(100 * used / capacity), ResetsAt: resetAtValue(w["resetAt"]), Scope: Scope{Kind: "account"}, Horizon: horizon(axis.window)})
	}
	if credits := object(data["credits"]); len(credits) > 0 {
		monthly := creditAmount(credits["monthlyCredits"])
		purchased := creditAmount(credits["purchasedCredits"])
		free := creditAmount(credits["freeCredits"])
		remaining := monthly + purchased + free
		o.Plan = str(credits["planId"])
		if total, ok := commandCodeCreditTotal(o.Plan, monthly, purchased, free); ok && total > 0 {
			o.Buckets = append(o.Buckets, Bucket{Label: "30d", Window: "30d", UsedPct: Pct(100 * (total - remaining) / total), Scope: Scope{Kind: "account"}, Horizon: "month", Note: fmt.Sprintf("credits remaining %.2f of %.2f USD", remaining, total)})
		} else if remaining > 0 {
			// A balance with no plan ceiling is money, not a percentage.
			o.Buckets = append(o.Buckets, Bucket{Label: "credits", Window: "credit", Scope: Scope{Kind: "account"}, Note: fmt.Sprintf("credits remaining %.2f USD", remaining)})
		}
	}
	if len(o.Buckets) == 0 {
		o.Error = "usage response has no recognized windows"
		o.ErrorKind = "parse_error"
	}
	return o
}

func commandCodeCreditTotal(plan string, monthly, purchased, free float64) (float64, bool) {
	plans := map[string]float64{
		"individual-goat":     70,
		"individual-provider": 15,
		"individual-pro-v1":   80,
		"individual-pro":      30,
		"individual-max":      150,
		"individual-ultra":    300,
		"individual-go":       10,
		"teams-pro":           40,
	}
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(plan)), "_", "-")
	best := ""
	for name := range plans {
		if strings.HasPrefix(normalized, name) && len(name) > len(best) {
			best = name
		}
	}
	if best == "" {
		return 0, false
	}
	return math.Max(plans[best], monthly) + purchased + free, true
}

func creditAmount(v any) float64 {
	n, ok := finiteNumber(v)
	if !ok || n < 0 {
		return 0
	}
	return n
}

func resetAtValue(v any) string {
	switch x := v.(type) {
	case float64:
		return epochTime(x)
	case json.Number:
		n, e := x.Float64()
		if e != nil {
			return ""
		}
		return epochTime(n)
	case int64:
		return epochTime(float64(x))
	case int:
		return epochTime(float64(x))
	case string:
		text := strings.TrimSpace(x)
		if text == "" {
			return ""
		}
		if at, e := time.Parse(time.RFC3339Nano, text); e == nil {
			return at.UTC().Format(time.RFC3339Nano)
		}
		if n, e := strconv.ParseFloat(text, 64); e == nil {
			return epochTime(n)
		}
		if at, e := http.ParseTime(text); e == nil {
			return at.UTC().Format(time.RFC3339Nano)
		}
	}
	return ""
}

func epochTime(n float64) string {
	if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return ""
	}
	if n > 1e12 {
		return time.UnixMilli(int64(n)).UTC().Format(time.RFC3339Nano)
	}
	return time.Unix(int64(n), 0).UTC().Format(time.RFC3339Nano)
}

func validated(p *float64) *float64 {
	if !validPct(p) {
		return nil
	}
	return p
}

type codexWindow struct {
	Used    *float64 `json:"used_percent"`
	Seconds float64  `json:"limit_window_seconds"`
	Reset   float64  `json:"reset_at"`
}

func (w codexWindow) bucket(scope Scope) Bucket {
	label := fmt.Sprintf("%gs", w.Seconds)
	switch {
	case w.Seconds >= 28*86400:
		label = "30d"
	case w.Seconds >= 6*86400:
		label = "7d"
	case w.Seconds >= 20*3600:
		label = "1d"
	case w.Seconds >= 4*3600 && w.Seconds <= 6*3600:
		label = "5h"
	}
	reset := ""
	if w.Reset > 0 {
		reset = time.UnixMilli(int64(w.Reset * 1000)).UTC().Format(time.RFC3339Nano)
	}
	return Bucket{Label: label, Window: label, UsedPct: validated(w.Used), ResetsAt: reset, Scope: scope, Horizon: horizon(label)}
}
func readObject(path string) map[string]any {
	var data map[string]any
	_ = ReadJSON(path, &data)
	return data
}
func object(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
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
					oauth = chooseOAuth(oauth, object(data["claudeAiOauth"]), time.Now())
				}
			}
		}
		token := str(oauth["accessToken"])
		if token == "" {
			return Observation{ID: "claude", Error: "credentials unavailable", ErrorKind: "credentials"}
		}
		if expires, ok := finiteNumber(oauth["expiresAt"]); ok && expires/1000 < float64(time.Now().Unix()) {
			return Observation{ID: "claude", Error: "token expired", ErrorKind: "token_expired"}
		}
		o := (HTTPProvider{ID: "claude", URL: "https://api.anthropic.com/api/oauth/usage", Headers: map[string]string{"Authorization": "Bearer " + token, "anthropic-beta": "oauth-2025-04-20"}, RateLimitWaitS: 300}).Fetch(ctx)
		acct := object(readObject(identityPath)["oauthAccount"])
		id := str(acct["accountUuid"])
		if id != "" && shareable {
			o.AccountFP = fingerprint("claude-account", id)
			o.AccountFPKind = "account"
		}
		return o
	})
	result["codex"] = ProviderFunc(func(ctx context.Context) Observation {
		dir := selectors["CODEX_HOME"]
		if dir == "" {
			dir = os.Getenv("CODEX_HOME")
		}
		if dir == "" {
			dir = filepath.Join(home, ".codex")
		}
		tokens := object(readObject(filepath.Join(dir, "auth.json"))["tokens"])
		token := str(tokens["access_token"])
		if token == "" {
			return Observation{ID: "codex", Error: "credentials unavailable", ErrorKind: "credentials"}
		}
		headers := map[string]string{"Authorization": "Bearer " + token}
		acct := str(tokens["account_id"])
		if acct != "" {
			headers["ChatGPT-Account-Id"] = acct
		}
		o := (HTTPProvider{ID: "codex", URL: "https://chatgpt.com/backend-api/wham/usage", Headers: headers}).Fetch(ctx)
		if acct != "" {
			o.PublishFP = fingerprint("codex-account", acct)
			o.PublishFPKind = "account"
		}
		return o
	})
	result["clinepass"] = ProviderFunc(func(ctx context.Context) Observation {
		token := clineToken(home)
		if token == "" {
			return Observation{ID: "clinepass", Error: "credentials unavailable", ErrorKind: "credentials"}
		}
		return (HTTPProvider{ID: "clinepass", URL: "https://api.cline.bot/api/v1/users/me/plan/usage-limits", Headers: map[string]string{"Authorization": "Bearer " + token}}).Fetch(ctx)
	})
	result["commandcode"] = ProviderFunc(func(ctx context.Context) Observation {
		return fetchCommandCode(ctx, commandCodeToken(home), nil, time.Now)
	})
	for _, pool := range []string{"devin", "grok", "kimi", "kiro"} {
		result[pool] = PTYProvider{ID: pool, Binary: ResolveBinary(pool, cfg), Timeout: 35 * time.Second, Env: selectors}
	}
	return result
}

func commandCodeToken(home string) string {
	if token := firstEnv("COMMAND_CODE_API_KEY", "COMMANDCODE_API_KEY"); token != "" {
		return token
	}
	return strings.TrimSpace(str(readObject(filepath.Join(home, ".commandcode", "auth.json"))["apiKey"]))
}

func clineToken(home string) string {
	if token := firstEnv("CLINE_API_KEY", "CLINEPASS_API_KEY"); token != "" {
		return token
	}
	providers := object(readObject(filepath.Join(home, ".cline", "data", "settings", "providers.json"))["providers"])
	for _, name := range []string{"cline-pass", "cline"} {
		settings := object(providers[name])["settings"]
		token := strings.TrimSpace(str(object(object(settings)["auth"])["accessToken"]))
		if token != "" {
			return token
		}
	}
	if token := str(object(readObject(filepath.Join(dataBase(), "opencode", "auth.json"))["cline-pass"])["key"]); token != "" {
		return token
	}
	b, _ := os.ReadFile(filepath.Join(configBase(), "cline", "api-key"))
	return strings.TrimSpace(string(b))
}

func claudeLocations(home string, selectors map[string]string) (credential, identity, service string, shareable bool) {
	ccd := selectors["CLAUDE_CONFIG_DIR"]
	if ccd == "" {
		ccd = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	secure, hasSecure := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	dir := ccd
	if hasSecure {
		dir = secure
	}
	if dir == "" {
		dir = filepath.Join(home, ".claude")
	}
	credential = filepath.Join(dir, ".credentials.json")
	identity = filepath.Join(home, ".claude.json")
	if ccd != "" {
		identity = filepath.Join(ccd, ".claude.json")
	}
	contextName := ccd
	if hasSecure {
		contextName = secure
	}
	service = "Claude Code-credentials"
	if contextName != "" {
		sum := sha256.Sum256([]byte(norm.NFC.String(contextName)))
		service += "-" + hex.EncodeToString(sum[:4])
	}
	shareable = !hasSecure || secure == ccd
	return
}
func chooseOAuth(file, keychain map[string]any, now time.Time) map[string]any {
	if str(file["accessToken"]) == "" {
		return keychain
	}
	if str(keychain["accessToken"]) == "" {
		return file
	}
	f, fok := finiteNumber(file["expiresAt"])
	k, kok := finiteNumber(keychain["expiresAt"])
	expiredF := fok && f/1000 <= float64(now.Unix())
	expiredK := kok && k/1000 <= float64(now.Unix())
	if expiredF != expiredK {
		if expiredF {
			return keychain
		}
		return file
	}
	if fok && kok && k > f {
		return keychain
	}
	return file
}

func finiteNumber(value any) (float64, bool) {
	var n float64
	switch v := value.(type) {
	case float64:
		n = v
	case json.Number:
		var e error
		n, e = v.Float64()
		if e != nil {
			return 0, false
		}
	case int64:
		n = float64(v)
	case int:
		n = float64(v)
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}
