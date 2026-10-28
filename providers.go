package pacelever

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		o.Error = fmt.Sprintf("usage HTTP %d", resp.StatusCode)
		o.ErrorKind = "http"
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			o.ErrorKind = "auth"
		}
		if resp.StatusCode == 429 {
			o.ErrorKind = "rate_limited"
		}
		return o
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
		if expires, ok := oauth["expiresAt"].(float64); ok && expires/1000 < float64(time.Now().Unix()) {
			return Observation{ID: "claude", Error: "token expired", ErrorKind: "token_expired"}
		}
		o := (HTTPProvider{ID: "claude", URL: "https://api.anthropic.com/api/oauth/usage", Headers: map[string]string{"Authorization": "Bearer " + token, "anthropic-beta": "oauth-2025-04-20"}}).Fetch(ctx)
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
		token := os.Getenv("CLINE_API_KEY")
		if token == "" {
			token = str(object(readObject(filepath.Join(dataBase(), "opencode", "auth.json"))["cline-pass"])["key"])
		}
		if token == "" {
			b, _ := os.ReadFile(filepath.Join(configBase(), "cline", "api-key"))
			token = strings.TrimSpace(string(b))
		}
		if token == "" {
			return Observation{ID: "clinepass", Error: "credentials unavailable", ErrorKind: "credentials"}
		}
		return (HTTPProvider{ID: "clinepass", URL: "https://api.cline.bot/api/v1/users/me/plan/usage-limits", Headers: map[string]string{"Authorization": "Bearer " + token}}).Fetch(ctx)
	})
	for _, pool := range []string{"devin", "grok", "kimi", "kiro", "commandcode"} {
		result[pool] = PTYProvider{ID: pool, Binary: ResolveBinary(pool, cfg), Timeout: 35 * time.Second, Env: selectors}
	}
	return result
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
	f, fok := file["expiresAt"].(float64)
	k, kok := keychain["expiresAt"].(float64)
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
