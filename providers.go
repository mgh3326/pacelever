package pacelever

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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
		// Header and credits handling ported from ClaudeBar (MIT,
		// github.com/tddworks/ClaudeBar codex.json "api" mapping):
		// x-codex-*-used-percent headers take precedence over body fields and
		// the credits balance is read from a header or the credits block.
		_ = json.Unmarshal(raw["plan_type"], &o.Plan)
		var limit struct {
			Primary   *codexWindow `json:"primary_window"`
			Secondary *codexWindow `json:"secondary_window"`
		}
		_ = json.Unmarshal(raw["rate_limit"], &limit)
		headerNames := []string{"x-codex-primary-used-percent", "x-codex-secondary-used-percent"}
		for i, w := range []*codexWindow{limit.Primary, limit.Secondary} {
			if w != nil {
				b := w.bucket(Scope{Kind: "account"}, p.now())
				if pct := validated(headerFloat(resp.Header, headerNames[i])); pct != nil {
					b.UsedPct = pct
				}
				o.Buckets = append(o.Buckets, b)
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
					o.Buckets = append(o.Buckets, w.bucket(Scope{Kind: "model", Name: x.Name}, p.now()))
				}
			}
		}
		var credits struct {
			Has     bool `json:"has_credits"`
			Balance any  `json:"balance"`
		}
		_ = json.Unmarshal(raw["credits"], &credits)
		if credits.Has {
			balance := headerFloat(resp.Header, "x-codex-credits-balance")
			if balance == nil {
				balance = anyFloat(credits.Balance)
			}
			if balance != nil {
				// The API reports no limit; ClaudeBar measures the balance
				// against a fixed 1000.
				used := 100 - (*balance / 1000 * 100)
				o.Buckets = append(o.Buckets, Bucket{Label: "credits", Window: "credit", UsedPct: Pct(used), Scope: Scope{Kind: "account"}, Note: fmt.Sprintf("credits remaining %.2f of %.2f", *balance, 1000.0)})
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
	// Decode every status so a caller can tell an API error from an HTML edge
	// rejection. The body is never retained in an observation or an error.
	if e = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out.Body); e != nil {
		out.ParseError = true
	}
	return out, nil
}

func commandCodeStatus(status int, header http.Header, parseError bool, now time.Time) Observation {
	if status == 403 && parseError {
		o := statusObservation("commandcode", status, header, now, "edge_rejected", "", 0)
		o.Error = "commandcode edge rejected the request; check the client user agent"
		return o
	}
	return statusObservation("commandcode", status, header, now, "auth_failed", "commandcode authentication failed; run cmd login or set COMMAND_CODE_API_KEY", 0)
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
		return commandCodeStatus(who.Status, who.Header, who.ParseError, now())
	}
	if who.ParseError {
		return Observation{ID: "commandcode", Error: "invalid usage JSON", ErrorKind: "parse_error"}
	}
	identity := object(who.Body["data"])
	if len(identity) == 0 {
		identity = who.Body
	}
	if success, ok := identity["success"].(bool); ok && !success {
		return Observation{ID: "commandcode", Error: "commandcode rejected the request", ErrorKind: "http"}
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
		return commandCodeStatus(billing.Status, billing.Header, billing.ParseError, now())
	}
	if billing.ParseError {
		return Observation{ID: "commandcode", Error: "invalid usage JSON", ErrorKind: "parse_error"}
	}
	o := parseCommandCode(billing.Body)
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

func parseCommandCode(raw map[string]any) Observation {
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
		used, usedOK := finiteNumber(w["used"])
		if !usedOK || used < 0 {
			// A malformed used value must never read as an unused window.
			continue
		}
		pct, locked := 100*used/capacity, false
		if pct > 100 {
			// A window past its cap is exhausted, not an out-of-range percentage.
			pct, locked = 100, true
		}
		if !validPct(&pct) {
			continue
		}
		o.Buckets = append(o.Buckets, Bucket{Label: axis.window, Window: axis.window, UsedPct: Pct(pct), ResetsAt: resetAtValue(w["resetAt"]), Scope: Scope{Kind: "account"}, Horizon: horizon(axis.window), Locked: locked})
	}
	if credits := object(data["credits"]); len(credits) > 0 {
		o.Plan = str(credits["planId"])
		monthly, monthlyOK := finiteNumber(credits["monthlyCredits"])
		if monthlyOK {
			if monthly < 0 {
				monthly = 0
			}
			purchased := creditAmount(credits["purchasedCredits"])
			free := creditAmount(credits["freeCredits"])
			remaining := monthly + purchased + free
			if total, ok := commandCodeCreditTotal(o.Plan, monthly, purchased, free); ok && total > 0 {
				pct := 100 * (total - remaining) / total
				if pct < 0 {
					pct = 0
				}
				if pct > 100 {
					pct = 100
				}
				o.Buckets = append(o.Buckets, Bucket{Label: "30d", Window: "30d", UsedPct: Pct(pct), Scope: Scope{Kind: "account"}, Horizon: "month", Note: fmt.Sprintf("credits remaining %.2f of %.2f USD", remaining, total)})
			} else if remaining <= 0 {
				// An unknown plan with no balance is exhausted, not absent.
				o.Buckets = append(o.Buckets, Bucket{Label: "credits", Window: "credit", UsedPct: Pct(100), Scope: Scope{Kind: "account"}, Locked: true, Note: "credits exhausted"})
			} else {
				// A positive balance with no plan ceiling is money, not a percentage.
				o.Buckets = append(o.Buckets, Bucket{Label: "credits", Window: "credit", Scope: Scope{Kind: "account"}, Note: fmt.Sprintf("credits remaining %.2f USD", remaining)})
			}
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

func numericValue(v any) float64 {
	n, _ := finiteNumber(v)
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
	if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 || n > 9.2e18 {
		return ""
	}
	var at time.Time
	switch {
	case n >= 1e14:
		at = time.UnixMicro(int64(n))
	case n >= 1e11:
		at = time.UnixMilli(int64(n))
	default:
		at = time.Unix(int64(n), 0)
	}
	at = at.UTC()
	// A quota reset outside this range is a malformed unit, not a real time.
	if at.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || at.After(time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)) {
		return ""
	}
	return at.Format(time.RFC3339Nano)
}

// grokRefreshSkew is the window in which a login is treated as due before it
// actually expires (ClaudeBar's `dueWhen.skew`).
const grokRefreshSkew = 300 * time.Second

func fetchGrok(ctx context.Context, token string, client HTTPDoer, now func() time.Time) Observation {
	return fetchGrokRecord(ctx, grokRecord{Token: token}, client, now)
}

// fetchGrokRecord reads Grok's credits with the login selected from auth.json.
// Its OAuth2 refresh is ported from ClaudeBar (MIT, tddworks/ClaudeBar —
// Resources/Providers/grok.json and Internal/Lookup/OAuth2Refresher.swift): a
// login that has expired or expires within the 300 s skew, or that the API
// refuses, is traded for a new one at the login's own OIDC issuer. The renewed
// token lives in memory only: this never writes the CLI's auth file, so a
// concurrent `grok` process is not raced and a refresh it performs is kept.
func fetchGrokRecord(ctx context.Context, record grokRecord, client HTTPDoer, now func() time.Time) Observation {
	if strings.TrimSpace(record.Token) == "" {
		return Observation{ID: "grok", Error: "credentials unavailable", ErrorKind: "credentials"}
	}
	refreshed, refreshTried := false, false
	if grokRefreshDue(record, now()) {
		refreshTried = true
		next, failure, ok := grokRefresh(ctx, record, client, now)
		switch {
		case ok:
			record, refreshed = next, true
		case record.Expires.After(now()):
			// Due by the skew but still valid: a token endpoint that cannot
			// answer must not cost a reading the current login can still give.
		default:
			return failure
		}
	}
	o := grokBilling(ctx, record.Token, client, now)
	if o.ErrorKind == "auth_failed" && !refreshed && !refreshTried && record.refreshable() {
		next, failure, ok := grokRefresh(ctx, record, client, now)
		if !ok {
			return failure
		}
		record = next
		o = grokBilling(ctx, record.Token, client, now)
	}
	if o.Error != "" {
		return o
	}
	// The tier is optional and its own short step: any failure or absence keeps
	// the billing plan and never turns a measured reading into an error.
	if plan := grokSettingsPlan(ctx, record.Token, client); plan != "" {
		o.Plan = plan
	}
	return o
}

// grokRefreshDue mirrors ClaudeBar's `dueWhen` (iso8601, 300 s skew). A login
// with no expiry is not due by itself — `missingIsDue` is false.
func grokRefreshDue(record grokRecord, now time.Time) bool {
	return record.refreshable() && record.HasExpiry && !record.Expires.After(now.Add(grokRefreshSkew))
}

func grokBilling(ctx context.Context, token string, client HTTPDoer, now func() time.Time) Observation {
	resp, e := doJSON(ctx, client, "GET", "https://cli-chat-proxy.grok.com/v1/billing?format=credits", map[string]string{"Authorization": "Bearer " + token, "x-grok-client-mode": "build"}, nil)
	if e != nil {
		return Observation{ID: "grok", Error: "usage request failed", ErrorKind: "network"}
	}
	if resp.Status != 200 {
		return statusObservation("grok", resp.Status, resp.Header, now(), "auth_failed", "grok authentication failed; run grok login", 0)
	}
	if resp.ParseError {
		return Observation{ID: "grok", Error: "invalid usage JSON", ErrorKind: "parse_error"}
	}
	return parseGrok(resp.Body)
}

func grokSettingsPlan(ctx context.Context, token string, client HTTPDoer) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, e := doJSON(ctx, client, "GET", "https://cli-chat-proxy.grok.com/v1/settings", map[string]string{"Authorization": "Bearer " + token, "x-grok-client-mode": "build"}, nil)
	if e != nil || resp.Status != 200 || resp.ParseError {
		return ""
	}
	return grokPlanName(str(resp.Body["subscription_tier_display"]))
}

// grokRefresh trades the refresh token for a new access token (RFC 6749 §6).
// It returns the renewed record in memory; nothing is written back to the
// login file. Failures carry no response body, URL or credential.
func grokRefresh(ctx context.Context, record grokRecord, client HTTPDoer, now func() time.Time) (grokRecord, Observation, bool) {
	fail := func(message, kind string) (grokRecord, Observation, bool) {
		return record, Observation{ID: "grok", Error: message, ErrorKind: kind}, false
	}
	if !record.refreshable() {
		return fail("grok authentication failed; run grok login", "auth_failed")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {record.RefreshToken}}
	if record.ClientID != "" {
		form.Set("client_id", record.ClientID)
	}
	resp, e := doJSON(ctx, client, "POST", record.Issuer+"/oauth2/token", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, []byte(form.Encode()))
	if e != nil {
		return fail("grok token refresh failed", "network")
	}
	if resp.Status == 400 || resp.Status == 401 {
		return fail("grok authentication failed; run grok login", "auth_failed")
	}
	if resp.Status != 200 {
		return fail("grok token refresh failed", "http")
	}
	token := strings.TrimSpace(str(resp.Body["access_token"]))
	if resp.ParseError || token == "" {
		return fail("grok token refresh returned no access token", "parse_error")
	}
	renewed := record
	renewed.Token = token
	// An empty refresh token in the answer never replaces the saved one.
	if next := strings.TrimSpace(str(resp.Body["refresh_token"])); next != "" {
		renewed.RefreshToken = next
	}
	if seconds, ok := grokAmount(resp.Body["expires_in"]); ok && seconds > 0 && seconds <= 10*365*24*3600 {
		renewed.Expires, renewed.HasExpiry = now().Add(time.Duration(seconds*float64(time.Second))), true
	} else {
		// A renewed token with no stated lifetime is used until the API refuses
		// it; keeping the old expiry would make every probe refresh again.
		renewed.Expires, renewed.HasExpiry = time.Time{}, false
	}
	return renewed, Observation{}, true
}

// grokPlanName names the tier the way the dashboard does; an unknown string
// stays as written and an empty one stays empty.
func grokPlanName(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	var compact strings.Builder
	for _, r := range strings.ToLower(text) {
		if r >= 'a' && r <= 'z' {
			compact.WriteRune(r)
		}
	}
	switch compact.String() {
	case "supergrokheavy", "heavy":
		return "SuperGrok Heavy"
	case "supergrok":
		return "SuperGrok"
	}
	return text
}

// grokProductName drops the shared Grok prefix and splits camel case, so
// GrokBuild reads Build and SomeNewProduct reads Some New Product.
func grokProductName(product string) string {
	name := product
	if strings.HasPrefix(name, "Grok") && len(name) > 4 {
		name = name[4:]
	}
	var out strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' && i > 0 && name[i-1] >= 'a' && name[i-1] <= 'z' {
			out.WriteByte(' ')
		}
		out.WriteByte(c)
	}
	return out.String()
}

func parseGrok(raw map[string]any) Observation {
	o := Observation{ID: "grok", Source: "usage-api"}
	config := object(raw["config"])
	if len(config) == 0 {
		config = raw
	}
	used, ok := finiteNumber(config["creditUsagePercent"])
	if !ok || !validPct(&used) {
		return Observation{ID: "grok", Error: "usage response has no valid credit percentage", ErrorKind: "parse_error"}
	}
	period := object(config["currentPeriod"])
	kind := strings.ToUpper(str(period["type"]))
	window, label := "", "usage"
	switch {
	case strings.Contains(kind, "WEEKLY"):
		window, label = "7d", "7d"
	case strings.Contains(kind, "MONTHLY"):
		window, label = "30d", "30d"
	case strings.Contains(kind, "DAILY"):
		window, label = "1d", "1d"
	default:
		return Observation{ID: "grok", Error: "usage response has an unrecognized billing period", ErrorKind: "parse_error"}
	}
	// The current period's end, or else the billing period's end — never one
	// shape's start with the other's end.
	reset := resetAtValue(period["end"])
	if reset == "" {
		reset = resetAtValue(config["billingPeriodEnd"])
	}
	o.Plan = grokPlanName(str(config["subscriptionTier"]))
	if o.Plan == "" {
		o.Plan = grokPlanName(str(raw["subscriptionTier"]))
	}
	bucket := Bucket{Label: label, Window: window, UsedPct: Pct(used), ResetsAt: reset, Scope: Scope{Kind: "account"}, Horizon: horizon(window)}
	note := productNote(config["productUsage"])
	// Prepaid balance is money in cents, not a percentage; it rides the account
	// note so every bucket stays percentage-bearing and publishable shared.
	if balance, ok := grokAmount(object(config["prepaidBalance"])["val"]); ok && balance > 0 {
		if note != "" {
			note += "; "
		}
		note += fmt.Sprintf("prepaid balance %.2f USD", balance/100)
	}
	if note != "" {
		bucket.Note = note
	}
	o.Buckets = []Bucket{bucket}
	// A product's own quota is its own bucket: informational for the gate but
	// visible instead of folded into the account note.
	if items, ok := config["productUsage"].([]any); ok {
		for _, item := range items {
			entry := object(item)
			name := grokProductName(strings.TrimSpace(str(entry["product"])))
			share, shareOK := finiteNumber(entry["usagePercent"])
			if name == "" || !shareOK || !validPct(&share) {
				continue
			}
			o.Buckets = append(o.Buckets, Bucket{Label: name, Window: window, UsedPct: Pct(share), ResetsAt: reset, Scope: Scope{Kind: "model", Name: name}, Horizon: horizon(window)})
		}
	}
	// The cap's unit isn't stated (dollars or cents), so it stays a share. It
	// is not the account window, so it carries a model scope and never gates.
	if capacity, ok := finiteNumber(object(config["onDemandCap"])["val"]); ok && capacity > 0 {
		spent, spentOK := finiteNumber(object(config["onDemandUsed"])["val"])
		if !spentOK || spent < 0 {
			spent = 0
		}
		share := 100 * spent / capacity
		locked := share >= 100
		if share < 0 {
			share = 0
		}
		if share > 100 {
			share = 100
		}
		o.Buckets = append(o.Buckets, Bucket{Label: "On-Demand", Window: window, UsedPct: Pct(share), ResetsAt: reset, Scope: Scope{Kind: "model", Name: "On-Demand"}, Horizon: horizon(window), Locked: locked})
	}
	return o
}

// grokAmount accepts the number or numeric string Grok uses for money fields.
func grokAmount(v any) (float64, bool) {
	if n, ok := finiteNumber(v); ok {
		return n, true
	}
	text := strings.TrimSpace(str(v))
	if text == "" {
		return 0, false
	}
	n, e := strconv.ParseFloat(text, 64)
	if e != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
}

func productNote(v any) string {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		entry := object(item)
		name := str(entry["product"])
		if name == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %.1f%%", name, numericValue(entry["usagePercent"])))
	}
	if len(parts) == 0 {
		return ""
	}
	return "products: " + strings.Join(parts, ", ")
}

// devinOrgRE is the organization id shape app.devin.ai uses. Rejecting other
// values keeps a malformed setting out of the request path and headers.
var devinOrgRE = regexp.MustCompile(`^org[-_][A-Za-z0-9_-]+$`)

func devinWebProvider(cfg Config, client HTTPDoer, now func() time.Time) Provider {
	token := strings.TrimSpace(os.Getenv("DEVIN_BEARER_TOKEN"))
	org := strings.TrimSpace(cfg.Pools["devin"].OrgID)
	if token == "" || !devinOrgRE.MatchString(org) {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return ProviderFunc(func(ctx context.Context) Observation {
		return fetchDevinWeb(ctx, token, org, client, now)
	})
}

// fetchDevinWeb reads the organization quota from app.devin.ai. It is the web
// source from ClaudeBar (MIT, tddworks/ClaudeBar): the credential is limited to
// the DEVIN_BEARER_TOKEN environment variable plus the pools.devin.org_id
// setting, so no browser storage is read and nothing is written back.
func fetchDevinWeb(ctx context.Context, token, org string, client HTTPDoer, now func() time.Time) Observation {
	if token == "" || !devinOrgRE.MatchString(org) {
		return Observation{ID: "devin", Error: "credentials unavailable", ErrorKind: "credentials"}
	}
	endpoint := "https://app.devin.ai/api/" + url.PathEscape(org) + "/billing/quota/usage"
	resp, e := doJSON(ctx, client, "GET", endpoint, map[string]string{
		"Authorization": "Bearer " + token,
		"x-cog-org-id":  org,
	}, nil)
	if e != nil {
		return Observation{ID: "devin", Error: "usage request failed", ErrorKind: "network"}
	}
	if resp.Status != 200 {
		if resp.Status == 401 || resp.Status == 403 {
			return statusObservation("devin", resp.Status, resp.Header, now(), "auth_failed", "devin authentication failed; set DEVIN_BEARER_TOKEN and pools.devin.org_id", 0)
		}
		return statusObservation("devin", resp.Status, resp.Header, now(), "", "", 0)
	}
	if resp.ParseError {
		return Observation{ID: "devin", Error: "invalid usage JSON", ErrorKind: "parse_error"}
	}
	return parseDevin(resp.Body)
}

func parseDevin(raw map[string]any) Observation {
	o := Observation{ID: "devin", Source: "usage-api"}
	nested := object(raw["quota_usage"])
	if raw["hide_daily_quota"] != true {
		if used, reset, ok := devinWindow(raw["daily_percentage"], raw["daily_reset_at"], nested["daily_quota"]); ok {
			o.Buckets = append(o.Buckets, Bucket{Label: "1d", Window: "1d", UsedPct: Pct(used), ResetsAt: reset, Scope: Scope{Kind: "account"}, Horizon: horizon("1d")})
		}
	}
	if used, reset, ok := devinWindow(raw["weekly_percentage"], raw["weekly_reset_at"], nested["weekly_quota"]); ok {
		o.Buckets = append(o.Buckets, Bucket{Label: "7d", Window: "7d", UsedPct: Pct(used), ResetsAt: reset, Scope: Scope{Kind: "account"}, Horizon: horizon("7d")})
	}
	o.Plan = devinPlan(raw)
	if len(o.Buckets) == 0 {
		o.Error = "usage response has no recognized windows"
		o.ErrorKind = "parse_error"
	}
	return o
}

// devinWindow reads one window from either the flat percentage shape or the
// nested quota_usage shape. A fraction below one is a percentage, matching the
// API and ClaudeBar's mapper.
func devinWindow(percentage, resetValue, nestedValue any) (float64, string, bool) {
	if used, ok := devinUsedPercent(percentage); ok {
		return used, resetAtValue(resetValue), true
	}
	nested := object(nestedValue)
	if len(nested) == 0 {
		return 0, "", false
	}
	at := nested["reset_at"]
	if at == nil {
		at = nested["next_reset_at"]
	}
	reset := resetAtValue(at)
	percent := nested["used_percent"]
	if percent == nil {
		percent = nested["percentUsed"]
	}
	if used, ok := devinUsedPercent(percent); ok {
		return used, reset, true
	}
	if remaining, ok := devinNumber(nested["remaining_percent"]); ok {
		remaining = devinRemainingPercent(remaining)
		if remaining < 0 || remaining > 100 {
			return 0, "", false
		}
		return 100 - remaining, reset, true
	}
	if limit, ok := devinNumber(nested["limit"]); ok && limit > 0 {
		count, hasCount := devinNumber(nested["used"])
		if !hasCount {
			if remaining, ok := devinNumber(nested["remaining"]); ok {
				count, hasCount = limit-remaining, true
			}
		}
		if hasCount && count >= 0 {
			return devinClamp(100 * count / limit), reset, true
		}
	}
	return 0, "", false
}

// devinUsedPercent applies the reference convention: a value below one is a
// fraction (0.12 is 12%), while exactly one is one percent. devinRemainingPercent
// is intentionally inclusive at one, matching ClaudeBar's devin-quota.js.
func devinUsedPercent(v any) (float64, bool) {
	n, ok := devinNumber(v)
	if !ok || n < 0 {
		return 0, false
	}
	if n < 1 {
		n *= 100
	}
	return devinClamp(n), true
}

func devinRemainingPercent(n float64) float64 {
	if n <= 1 {
		return n * 100
	}
	return n
}

func devinNumber(v any) (float64, bool) {
	if n, ok := finiteNumber(v); ok {
		return n, true
	}
	text, ok := v.(string)
	if !ok {
		return 0, false
	}
	n, e := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if e != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
}

func devinClamp(n float64) float64 {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func devinPlan(raw map[string]any) string {
	for _, key := range []string{"plan_name", "planName", "plan", "tier", "subscription_tier"} {
		plan := strings.TrimSpace(str(raw[key]))
		if plan == "" {
			continue
		}
		runes := []rune(plan)
		runes[0] = unicode.ToUpper(runes[0])
		return string(runes)
	}
	return ""
}

func fetchAgy(ctx context.Context, token string, client HTTPDoer, now func() time.Time) Observation {
	if token == "" {
		return Observation{ID: "agy", Error: "credentials unavailable", ErrorKind: "credentials"}
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "User-Agent": "antigravity", "Content-Type": "application/json"}
	var last httpJSON
	gotStatus := false
	for _, base := range []string{"https://daily-cloudcode-pa.googleapis.com", "https://cloudcode-pa.googleapis.com"} {
		resp, e := doJSON(ctx, client, "POST", base+"/v1internal:retrieveUserQuotaSummary", headers, []byte("{}"))
		if e != nil {
			continue
		}
		last, gotStatus = resp, true
		if resp.Status == 401 || resp.Status == 403 {
			return statusObservation("agy", resp.Status, resp.Header, now(), "auth_failed", "antigravity authentication failed; run agy to refresh the login", 0)
		}
		if resp.Status == 200 && !resp.ParseError {
			return parseAgy(resp.Body, fetchAgyModels(ctx, headers, client, base))
		}
	}
	if gotStatus && last.Status == 200 {
		return Observation{ID: "agy", Error: "invalid usage JSON", ErrorKind: "parse_error"}
	}
	if gotStatus {
		o := statusObservation("agy", last.Status, last.Header, now(), "auth_failed", "antigravity authentication failed; run agy to refresh the login", 0)
		if last.Status >= 500 {
			// A server error is retryable, not a sign the login expired.
			o.ErrorKind = "network"
			o.Error = "antigravity temporarily unavailable"
		}
		return o
	}
	return Observation{ID: "agy", Error: "usage request failed", ErrorKind: "network"}
}

// fetchAgyModels reads the per-model quota list. The summary groups omit a
// metering group when it is not reported for the account; each model's
// quotaInfo still carries that group's session (5h) quota. Best effort: a
// failure keeps the summary-only observation. The answered host goes first.
func fetchAgyModels(ctx context.Context, headers map[string]string, client HTTPDoer, answered string) map[string]any {
	hosts := []string{answered}
	for _, base := range []string{"https://daily-cloudcode-pa.googleapis.com", "https://cloudcode-pa.googleapis.com"} {
		if base != answered {
			hosts = append(hosts, base)
		}
	}
	for _, base := range hosts {
		resp, e := doJSON(ctx, client, "POST", base+"/v1internal:fetchAvailableModels", headers, []byte("{}"))
		if e != nil || resp.Status != 200 || resp.ParseError {
			continue
		}
		root := resp.Body
		if response := object(root["response"]); len(response) > 0 {
			root = response
		}
		return object(root["models"])
	}
	return nil
}

func parseAgy(raw map[string]any, extraModels map[string]any) Observation {
	o := Observation{ID: "agy", Source: "usage-api"}
	root := raw
	if response := object(raw["response"]); len(response) > 0 {
		root = response
	}
	worst := map[string]Bucket{}
	add := func(group, window string, fraction float64, reset string) {
		// An unmappable identity is emitted as scope group/unknown: visible in
		// output but never matched by the gate, so it fails closed instead of
		// passing as an account-wide bucket.
		if group == "" {
			group = "unknown"
		}
		if window == "" || math.IsNaN(fraction) || fraction < 0 || fraction > 1 {
			return
		}
		h := horizon(window)
		if window == "unknown" {
			h = ""
		}
		bucket := Bucket{Label: window, Window: window, UsedPct: Pct(100 * (1 - fraction)), ResetsAt: reset, Scope: Scope{Kind: "group", Name: group}, Horizon: h}
		key := group + "\x00" + window
		if previous, ok := worst[key]; !ok || (previous.UsedPct != nil && bucket.UsedPct != nil && *bucket.UsedPct > *previous.UsedPct) {
			worst[key] = bucket
		}
	}
	for _, groupValue := range list(root["groups"]) {
		group := object(groupValue)
		display := str(group["displayName"]) + " " + str(group["description"])
		for _, value := range list(group["buckets"]) {
			bucket := object(value)
			fraction, ok := finiteNumber(bucket["remainingFraction"])
			if !ok {
				continue
			}
			id := str(bucket["bucketId"])
			add(agyGroup(id, display), agyWindow(str(bucket["window"]), id), fraction, resetAtValue(bucket["resetTime"]))
		}
	}
	addModels := func(models map[string]any) {
		for key, value := range models {
			entry := object(value)
			if entry["isInternal"] == true {
				continue
			}
			quota := object(entry["quotaInfo"])
			fraction, ok := finiteNumber(quota["remainingFraction"])
			if !ok {
				continue
			}
			window := agyWindow(str(quota["window"]), key)
			switch {
			case window != "":
			case strings.TrimSpace(str(quota["window"])) == "":
				// Per-model quotaInfo without a window is the session quota.
				window = "5h"
			default:
				// An explicit window we do not recognize stays unmetered
				// instead of masquerading as the session quota.
				window = "unknown"
			}
			add(agyGroup(key, str(entry["displayName"])), window, fraction, resetAtValue(quota["resetTime"]))
		}
	}
	if len(worst) == 0 {
		addModels(object(root["models"]))
	}
	addModels(extraModels)
	for _, group := range []string{"gemini", "3p", "unknown"} {
		for _, window := range []string{"5h", "7d", "30d", "1d", "unknown"} {
			if bucket, ok := worst[group+"\x00"+window]; ok {
				o.Buckets = append(o.Buckets, bucket)
			}
		}
	}
	for _, key := range []string{"planDaily", "plan"} {
		plan := object(raw[key])
		name := str(object(plan["paidTier"])["name"])
		if name == "" {
			name = str(object(plan["currentTier"])["name"])
		}
		if name != "" {
			o.Plan = strings.ToUpper(name)
			break
		}
	}
	if len(o.Buckets) == 0 {
		o.Error = "usage response has no recognized windows"
		o.ErrorKind = "parse_error"
	}
	return o
}

func agyWindow(window, bucketID string) string {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "weekly":
		return "7d"
	case "5h", "session":
		return "5h"
	case "daily":
		return "1d"
	}
	id := strings.ToLower(bucketID)
	switch {
	case strings.HasSuffix(id, "-weekly"):
		return "7d"
	case strings.HasSuffix(id, "-5h"):
		return "5h"
	}
	return ""
}

// agyGroup maps a bucket or model identity to the metered account group.
// A gemini model must never be gated on the Claude/GPT (3p) numbers.
func agyGroup(id, display string) string {
	identity := strings.ToLower(strings.TrimSpace(id))
	switch {
	case strings.HasPrefix(identity, "gemini"):
		return "gemini"
	case strings.HasPrefix(identity, "3p-"), strings.HasPrefix(identity, "3p_"):
		return "3p"
	}
	text := strings.ToLower(display + " " + identity)
	switch {
	case strings.Contains(text, "gemini"):
		return "gemini"
	case strings.Contains(text, "claude"), strings.Contains(text, "gpt"), strings.Contains(text, "oss"):
		return "3p"
	}
	return "unknown"
}

func list(v any) []any {
	if items, ok := v.([]any); ok {
		return items
	}
	return nil
}

func validated(p *float64) *float64 {
	if !validPct(p) {
		return nil
	}
	return p
}

type codexWindow struct {
	Used       *float64 `json:"used_percent"`
	Seconds    float64  `json:"limit_window_seconds"`
	Reset      float64  `json:"reset_at"`
	ResetAfter float64  `json:"reset_after_seconds"`
}

func codexWindowLabel(seconds float64) string {
	label := fmt.Sprintf("%gs", seconds)
	switch {
	case seconds >= 28*86400:
		label = "30d"
	case seconds >= 6*86400:
		label = "7d"
	case seconds >= 20*3600:
		label = "1d"
	case seconds >= 4*3600 && seconds <= 6*3600:
		label = "5h"
	}
	return label
}

func (w codexWindow) bucket(scope Scope, now time.Time) Bucket {
	label := codexWindowLabel(w.Seconds)
	reset := ""
	switch {
	case w.Reset > 0:
		reset = time.UnixMilli(int64(w.Reset * 1000)).UTC().Format(time.RFC3339Nano)
	case w.ResetAfter > 0:
		reset = now.Add(time.Duration(w.ResetAfter) * time.Second).UTC().Format(time.RFC3339Nano)
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
		return fetchCodex(ctx, home, selectors, cfg, nil)
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
	result["grok"] = ProviderFunc(func(ctx context.Context) Observation {
		record, ok := selectGrokRecord(grokRecords(home, selectors), cfg.Quota.Accounts["grok"], time.Now())
		if !ok {
			return Observation{ID: "grok", Error: "credentials unavailable", ErrorKind: "credentials"}
		}
		o := fetchGrokRecord(ctx, record, nil, time.Now)
		if record.Fingerprint != "" {
			o.PublishFP = record.Fingerprint
			o.PublishFPKind = "account"
		}
		return o
	})
	result["agy"] = newAgyProvider(home, cfg, nil)
	// Devin keeps its CLI probe for the per-model Free tags; the read-only web
	// quota is tried first when its environment token and org setting are set.
	result["devin"] = PTYProvider{ID: "devin", Binary: ResolveBinary("devin", cfg), Timeout: 35 * time.Second, Env: selectors, Preferred: devinWebProvider(cfg, nil, time.Now)}
	for _, pool := range []string{"kimi", "kiro"} {
		result[pool] = PTYProvider{ID: pool, Binary: ResolveBinary(pool, cfg), Timeout: 35 * time.Second, Env: selectors}
	}
	return result
}

// fetchCodex tries the app-server JSON-RPC source first when it is strictly
// safer than the API — it writes no credentials and, with a file login already
// present, cannot open a browser sign-in — and falls back to the wham usage
// API on any RPC failure.
func fetchCodex(ctx context.Context, home string, selectors map[string]string, cfg Config, client HTTPDoer) Observation {
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
	acct := str(tokens["account_id"])
	if o, ok := fetchCodexRPC(ctx, home, cfg, dir); ok {
		if acct != "" {
			o.PublishFP = fingerprint("codex-account", acct)
			o.PublishFPKind = "account"
		}
		return o
	}
	headers := map[string]string{"Authorization": "Bearer " + token}
	if acct != "" {
		headers["ChatGPT-Account-Id"] = acct
	}
	o := (HTTPProvider{ID: "codex", URL: "https://chatgpt.com/backend-api/wham/usage", Headers: headers, Client: client}).Fetch(ctx)
	if acct != "" {
		o.PublishFP = fingerprint("codex-account", acct)
		o.PublishFPKind = "account"
	}
	return o
}

func commandCodeToken(home string) string {
	if token := firstEnv("COMMAND_CODE_API_KEY", "COMMANDCODE_API_KEY"); token != "" {
		return token
	}
	return strings.TrimSpace(str(readObject(filepath.Join(home, ".commandcode", "auth.json"))["apiKey"]))
}

type grokRecord struct {
	Token        string
	RefreshToken string
	Issuer       string
	ClientID     string
	Fingerprint  string
	Expires      time.Time
	HasExpiry    bool
	Created      time.Time
}

// refreshable is true when the login file holds everything the OIDC token
// endpoint needs. A key without both fields is a setup or API-key login and
// stays unusable once expired. A refresh token is never sent over plain HTTP.
func (r grokRecord) refreshable() bool {
	if r.RefreshToken == "" || r.Issuer == "" {
		return false
	}
	issuer, e := url.Parse(r.Issuer)
	return e == nil && issuer.Scheme == "https" && issuer.Host != ""
}

func grokAuthPath(home string, selectors map[string]string) string {
	dir := selectors["GROK_HOME"]
	if dir == "" {
		dir = os.Getenv("GROK_HOME")
	}
	if dir == "" {
		dir = filepath.Join(home, ".grok")
	}
	return filepath.Join(dir, "auth.json")
}

func grokRecords(home string, selectors map[string]string) []grokRecord {
	data := readObject(grokAuthPath(home, selectors))
	var out []grokRecord
	add := func(token string, fields map[string]any) {
		token = strings.TrimSpace(token)
		if token == "" {
			return
		}
		for _, existing := range out {
			if existing.Token == token {
				return
			}
		}
		record := grokRecord{
			Token:        token,
			RefreshToken: strings.TrimSpace(str(fields["refresh_token"])),
			Issuer:       strings.TrimRight(strings.TrimSpace(str(fields["oidc_issuer"])), "/"),
			ClientID:     strings.TrimSpace(str(fields["oidc_client_id"])),
		}
		if user := strings.TrimSpace(str(fields["user_id"])); user != "" {
			record.Fingerprint = fingerprint("grok-account", user)
		}
		if expires := parseOptionalTime(str(fields["expires_at"])); !expires.IsZero() {
			record.Expires, record.HasExpiry = expires, true
		}
		record.Created = parseOptionalTime(str(fields["create_time"]))
		out = append(out, record)
	}
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := data[key]
		record := object(value)
		token := strings.TrimSpace(str(record["key"]))
		if token == "" && strings.HasSuffix(key, ".key") {
			token = strings.TrimSpace(str(value))
		}
		add(token, record)
	}
	// A flat $.key shape is the fallback when no nested records exist.
	if len(out) == 0 {
		add(str(data["key"]), data)
	}
	return out
}

func parseOptionalTime(text string) time.Time {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}
	if at, e := time.Parse(time.RFC3339Nano, text); e == nil {
		return at.UTC()
	}
	if at, e := http.ParseTime(text); e == nil {
		return at.UTC()
	}
	return time.Time{}
}

// selectGrokRecord is deterministic: latest valid expiry, then latest creation,
// then token. An expired record survives only when it can be refreshed, and it
// ranks below every unexpired one, so a live login is never displaced by a
// stale record. A configured account fingerprint must match exactly.
func selectGrokRecord(records []grokRecord, wantFP string, now time.Time) (grokRecord, bool) {
	valid := make([]grokRecord, 0, len(records))
	for _, record := range records {
		if record.HasExpiry && !record.Expires.After(now) && !record.refreshable() {
			continue
		}
		valid = append(valid, record)
	}
	sort.SliceStable(valid, func(i, j int) bool {
		a, b := valid[i], valid[j]
		aExpired := a.HasExpiry && !a.Expires.After(now)
		bExpired := b.HasExpiry && !b.Expires.After(now)
		if aExpired != bExpired {
			return !aExpired
		}
		if a.HasExpiry != b.HasExpiry {
			return a.HasExpiry
		}
		if !a.Expires.Equal(b.Expires) {
			return a.Expires.After(b.Expires)
		}
		if !a.Created.Equal(b.Created) {
			return a.Created.After(b.Created)
		}
		return a.Token < b.Token
	})
	if wantFP != "" {
		for _, record := range valid {
			if record.Fingerprint == wantFP {
				return record, true
			}
		}
		return grokRecord{}, false
	}
	if len(valid) == 0 {
		return grokRecord{}, false
	}
	return valid[0], true
}

func agyToken(home string) string {
	for _, path := range []string{
		filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"),
		filepath.Join(configBase(), "antigravity-cli", "antigravity-oauth-token"),
	} {
		token := strings.TrimSpace(str(object(readObject(path)["token"])["access_token"]))
		if token != "" {
			return token
		}
	}
	return ""
}

// agySecrets is an optional credential fallback store. On macOS it is the
// login keychain entry the Antigravity CLI writes through go-keyring.
type agySecrets interface {
	agyToken(context.Context) string
}

// agyProvider measures Antigravity quota over HTTP. A refused token is re-armed
// once by running the CLI's model listing, which refreshes the OAuth file as a
// side effect; a shared lock keeps the refresh single-flight.
type agyProvider struct {
	client  HTTPDoer
	now     func() time.Time
	file    func() string
	secrets agySecrets
	wake    func(context.Context) error
	// wakeLock serializes the CLI refresh across collectors whose cache
	// directories differ (default runs and herdr events). Empty relies on
	// the caller's own serialization.
	wakeLock string
	// The app's local server source (agy_local.go): running processes,
	// their listening ports and the loopback client. localProcs nil means
	// no local source; localPorts nil skips the TLS-port fallback.
	localProcs func(context.Context) []agyLocalProc
	localPorts func(context.Context, int) []int
	localHTTP  HTTPDoer
}

func newAgyProvider(home string, cfg Config, client HTTPDoer) agyProvider {
	p := agyProvider{
		client:     client,
		now:        time.Now,
		file:       func() string { return agyToken(home) },
		localProcs: agyLocalProcesses,
		localPorts: agyListenPorts,
	}
	if runtime.GOOS == "darwin" {
		p.secrets = agyKeychain{}
	}
	if binary, e := exec.LookPath(ResolveBinary("agy", cfg)); e == nil {
		p.wake = agyWake(binary)
		p.wakeLock = filepath.Join(CacheDir(), "agy-wake.lock")
	}
	return p
}

func (p agyProvider) fileToken() string {
	if p.file == nil {
		return ""
	}
	return p.file()
}
func (p agyProvider) secretToken(ctx context.Context) string {
	if p.secrets == nil {
		return ""
	}
	return p.secrets.agyToken(ctx)
}
func (p agyProvider) Fetch(ctx context.Context) Observation {
	now := p.now
	if now == nil {
		now = time.Now
	}
	// The running app's own server answers without a stored credential, so
	// it is strictly safer than the token path and goes first; any local
	// failure falls back to today's cloud source unchanged.
	if o, ok := p.local(ctx); ok {
		return o
	}
	token := p.fileToken()
	usedSecret := false
	if token == "" {
		if token = p.secretToken(ctx); token != "" {
			usedSecret = true
		}
	}
	if token == "" {
		return Observation{ID: "agy", Error: "credentials unavailable", ErrorKind: "credentials"}
	}
	o := fetchAgy(ctx, token, p.client, now)
	if o.ErrorKind != "auth_failed" {
		return o
	}
	secretRefused := usedSecret
	switch {
	case p.wake == nil:
		o.Error = "antigravity authentication failed; the agy command is unavailable to refresh the login"
	default:
		fresh, freshSecret, e := p.refresh(ctx, token, usedSecret)
		if e != nil {
			o.Error = "antigravity authentication failed; `agy models` could not refresh the login"
			break
		}
		token, usedSecret = fresh, freshSecret
		if o = fetchAgy(ctx, token, p.client, now); o.ErrorKind != "auth_failed" {
			return o
		}
		if usedSecret {
			secretRefused = true
		}
	}
	// A file credential that is missing or still refused after the refresh
	// falls back to the keychain copy the CLI keeps. A refused fallback keeps
	// the refresh hint rather than masking it behind the same auth failure.
	if !secretRefused {
		if secret := p.secretToken(ctx); secret != "" && secret != token {
			if alt := fetchAgy(ctx, secret, p.client, now); alt.Error == "" {
				o = alt
			}
		}
	}
	return o
}

// refresh re-arms a refused token under a lock shared by collectors whose
// cache directories differ. A token another holder just refreshed is reused
// instead of waking the CLI again. It returns the token to retry with.
func (p agyProvider) refresh(ctx context.Context, token string, usedSecret bool) (string, bool, error) {
	// Re-read whichever store supplied the token: the wake or a concurrent
	// refresh may have replaced it in either one.
	latest := func() (string, bool) {
		if fresh := p.fileToken(); fresh != "" {
			return fresh, false
		}
		if usedSecret {
			if fresh := p.secretToken(ctx); fresh != "" {
				return fresh, true
			}
		}
		return token, usedSecret
	}
	if p.wakeLock == "" {
		if e := p.wake(ctx); e != nil {
			return token, usedSecret, e
		}
		next, secret := latest()
		return next, secret, nil
	}
	next, nextSecret := token, usedSecret
	e := WithFileLock(p.wakeLock, false, func() error {
		if fresh, secret := latest(); fresh != token {
			next, nextSecret = fresh, secret
			return nil
		}
		if e := p.wake(ctx); e != nil {
			return e
		}
		next, nextSecret = latest()
		return nil
	})
	return next, nextSecret, e
}

// agyWake re-arms the OAuth token with one model listing. The CLI output may
// carry account details, so it is discarded and never logged.
func agyWake(binary string) func(context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "models")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		return cmd.Run()
	}
}

type agyKeychain struct{}

func (agyKeychain) agyToken(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, "security", "find-generic-password", "-s", "gemini", "-a", "antigravity", "-w").Output()
	if e != nil {
		return ""
	}
	return agyEnvelopeToken(strings.TrimSpace(string(out)))
}

// agyEnvelopeToken unwraps the go-keyring envelope and reads
// $.token.access_token from the enclosed JSON.
func agyEnvelopeToken(raw string) string {
	if s := strings.TrimPrefix(raw, "go-keyring-base64:"); s != raw {
		decoded, e := base64.StdEncoding.DecodeString(s)
		if e != nil {
			return ""
		}
		raw = string(decoded)
	} else if s := strings.TrimPrefix(raw, "go-keyring-encoded:"); s != raw {
		decoded, e := hex.DecodeString(s)
		if e != nil {
			return ""
		}
		raw = string(decoded)
	}
	var data map[string]any
	if json.Unmarshal([]byte(raw), &data) != nil {
		return ""
	}
	return strings.TrimSpace(str(object(data["token"])["access_token"]))
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

// headerFloat parses a numeric response header; absent or unparsable is nil,
// never a guessed value.
func headerFloat(header http.Header, name string) *float64 {
	if header == nil {
		return nil
	}
	s := strings.TrimSpace(header.Get(name))
	if s == "" {
		return nil
	}
	f, e := strconv.ParseFloat(s, 64)
	if e != nil {
		return nil
	}
	return &f
}

// anyFloat accepts a JSON number or a numeric string (the codex credits
// balance has been observed as "0").
func anyFloat(value any) *float64 {
	if s, ok := value.(string); ok {
		f, e := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if e != nil {
			return nil
		}
		return &f
	}
	if n, ok := finiteNumber(value); ok {
		return &n
	}
	return nil
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
