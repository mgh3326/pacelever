package pacelever

import (
	"context"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
)

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
		o := fetchDevinWeb(ctx, token, org, client, now)
		// Attest the organization captured by this provider, independently of
		// any later changes to the collector's request configuration.
		o.PublishFP, o.PublishFPKind = fingerprint("devin-account", org), "account"
		return o
	})
}

// fetchDevinWeb reads the organization quota from app.devin.ai. It is the web
// source from ClaudeBar (MIT, tddworks/ClaudeBar): the credential is limited to
// the DEVIN_BEARER_TOKEN environment variable plus the pools.devin.org_id
// setting, so no browser storage is read and nothing is written back.
func fetchDevinWeb(ctx context.Context, token, org string, client HTTPDoer, now func() time.Time) Observation {
	if token == "" || !devinOrgRE.MatchString(org) {
		return Observation{ID: "devin", Error: "credentials unavailable", ErrorKind: ErrKindCredentials}
	}
	endpoint := "https://app.devin.ai/api/" + url.PathEscape(org) + "/billing/quota/usage"
	resp, e := doJSON(ctx, client, "GET", endpoint, map[string]string{
		"Authorization": "Bearer " + token,
		"x-cog-org-id":  org,
	}, nil)
	if e != nil {
		return Observation{ID: "devin", Error: "usage request failed", ErrorKind: ErrKindNetwork}
	}
	if resp.Status != 200 {
		if resp.Status == 401 || resp.Status == 403 {
			return statusObservation("devin", resp.Status, resp.Header, now(), ErrKindAuthFailed, "devin authentication failed; set DEVIN_BEARER_TOKEN and pools.devin.org_id", 0)
		}
		return statusObservation("devin", resp.Status, resp.Header, now(), "", "", 0)
	}
	if resp.ParseError {
		return Observation{ID: "devin", Error: "invalid usage JSON", ErrorKind: ErrKindParse}
	}
	return parseDevin(resp.Body)
}

func parseDevin(raw map[string]any) Observation {
	o := Observation{ID: "devin", Source: "usage-api"}
	nested := object(raw["quota_usage"])
	if raw["hide_daily_quota"] != true {
		if used, reset, ok := devinWindow(raw["daily_percentage"], raw["daily_reset_at"], nested["daily_quota"]); ok {
			o.Buckets = append(o.Buckets, accountBucket("1d", Pct(used), reset))
		}
	}
	if used, reset, ok := devinWindow(raw["weekly_percentage"], raw["weekly_reset_at"], nested["weekly_quota"]); ok {
		o.Buckets = append(o.Buckets, accountBucket("7d", Pct(used), reset))
	}
	o.Plan = devinPlan(raw)
	if len(o.Buckets) == 0 {
		o.Error = "usage response has no recognized windows"
		o.ErrorKind = ErrKindParse
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
	if remaining, ok := looseNumber(nested["remaining_percent"]); ok {
		remaining = devinRemainingPercent(remaining)
		if remaining < 0 || remaining > 100 {
			return 0, "", false
		}
		return 100 - remaining, reset, true
	}
	if limit, ok := looseNumber(nested["limit"]); ok && limit > 0 {
		count, hasCount := looseNumber(nested["used"])
		if !hasCount {
			if remaining, ok := looseNumber(nested["remaining"]); ok {
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
	n, ok := looseNumber(v)
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
