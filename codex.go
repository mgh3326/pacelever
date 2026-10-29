package pacelever

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// parseCodexAPI reads the wham usage endpoint. Header and credits handling
// is ported from ClaudeBar (MIT, github.com/tddworks/ClaudeBar codex.json
// "api" mapping): x-codex-*-used-percent headers take precedence over body
// fields and the credits balance is read from a header or the credits block.
func parseCodexAPI(o *Observation, raw map[string]json.RawMessage, header http.Header, now time.Time) {
	_ = json.Unmarshal(raw["plan_type"], &o.Plan)
	var limit struct {
		Primary   *codexWindow `json:"primary_window"`
		Secondary *codexWindow `json:"secondary_window"`
	}
	_ = json.Unmarshal(raw["rate_limit"], &limit)
	headerNames := []string{"x-codex-primary-used-percent", "x-codex-secondary-used-percent"}
	for i, w := range []*codexWindow{limit.Primary, limit.Secondary} {
		if w != nil {
			b := w.bucket(Scope{Kind: ScopeAccount}, now)
			if pct := validated(headerFloat(header, headerNames[i])); pct != nil {
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
				o.Buckets = append(o.Buckets, w.bucket(Scope{Kind: ScopeModel, Name: x.Name}, now))
			}
		}
	}
	var credits struct {
		Has     bool `json:"has_credits"`
		Balance any  `json:"balance"`
	}
	_ = json.Unmarshal(raw["credits"], &credits)
	if credits.Has {
		balance := headerFloat(header, "x-codex-credits-balance")
		if balance == nil {
			balance = anyFloat(credits.Balance)
		}
		if balance != nil {
			// The API reports no limit; ClaudeBar measures the balance
			// against a fixed 1000.
			used := 100 - (*balance / 1000 * 100)
			o.Buckets = append(o.Buckets, Bucket{Label: "credits", Window: "credit", UsedPct: Pct(used), Scope: Scope{Kind: ScopeAccount}, Note: fmt.Sprintf("credits remaining %.2f of %.2f", *balance, 1000.0)})
		}
	}
}

// fetchCodex tries the app-server JSON-RPC source first when it is strictly
// safer than the API — it writes no credentials and, with a file login already
// present, cannot open a browser sign-in — and falls back to the wham usage
// API on any RPC failure.
func fetchCodex(ctx context.Context, home string, selectors map[string]string, cfg Config, client HTTPDoer, now func() time.Time) Observation {
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
		return Observation{ID: "codex", Error: "credentials unavailable", ErrorKind: ErrKindCredentials}
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
	o := (HTTPProvider{ID: "codex", URL: "https://chatgpt.com/backend-api/wham/usage", Headers: headers, Client: client, Now: now}).Fetch(ctx)
	if acct != "" {
		o.PublishFP = fingerprint("codex-account", acct)
		o.PublishFPKind = "account"
	}
	return o
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
	return windowBucket(scope, label, validated(w.Used), reset)
}
