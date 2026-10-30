package pacelever

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

func commandCodeToken(home string) string {
	if token := firstEnv("COMMAND_CODE_API_KEY", "COMMANDCODE_API_KEY"); token != "" {
		return token
	}
	return strings.TrimSpace(str(readObject(filepath.Join(home, ".commandcode", "auth.json"))["apiKey"]))
}

func fetchCommandCode(ctx context.Context, token string, client HTTPDoer, now func() time.Time) Observation {
	if token == "" {
		return Observation{ID: "commandcode", Error: "credentials unavailable", ErrorKind: ErrKindCredentials}
	}
	// The API's edge rejects generic clients; use the CLI's own user agent.
	headers := map[string]string{"Authorization": "Bearer " + token, "User-Agent": "cmd/1.88.1"}
	who, e := doJSON(ctx, client, "GET", "https://api.commandcode.ai/alpha/whoami?limits=1", headers, nil)
	if e != nil {
		return Observation{ID: "commandcode", Error: "usage request failed", ErrorKind: ErrKindNetwork}
	}
	if who.Status != 200 {
		return commandCodeStatus(who.Status, who.Header, who.ParseError, now())
	}
	if who.ParseError {
		return Observation{ID: "commandcode", Error: "invalid usage JSON", ErrorKind: ErrKindParse}
	}
	identity := object(who.Body["data"])
	if len(identity) == 0 {
		identity = who.Body
	}
	if success, ok := identity["success"].(bool); ok && !success {
		return Observation{ID: "commandcode", Error: "commandcode rejected the request", ErrorKind: ErrKindHTTP}
	}
	orgID := str(object(identity["org"])["id"])
	endpoint := "https://api.commandcode.ai/alpha/billing/credits"
	if orgID != "" {
		endpoint += "?orgId=" + url.QueryEscape(orgID)
	}
	billing, e := doJSON(ctx, client, "GET", endpoint, headers, nil)
	if e != nil {
		return Observation{ID: "commandcode", Error: "usage request failed", ErrorKind: ErrKindNetwork}
	}
	if billing.Status != 200 {
		return commandCodeStatus(billing.Status, billing.Header, billing.ParseError, now())
	}
	if billing.ParseError {
		return Observation{ID: "commandcode", Error: "invalid usage JSON", ErrorKind: ErrKindParse}
	}
	o := parseCommandCode(billing.Body)
	account := str(object(identity["user"])["id"])
	if account == "" {
		account = orgID
	}
	o.publishAccount(accountFP("commandcode", account))
	return o
}

func commandCodeStatus(status int, header http.Header, parseError bool, now time.Time) Observation {
	if status == 403 && parseError {
		o := statusObservation("commandcode", status, header, now, ErrKindEdgeRejected, "", 0)
		o.Error = "commandcode edge rejected the request; check the client user agent"
		return o
	}
	return statusObservation("commandcode", status, header, now, ErrKindAuthFailed, "commandcode authentication failed; run cmd login or set COMMAND_CODE_API_KEY", 0)
}

func parseCommandCode(raw map[string]any) Observation {
	o := Observation{ID: "commandcode", Source: SourceUsageAPI}
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
		b := accountBucket(axis.window, Pct(pct), resetAtValue(w["resetAt"]))
		b.Locked = locked
		o.Buckets = append(o.Buckets, b)
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
				b := accountBucket("30d", Pct(pct), "")
				b.Note = fmt.Sprintf("credits remaining %.2f of %.2f USD", remaining, total)
				o.Buckets = append(o.Buckets, b)
			} else if remaining <= 0 {
				// An unknown plan with no balance is exhausted, not absent.
				o.Buckets = append(o.Buckets, Bucket{Label: "credits", Window: "credit", UsedPct: Pct(100), Scope: Scope{Kind: ScopeAccount}, Locked: true, Note: "credits exhausted"})
			} else {
				// A positive balance with no plan ceiling is money, not a percentage.
				o.Buckets = append(o.Buckets, Bucket{Label: "credits", Window: "credit", Scope: Scope{Kind: ScopeAccount}, Note: fmt.Sprintf("credits remaining %.2f USD", remaining)})
			}
		}
	}
	if len(o.Buckets) == 0 {
		o.Error = "usage response has no recognized windows"
		o.ErrorKind = ErrKindParse
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
