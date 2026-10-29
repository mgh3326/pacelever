package pacelever

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func responseWithHeader(code int, body string, header http.Header) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: header}
}

func pctClose(p *float64, want float64) bool {
	return p != nil && math.Abs(*p-want) < 1e-6
}

func mustJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var value map[string]any
	if e := json.Unmarshal([]byte(body), &value); e != nil {
		t.Fatal(e)
	}
	return value
}

func fixedNow() time.Time { return testNow }

func TestCommandCodeHTTP(t *testing.T) {
	who := `{"success":true,"user":{"id":"fixture-user"},"org":null}`
	credits := `{"credits":{"monthlyCredits":8.5,"purchasedCredits":0,"freeCredits":0,"planId":"individual-go"},"windowLimits":{"limited":true,"fiveHour":{"used":10,"cap":40,"resetAt":1770000000000},"weekly":{"used":50,"cap":200,"resetAt":1770500000}}}`
	var sawOrg string
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("User-Agent") != "cmd/1.88.1" {
			t.Fatalf("unexpected command code user agent")
		}
		switch r.URL.Path {
		case "/alpha/whoami":
			return response(200, who), nil
		case "/alpha/billing/credits":
			sawOrg = r.URL.Query().Get("orgId")
			return response(200, credits), nil
		}
		return response(404, `{}`), nil
	})
	o := fetchCommandCode(context.Background(), "fixture-key", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 3 {
		t.Fatalf("command code buckets %+v", o)
	}
	if sawOrg != "" {
		t.Fatal("empty organization sent an orgId")
	}
	if o.Buckets[0].Window != "5h" || *o.Buckets[0].UsedPct != 25 || o.Buckets[0].ResetsAt != "2026-02-02T02:40:00Z" {
		t.Fatalf("five-hour bucket %+v", o.Buckets[0])
	}
	if o.Buckets[1].Window != "7d" || *o.Buckets[1].UsedPct != 25 || o.Buckets[1].ResetsAt != "2026-02-07T21:33:20Z" {
		t.Fatalf("weekly bucket %+v", o.Buckets[1])
	}
	if o.Buckets[2].Window != "30d" || *o.Buckets[2].UsedPct != 15 {
		t.Fatalf("credit bucket %+v", o.Buckets[2])
	}
	if o.PublishFP == "" || o.PublishFPKind != "account" {
		t.Fatal("command code account fingerprint missing")
	}
}

func TestCommandCodeHTTPWrappedAndUnknownPlan(t *testing.T) {
	who := `{"data":{"user":{"id":"fixture-user"},"org":{"id":"team & org"}}}`
	credits := `{"data":{"credits":{"monthlyCredits":12.5,"planId":"mystery-tier"},"windowLimits":{"weekly":{"used":50,"cap":200,"resetAt":"2026-11-05T01:18:55Z"}}}}`
	var sawOrg string
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/alpha/whoami" {
			return response(200, who), nil
		}
		sawOrg = r.URL.Query().Get("orgId")
		return response(200, credits), nil
	})
	o := fetchCommandCode(context.Background(), "fixture-key", client, fixedNow)
	if sawOrg != "team & org" {
		t.Fatalf("organization not forwarded: %q", sawOrg)
	}
	if o.Error != "" || len(o.Buckets) != 2 || o.Buckets[1].Window != "credit" || o.Buckets[1].UsedPct != nil {
		t.Fatalf("unknown-plan credit bucket %+v", o)
	}
	if !strings.Contains(o.Buckets[1].Note, "12.50 USD") {
		t.Fatalf("credit note %q", o.Buckets[1].Note)
	}
}

func TestCommandCodeHTTPAuthFailureDoesNotLeakKey(t *testing.T) {
	sentinel := "do-not-print-this-key"
	client := doFunc(func(*http.Request) (*http.Response, error) { return response(401, sentinel), nil })
	o := fetchCommandCode(context.Background(), sentinel, client, fixedNow)
	if o.ErrorKind != "auth_failed" || !strings.Contains(o.Error, "cmd login") {
		t.Fatalf("auth failure %+v", o)
	}
	if strings.Contains(o.Error, sentinel) {
		t.Fatal("credential leaked into the error")
	}
}

func TestCommandCodeHTTPSkipsZeroCapacity(t *testing.T) {
	who := `{"success":true}`
	credits := `{"windowLimits":{"fiveHour":{"used":0,"cap":0,"resetAt":1770000000000},"weekly":{"used":50,"cap":200,"resetAt":1770500000000}}}`
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/alpha/whoami" {
			return response(200, who), nil
		}
		return response(200, credits), nil
	})
	o := fetchCommandCode(context.Background(), "fixture-key", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 1 || o.Buckets[0].Window != "7d" {
		t.Fatalf("zero cap bucket %+v", o)
	}
}

func TestCommandCodeHTTPSoftFailure(t *testing.T) {
	who := `{"success":false,"user":{"id":"fixture-user"},"org":null}`
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/alpha/whoami" {
			t.Fatal("credits requested after a soft failure")
		}
		return response(200, who), nil
	})
	o := fetchCommandCode(context.Background(), "fixture-key", client, fixedNow)
	if o.ErrorKind != "http" || !strings.Contains(o.Error, "rejected") {
		t.Fatalf("soft failure %+v", o)
	}
}

func TestCommandCodeHTTPMissingUsed(t *testing.T) {
	who := `{"success":true}`
	credits := `{"windowLimits":{"fiveHour":{"cap":40,"resetAt":1770000000},"weekly":{"used":"10","cap":200,"resetAt":1770500000}}}`
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/alpha/whoami" {
			return response(200, who), nil
		}
		return response(200, credits), nil
	})
	o := fetchCommandCode(context.Background(), "fixture-key", client, fixedNow)
	if o.Error == "" || o.ErrorKind != "parse_error" || len(o.Buckets) != 0 {
		t.Fatalf("malformed used admitted %+v", o)
	}
}

func TestCommandCodeHTTPUnknownPlanExhausted(t *testing.T) {
	who := `{"success":true}`
	credits := `{"credits":{"monthlyCredits":0,"planId":"mystery-tier"},"windowLimits":{"weekly":{"used":10,"cap":200,"resetAt":1770500000}}}`
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/alpha/whoami" {
			return response(200, who), nil
		}
		return response(200, credits), nil
	})
	o := fetchCommandCode(context.Background(), "fixture-key", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 2 {
		t.Fatalf("exhausted unknown plan %+v", o)
	}
	credit := o.Buckets[1]
	if credit.Window != "credit" || credit.UsedPct == nil || *credit.UsedPct != 100 || !credit.Locked {
		t.Fatalf("exhausted credit bucket %+v", credit)
	}
}

func TestCommandCodeHTTPMissingMonthlyCredits(t *testing.T) {
	who := `{"success":true}`
	credits := `{"credits":{"purchasedCredits":5,"planId":"individual-go"},"windowLimits":{"weekly":{"used":10,"cap":200,"resetAt":1770500000}}}`
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/alpha/whoami" {
			return response(200, who), nil
		}
		return response(200, credits), nil
	})
	o := fetchCommandCode(context.Background(), "fixture-key", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 1 || o.Buckets[0].Window != "7d" {
		t.Fatalf("missing monthlyCredits invented a credit bucket %+v", o)
	}
}

func TestCommandCodeHTTPNonJSON403(t *testing.T) {
	html := `<html><body>error 1010</body></html>`
	client := doFunc(func(*http.Request) (*http.Response, error) { return response(403, html), nil })
	o := fetchCommandCode(context.Background(), "fixture-key", client, fixedNow)
	if o.ErrorKind != "edge_rejected" || !strings.Contains(o.Error, "user agent") {
		t.Fatalf("non-JSON 403 %+v", o)
	}
	jsonBody := `{"error":"invalid token"}`
	client = doFunc(func(*http.Request) (*http.Response, error) { return response(403, jsonBody), nil })
	o = fetchCommandCode(context.Background(), "fixture-key", client, fixedNow)
	if o.ErrorKind != "auth_failed" || !strings.Contains(o.Error, "cmd login") {
		t.Fatalf("JSON 403 %+v", o)
	}
}

func TestCommandCodeRequiredWindows(t *testing.T) {
	windows := RequiredWindows("commandcode", "", false)
	if len(windows) != 2 || windows[0] != "5h" || windows[1] != "7d" {
		t.Fatalf("automatic windows %v", windows)
	}
	if manual := RequiredWindows("commandcode", "", true); len(manual) != 1 || manual[0] != "credit" {
		t.Fatalf("manual windows %v", manual)
	}
}

func TestCommandCodeStaleAcceptanceWithoutCreditReset(t *testing.T) {
	o := observation("commandcode", 10)
	o.Buckets = append(o.Buckets, Bucket{Label: "credits", Window: "credit", UsedPct: Pct(50), Scope: Scope{Kind: "account"}})
	o.Stale = true
	o.Error = "usage HTTP 429"
	o.ErrorKind = "rate_limited"
	o.AccountFPMatch = ptr(true)
	o.AgeS = ptr(60.0)
	o.FetchedAt = float64(testNow.Add(-time.Minute).Unix())
	d := Gate(Inputs{Now: testNow, Observations: []Observation{o}}, Request{Profile: "cmd-dsflash41", Effort: "high", Role: "worker"})
	if !d.OK || !d.StaleAccepted {
		t.Fatalf("stale command code rejected: %+v", d)
	}
	for _, w := range d.MissingWindows {
		if w == "30d" || w == "credit" {
			t.Fatalf("informational credit window required: %v", d.MissingWindows)
		}
	}
}

func TestCommandCodeHTTPSpikedWindow(t *testing.T) {
	who := `{"success":true}`
	credits := `{"windowLimits":{"fiveHour":{"used":44,"cap":40,"resetAt":1770000000},"weekly":{"used":-1,"cap":200,"resetAt":1770500000}}}`
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/alpha/whoami" {
			return response(200, who), nil
		}
		return response(200, credits), nil
	})
	o := fetchCommandCode(context.Background(), "fixture-key", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 1 {
		t.Fatalf("spiked window %+v", o)
	}
	if o.Buckets[0].Window != "5h" || o.Buckets[0].UsedPct == nil || *o.Buckets[0].UsedPct != 100 || !o.Buckets[0].Locked {
		t.Fatalf("clamped window %+v", o.Buckets[0])
	}
}

func TestEpochTimeUnits(t *testing.T) {
	want := "2026-02-02T02:40:00Z"
	for _, n := range []float64{1770000000, 1770000000000, 1770000000000000} {
		if got := epochTime(n); got != want {
			t.Fatalf("epoch %g = %q, want %q", n, got, want)
		}
	}
	if epochTime(1e18) != "" || epochTime(1e19) != "" {
		t.Fatal("absurd reset timestamp accepted")
	}
}

func TestGrokBilling(t *testing.T) {
	body := `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-22T19:58:08Z","end":"2026-10-29T19:58:08Z"},"creditUsagePercent":91.0,"productUsage":[{"product":"GrokBuild","usagePercent":5.0},{"product":"GrokChat","usagePercent":3.0}]}}`
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("x-grok-client-mode") != "build" {
			t.Fatal("grok client mode missing")
		}
		return response(200, body), nil
	})
	o := fetchGrok(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 1 {
		t.Fatalf("grok buckets %+v", o)
	}
	b := o.Buckets[0]
	if b.Window != "7d" || b.UsedPct == nil || *b.UsedPct != 91 || b.ResetsAt != "2026-10-29T19:58:08Z" {
		t.Fatalf("grok bucket %+v", b)
	}
	if !strings.Contains(b.Note, "GrokBuild 5.0%") {
		t.Fatalf("product note %q", b.Note)
	}
}

func TestGrokInvalidPercent(t *testing.T) {
	body := `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-10-29T19:58:08Z"},"creditUsagePercent":140}}`
	client := doFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })
	o := fetchGrok(context.Background(), "fixture-token", client, fixedNow)
	if o.ErrorKind != "parse_error" {
		t.Fatalf("invalid grok percent %+v", o)
	}
}

func TestGrokUnknownPeriod(t *testing.T) {
	body := `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_UNSPECIFIED","end":"2026-10-29T19:58:08Z"},"creditUsagePercent":10}}`
	client := doFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })
	o := fetchGrok(context.Background(), "fixture-token", client, fixedNow)
	if o.ErrorKind != "parse_error" {
		t.Fatalf("unknown grok period %+v", o)
	}
}

func TestGrokAuthFailure(t *testing.T) {
	client := doFunc(func(*http.Request) (*http.Response, error) { return response(403, `{}`), nil })
	o := fetchGrok(context.Background(), "fixture-token", client, fixedNow)
	if o.ErrorKind != "auth_failed" || !strings.Contains(o.Error, "grok login") {
		t.Fatalf("grok auth %+v", o)
	}
}

func TestAgyQuotaSummaryWeeklyAndFiveHour(t *testing.T) {
	body := `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.8,"resetTime":"2026-10-29T06:18:55Z"},{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.6,"resetTime":"2026-11-05T01:18:55Z"}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","window":"5h","remainingFraction":0.5,"resetTime":"2026-10-29T06:18:55Z"}]}]}`
	var hosts []string
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		if strings.HasSuffix(r.URL.Path, ":fetchAvailableModels") {
			return response(200, `{"models":{}}`), nil
		}
		return response(200, body), nil
	})
	o := fetchAgy(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 3 {
		t.Fatalf("agy buckets %+v", o)
	}
	if o.Buckets[0].Scope.Name != "gemini" || o.Buckets[0].Window != "5h" || !pctClose(o.Buckets[0].UsedPct, 20) {
		t.Fatalf("agy gemini five-hour %+v", o.Buckets[0])
	}
	if o.Buckets[1].Scope.Name != "gemini" || o.Buckets[1].Window != "7d" || !pctClose(o.Buckets[1].UsedPct, 40) {
		t.Fatalf("agy gemini weekly %+v", o.Buckets[1])
	}
	if o.Buckets[2].Scope.Name != "3p" || o.Buckets[2].Window != "5h" || !pctClose(o.Buckets[2].UsedPct, 50) {
		t.Fatalf("agy 3p five-hour %+v", o.Buckets[2])
	}
	if len(matching(o.Buckets, "gemini")) != 2 || len(matching(o.Buckets, "3p")) != 1 {
		t.Fatalf("agy group selection %+v", o.Buckets)
	}
	if len(hosts) != 2 || hosts[0] != "daily-cloudcode-pa.googleapis.com" || hosts[1] != "daily-cloudcode-pa.googleapis.com" {
		t.Fatalf("agy host order %v", hosts)
	}
}

func TestAgySummaryMissingGroupMergedFromModels(t *testing.T) {
	summary := `{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","window":"5h","remainingFraction":0.5,"resetTime":"2026-10-29T06:18:55Z"},{"bucketId":"3p-weekly","window":"weekly","remainingFraction":0.4,"resetTime":"2026-11-05T01:18:55Z"}]}]}`
	var paths []string
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, ":fetchAvailableModels") {
			return response(200, agyModelsBody), nil
		}
		return response(200, summary), nil
	})
	o := fetchAgy(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 5 {
		t.Fatalf("agy merged buckets %+v", o)
	}
	if o.Buckets[0].Scope.Name != "gemini" || o.Buckets[0].Window != "5h" || !pctClose(o.Buckets[0].UsedPct, 0.3521) {
		t.Fatalf("agy merged gemini five-hour %+v", o.Buckets[0])
	}
	if len(matching(o.Buckets, "gemini")) != 1 || len(matching(o.Buckets, "3p")) != 2 {
		t.Fatalf("agy merged group selection %+v", o.Buckets)
	}
	if len(matching(o.Buckets, "unknown")) != 2 {
		t.Fatalf("agy unmapped models not emitted: %+v", o.Buckets)
	}
	for _, b := range matching(o.Buckets, "unknown") {
		if b.Window == "monthly" || (b.Window == "5h" && pctClose(b.UsedPct, 10)) {
			t.Fatalf("unrecognized model window mislabeled: %+v", b)
		}
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], ":retrieveUserQuotaSummary") || !strings.HasSuffix(paths[1], ":fetchAvailableModels") {
		t.Fatalf("agy endpoint order %v", paths)
	}
	for _, b := range o.Buckets {
		if pctClose(b.UsedPct, 90) {
			t.Fatalf("internal model leaked: %+v", o.Buckets)
		}
	}
}

func TestAgyModelsOnlyCoverGroups(t *testing.T) {
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, ":fetchAvailableModels") {
			return response(200, agyModelsBody), nil
		}
		return response(200, `{"groups":[]}`), nil
	})
	o := fetchAgy(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" || len(matching(o.Buckets, "gemini")) != 1 || len(matching(o.Buckets, "3p")) != 1 {
		t.Fatalf("agy models-only %+v", o)
	}
}

func TestAgyModelsUnavailableKeepsSummary(t *testing.T) {
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, ":fetchAvailableModels") {
			return response(404, `{}`), nil
		}
		return response(200, `{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","window":"5h","remainingFraction":0.5,"resetTime":"2026-10-29T06:18:55Z"}]}]}`), nil
	})
	o := fetchAgy(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 1 || o.Buckets[0].Scope.Name != "3p" {
		t.Fatalf("agy summary-only %+v", o)
	}
}

var agyModelsBody = `{"models":{"gemini-3.8-flash-high":{"displayName":"Gemini 3.8 Flash (High)","quotaInfo":{"remainingFraction":0.996479,"resetTime":"2026-10-29T06:18:55Z"}},"claude-sonnet-4-6":{"displayName":"Claude Sonnet 4.6","quotaInfo":{"remainingFraction":1,"resetTime":"2026-10-29T06:18:55Z"}},"chat_1":{"isInternal":true,"quotaInfo":{"remainingFraction":0.1,"resetTime":"2026-10-29T06:18:55Z"}},"mystery-1":{"quotaInfo":{"remainingFraction":0.5,"resetTime":"2026-10-29T06:18:55Z"}},"mystery-monthly":{"quotaInfo":{"remainingFraction":0.9,"window":"monthly","resetTime":"2026-10-29T06:18:55Z"}}}}`

func agyGateCatalog() Catalog {
	c := BundledCatalog()
	c.Rows = []CatalogRow{
		{Profile: "agy-gem", Effort: "high", ModelID: "gemini-3.8-flash-high", Pool: "agy", Grade: "A", Gate: "default", Placed: true, Group: "gemini"},
		{Profile: "agy-son", Effort: "high", ModelID: "claude-sonnet-4-6", Pool: "agy", Grade: "A", Gate: "default", Placed: true, Group: "3p"},
		{Profile: "agy-old", Effort: "high", ModelID: "mystery-1", Pool: "agy", Grade: "A", Gate: "default", Placed: true},
	}
	return c
}

func TestAgyGateAdmitsGroupBucketsFromEitherSource(t *testing.T) {
	summary := `{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","window":"5h","remainingFraction":0.5,"resetTime":"2026-10-29T06:18:55Z"},{"bucketId":"3p-weekly","window":"weekly","remainingFraction":0.4,"resetTime":"2026-11-05T01:18:55Z"}]}]}`
	usage := parseAgy(mustJSON(t, summary), object(mustJSON(t, agyModelsBody)["models"]))
	reset5h := testNow.Add(5 * time.Hour)
	reset7d := testNow.Add(144 * time.Hour)
	shared, ok := sharedAuto(&SharedObservation{Kind: "auto", MeasuredAt: testNow, Buckets: []SharedBucket{
		{Window: "5h", UsedPct: 0.4, ResetsAt: &reset5h, Scope: "group", Model: "gemini"},
		{Window: "5h", UsedPct: 50, ResetsAt: &reset5h, Scope: "group", Model: "3p"},
		{Window: "7d", UsedPct: 40, ResetsAt: &reset7d, Scope: "group", Model: "3p"},
	}}, "agy", "agy:0123456789abcdef", testNow, time.Hour)
	if !ok {
		t.Fatal("shared agy observation rejected")
	}
	for name, obs := range map[string]Observation{"usage-api": usage, "shared": shared} {
		t.Run(name, func(t *testing.T) {
			in := Inputs{Now: testNow, Catalog: agyGateCatalog(), Observations: []Observation{obs}}
			if d := Gate(in, Request{Profile: "agy-gem", Effort: "high"}); d.ExitCode != 0 {
				t.Fatalf("gemini row denied %d: %s", d.ExitCode, d.Reason)
			}
			if d := Gate(in, Request{Profile: "agy-son", Effort: "high"}); d.ExitCode != 0 {
				t.Fatalf("3p row denied %d: %s", d.ExitCode, d.Reason)
			}
			if d := Gate(in, Request{Profile: "agy-old", Effort: "high"}); d.ExitCode != 4 {
				t.Fatalf("ungrouped row admitted %d: %s", d.ExitCode, d.Reason)
			}
		})
	}
}

func TestAgyMissingGroupIsUnknown(t *testing.T) {
	body := `{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","window":"5h","remainingFraction":0.5,"resetTime":"2026-10-29T06:18:55Z"}]}]}`
	o := parseAgy(mustJSON(t, body), nil)
	if len(matching(o.Buckets, "gemini")) != 0 {
		t.Fatalf("3p numbers reached the gemini group: %+v", o.Buckets)
	}
	if len(matching(o.Buckets, "3p")) != 1 {
		t.Fatalf("3p group missing: %+v", o.Buckets)
	}
}

func TestAgyModelFallbackPeriod(t *testing.T) {
	body := `{"models":{"gemini-3.7-flash":{"quotaInfo":{"remainingFraction":0.5,"window":"weekly","resetTime":"2026-11-05T01:18:55Z"}},"claude-3.7":{"quotaInfo":{"remainingFraction":0.25}}}}`
	o := parseAgy(mustJSON(t, body), nil)
	if o.Error != "" || len(o.Buckets) != 2 {
		t.Fatalf("agy model fallback %+v", o)
	}
	if o.Buckets[0].Scope.Name != "gemini" || o.Buckets[0].Window != "7d" || !pctClose(o.Buckets[0].UsedPct, 50) {
		t.Fatalf("gemini model period %+v", o.Buckets[0])
	}
	if o.Buckets[1].Scope.Name != "3p" || o.Buckets[1].Window != "5h" || !pctClose(o.Buckets[1].UsedPct, 75) {
		t.Fatalf("model quota is the session window %+v", o.Buckets[1])
	}
}

func TestAgyGroupMapping(t *testing.T) {
	for _, tt := range []struct{ id, display, want string }{
		{"gemini-5h", "Gemini Models", "gemini"},
		{"3p-5h", "Claude and GPT models", "3p"},
		{"gemini-argon-wtu-weekly", "", "gemini"},
		{"claude-3.7", "", "3p"},
		{"gpt-oss-120b", "", "3p"},
		{"mystery-5h", "", "unknown"},
	} {
		if got := agyGroup(tt.id, tt.display); got != tt.want {
			t.Fatalf("agyGroup(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestAgyFallsBackToSecondHost(t *testing.T) {
	body := `{"response":{"groups":[{"buckets":[{"bucketId":"3p-weekly","window":"weekly","remainingFraction":0.6,"resetTime":"2026-11-05T01:18:55Z"}]}]}}`
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "daily-cloudcode-pa.googleapis.com" {
			return response(500, `{}`), nil
		}
		return response(200, body), nil
	})
	o := fetchAgy(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 1 || o.Buckets[0].Window != "7d" {
		t.Fatalf("agy fallback %+v", o)
	}
}

func TestAgyServerErrorIsRetryable(t *testing.T) {
	client := doFunc(func(*http.Request) (*http.Response, error) { return response(503, `{}`), nil })
	o := fetchAgy(context.Background(), "fixture-token", client, fixedNow)
	if o.ErrorKind != "network" || !strings.Contains(o.Error, "temporarily unavailable") {
		t.Fatalf("agy server error %+v", o)
	}
}

func TestAgyAuthFailure(t *testing.T) {
	client := doFunc(func(*http.Request) (*http.Response, error) { return response(401, `{}`), nil })
	o := fetchAgy(context.Background(), "fixture-token", client, fixedNow)
	if o.ErrorKind != "auth_failed" || !strings.Contains(o.Error, "agy") {
		t.Fatalf("agy auth %+v", o)
	}
}

var agyQuotaBody = `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.8,"resetTime":"2026-10-29T06:18:55Z"},{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.6,"resetTime":"2026-11-05T01:18:55Z"}]}]}`

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func writeFakeAgy(t *testing.T, script string) (dir, marker string) {
	t.Helper()
	for _, k := range []string{"PACELEVER_AGY_BIN", "SCOPEFUEL_AGY_BIN"} {
		t.Setenv(k, "")
	}
	dir = t.TempDir()
	marker = filepath.Join(dir, "wake-count")
	script = "#!/bin/sh\n[ \"$1\" = \"models\" ] || exit 3\necho wake >> " + shellQuote(marker) + "\n" + script
	if e := os.WriteFile(filepath.Join(dir, "agy"), []byte(script), 0755); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir, marker
}

func wakeCount(t *testing.T, marker string) int {
	t.Helper()
	b, e := os.ReadFile(marker)
	if e != nil {
		return 0
	}
	return strings.Count(strings.TrimSpace(string(b)), "\n") + 1
}

func TestAgyWakeRefreshesExpiredToken(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	tokenPath := filepath.Join(dir, "antigravity-oauth-token")
	if e := os.WriteFile(tokenPath, []byte(`{"token":{"access_token":"fixture-expired"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	_, marker := writeFakeAgy(t, "printf '%s' '{\"token\":{\"access_token\":\"fixture-fresh\"}}' > "+shellQuote(tokenPath)+"\n")
	var tokens []string
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tokens = append(tokens, token)
		if token == "fixture-fresh" {
			return response(200, agyQuotaBody), nil
		}
		return response(401, `{}`), nil
	})
	p := newAgyProvider(home, Config{}, client)
	if p.wake == nil {
		t.Fatal("fake agy binary not resolved")
	}
	o := p.Fetch(context.Background())
	if o.Error != "" || len(o.Buckets) == 0 {
		t.Fatalf("refresh did not recover: %+v", o)
	}
	if len(tokens) != 3 || tokens[0] != "fixture-expired" || tokens[1] != "fixture-fresh" || tokens[2] != "fixture-fresh" {
		t.Fatalf("unexpected token sequence %v", tokens)
	}
	if wakeCount(t, marker) != 1 {
		t.Fatal("agy models ran more than once")
	}
}

func TestAgyWakeFailureKeepsAuthHint(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "antigravity-oauth-token"), []byte(`{"token":{"access_token":"fixture-dead"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	_, marker := writeFakeAgy(t, "exit 1\n")
	var calls atomic.Int32
	client := doFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(401, `{}`), nil
	})
	p := newAgyProvider(home, Config{}, client)
	p.secrets = stubSecrets{}
	o := p.Fetch(context.Background())
	if o.ErrorKind != "auth_failed" || !strings.Contains(o.Error, "agy models") {
		t.Fatalf("failed wake did not keep a clear hint: %+v", o)
	}
	if calls.Load() != 1 || wakeCount(t, marker) != 1 {
		t.Fatalf("unexpected retries calls=%d wakes=%d", calls.Load(), wakeCount(t, marker))
	}
}

func TestAgyMissingBinaryKeepsAuthHint(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "antigravity-oauth-token"), []byte(`{"token":{"access_token":"fixture-dead"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	for _, k := range []string{"PACELEVER_AGY_BIN", "SCOPEFUEL_AGY_BIN"} {
		t.Setenv(k, "")
	}
	t.Setenv("PATH", t.TempDir())
	client := doFunc(func(*http.Request) (*http.Response, error) { return response(401, `{}`), nil })
	p := newAgyProvider(home, Config{}, client)
	p.secrets = stubSecrets{}
	if p.wake != nil {
		t.Fatal("wake available without an agy binary")
	}
	o := p.Fetch(context.Background())
	if o.ErrorKind != "auth_failed" || !strings.Contains(o.Error, "agy") {
		t.Fatalf("missing binary hint lost: %+v", o)
	}
}

type stubSecrets struct{ token string }

func (s stubSecrets) agyToken(context.Context) string { return s.token }

type secretFunc func(context.Context) string

func (f secretFunc) agyToken(ctx context.Context) string { return f(ctx) }

func TestAgyKeychainFallbackAfterRefusedFile(t *testing.T) {
	var tokens []string
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tokens = append(tokens, token)
		if token == "fixture-keychain" {
			return response(200, agyQuotaBody), nil
		}
		return response(401, `{}`), nil
	})
	p := agyProvider{
		client:  client,
		file:    func() string { return "fixture-file" },
		secrets: stubSecrets{"fixture-keychain"},
		wake:    func(context.Context) error { return nil },
	}
	o := p.Fetch(context.Background())
	if o.Error != "" || len(o.Buckets) == 0 {
		t.Fatalf("keychain fallback failed: %+v", o)
	}
	if len(tokens) != 4 || tokens[0] != "fixture-file" || tokens[1] != "fixture-file" || tokens[2] != "fixture-keychain" || tokens[3] != "fixture-keychain" {
		t.Fatalf("unexpected token order %v", tokens)
	}
}

func TestAgyWakeRefreshesKeychainToken(t *testing.T) {
	current := "fixture-secret-old"
	var tokens []string
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tokens = append(tokens, token)
		if token == "fixture-secret-new" {
			return response(200, agyQuotaBody), nil
		}
		return response(401, `{}`), nil
	})
	p := agyProvider{
		client:  client,
		file:    func() string { return "" },
		secrets: secretFunc(func(context.Context) string { return current }),
		wake: func(context.Context) error {
			current = "fixture-secret-new"
			return nil
		},
	}
	o := p.Fetch(context.Background())
	if o.Error != "" || len(o.Buckets) == 0 {
		t.Fatalf("refreshed keychain token not picked up: %+v", o)
	}
	if len(tokens) != 3 || tokens[0] != "fixture-secret-old" || tokens[1] != "fixture-secret-new" || tokens[2] != "fixture-secret-new" {
		t.Fatalf("unexpected token order %v", tokens)
	}
}

func TestAgyKeychainFailureKeepsHint(t *testing.T) {
	var calls atomic.Int32
	client := doFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(401, `{}`), nil
	})
	p := agyProvider{
		client:  client,
		file:    func() string { return "fixture-file" },
		secrets: stubSecrets{"fixture-keychain"},
		wake:    func(context.Context) error { return fmt.Errorf("exit 1") },
	}
	o := p.Fetch(context.Background())
	if o.ErrorKind != "auth_failed" || !strings.Contains(o.Error, "agy models") {
		t.Fatalf("refused keychain masked the refresh hint: %+v", o)
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected request count %d", calls.Load())
	}
}

func TestAgyKeychainUsedWhenFileMissing(t *testing.T) {
	var tokens []string
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tokens = append(tokens, token)
		return response(200, agyQuotaBody), nil
	})
	p := agyProvider{
		client:  client,
		file:    func() string { return "" },
		secrets: stubSecrets{"fixture-keychain"},
	}
	o := p.Fetch(context.Background())
	if o.Error != "" || len(tokens) != 2 || tokens[0] != "fixture-keychain" || tokens[1] != "fixture-keychain" {
		t.Fatalf("missing file did not read the keychain: %+v %v", o, tokens)
	}
}

func TestAgyEnvelopeToken(t *testing.T) {
	body := `{"token":{"access_token":"fixture-secret"}}`
	for _, raw := range []string{
		"go-keyring-base64:" + base64.StdEncoding.EncodeToString([]byte(body)),
		"go-keyring-encoded:" + hex.EncodeToString([]byte(body)),
		body,
	} {
		if token := agyEnvelopeToken(raw); token != "fixture-secret" {
			t.Fatalf("envelope %q decoded to %q", raw, token)
		}
	}
	for _, raw := range []string{"", "go-keyring-base64:!!!", "go-keyring-base64:aGVsbG8=", `{"other":1}`} {
		if token := agyEnvelopeToken(raw); token != "" {
			t.Fatalf("envelope %q leaked %q", raw, token)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	for _, tt := range []struct {
		header string
		wait   float64
		want   float64
	}{
		{"120", 300, 120},
		{"0", 300, 300},
		{"", 300, 300},
		{"not-a-date", 0, 0},
	} {
		client := doFunc(func(*http.Request) (*http.Response, error) {
			return responseWithHeader(429, `{}`, http.Header{"Retry-After": []string{tt.header}}), nil
		})
		o := (HTTPProvider{ID: "claude", URL: "https://provider.example.invalid/usage", Client: client, Now: fixedNow, RateLimitWaitS: tt.wait}).Fetch(context.Background())
		if o.ErrorKind != "rate_limited" || o.RetryAfterS != tt.want {
			t.Fatalf("retry-after %q: %+v", tt.header, o)
		}
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	header := testNow.Add(90 * time.Second).UTC().Format(http.TimeFormat)
	client := doFunc(func(*http.Request) (*http.Response, error) {
		return responseWithHeader(429, `{}`, http.Header{"Retry-After": []string{header}}), nil
	})
	o := (HTTPProvider{ID: "claude", URL: "https://provider.example.invalid/usage", Client: client, Now: fixedNow}).Fetch(context.Background())
	if o.RetryAfterS != 90 {
		t.Fatalf("http-date retry-after %+v", o)
	}
}

func TestNegativeCacheHonorsRetryAfter(t *testing.T) {
	isolate(t)
	var calls atomic.Int32
	c := Collector{Providers: map[string]Provider{"claude": ProviderFunc(func(context.Context) Observation {
		calls.Add(1)
		return Observation{ID: "claude", Error: "usage HTTP 429", ErrorKind: "rate_limited", RetryAfterS: 300}
	})}, Now: func() time.Time { return testNow }}
	rows, e := c.Collect(context.Background(), []string{"claude"})
	if e != nil || rows[0].ErrorKind != "rate_limited" {
		t.Fatalf("first rate limit %+v %v", rows, e)
	}
	rows, e = c.Collect(context.Background(), []string{"claude"})
	if e != nil || rows[0].ErrorKind != "rate_limited" || calls.Load() != 1 {
		t.Fatalf("retry-after not honoured %+v %v calls=%d", rows, e, calls.Load())
	}
	c.Now = func() time.Time { return testNow.Add(301 * time.Second) }
	if _, e = c.Collect(context.Background(), []string{"claude"}); e != nil || calls.Load() != 2 {
		t.Fatalf("retry-after did not expire calls=%d %v", calls.Load(), e)
	}
}

func TestProviderTTL(t *testing.T) {
	if ttlFor("claude") != 900*time.Second {
		t.Fatal("claude TTL must be 900s")
	}
	if ttlFor("agy") != 300*time.Second {
		t.Fatal("agy TTL must be 300s")
	}
}

func TestCodexWindowsBySeconds(t *testing.T) {
	body := `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_at":1793232000},"secondary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":1793232000}}}`
	p := HTTPProvider{ID: "codex", URL: "https://provider.example.invalid/usage", Client: doFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })}
	o := p.Fetch(context.Background())
	if o.Error != "" || len(o.Buckets) != 2 || o.Buckets[0].Window != "7d" || o.Buckets[1].Window != "5h" {
		t.Fatalf("codex windows by position %+v", o)
	}
}

func TestClineProvidersJSON(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".cline", "data", "settings", "providers.json")
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte(`{"providers":{"cline-pass":{"settings":{"auth":{"accessToken":"fixture-cline-token"}}}}}`), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("CLINE_API_KEY", "")
	t.Setenv("CLINEPASS_API_KEY", "")
	if token := clineToken(home); token != "fixture-cline-token" {
		t.Fatalf("cline token %q", token)
	}
}

func TestProviderTokens(t *testing.T) {
	home := t.TempDir()
	if e := os.MkdirAll(filepath.Join(home, ".commandcode"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(home, ".commandcode", "auth.json"), []byte(`{"apiKey":"fixture-command-code"}`), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("COMMAND_CODE_API_KEY", "")
	t.Setenv("COMMANDCODE_API_KEY", "")
	if token := commandCodeToken(home); token != "fixture-command-code" {
		t.Fatalf("command code token %q", token)
	}
	if e := os.MkdirAll(filepath.Join(home, ".grok"), 0700); e != nil {
		t.Fatal(e)
	}
	grok := `{"https://auth.x.ai::fixture":{"key":"fixture-grok","expires_at":"2026-11-01T00:00:00Z"}}`
	if e := os.WriteFile(filepath.Join(home, ".grok", "auth.json"), []byte(grok), 0600); e != nil {
		t.Fatal(e)
	}
	if token, _ := grokCredential(home, nil, ""); token != "fixture-grok" {
		t.Fatalf("grok token %q", token)
	}
	if e := os.MkdirAll(filepath.Join(home, ".gemini", "antigravity-cli"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"), []byte(`{"token":{"access_token":"fixture-agy"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	if token := agyToken(home); token != "fixture-agy" {
		t.Fatalf("agy token %q", token)
	}
}

func TestGrokRecordDeterministic(t *testing.T) {
	home := t.TempDir()
	if e := os.MkdirAll(filepath.Join(home, ".grok"), 0700); e != nil {
		t.Fatal(e)
	}
	body := `{
	  "https://auth.x.ai::a":{"key":"older","user_id":"fixture-old","expires_at":"2026-11-01T00:00:00Z","create_time":"2026-10-01T00:00:00Z"},
	  "https://auth.x.ai::b":{"key":"newer","user_id":"fixture-new","expires_at":"2026-12-01T00:00:00Z","create_time":"2026-10-02T00:00:00Z"}
	}`
	if e := os.WriteFile(filepath.Join(home, ".grok", "auth.json"), []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	token, fp := grokCredential(home, nil, "")
	if token != "newer" || fp != fingerprint("grok-account", "fixture-new") {
		t.Fatalf("grok picked %q %q", token, fp)
	}
	want := fingerprint("grok-account", "fixture-old")
	token, fp = grokCredential(home, nil, want)
	if token != "older" || fp != want {
		t.Fatalf("grok did not honour the account key: %q %q", token, fp)
	}
	if token, _ := grokCredential(home, nil, "ffffffffffffffff"); token != "" {
		t.Fatalf("grok used a record for another account: %q", token)
	}
}

func TestSharedAutoAcceptsGroupScope(t *testing.T) {
	x := &SharedObservation{Kind: "auto", MeasuredAt: testNow, Buckets: []SharedBucket{{Window: "7d", UsedPct: 20, Scope: "group", Model: "gemini"}}}
	o, ok := sharedAuto(x, "agy", "agy:0123456789abcdef", testNow, time.Hour)
	if !ok || len(o.Buckets) != 1 || o.Buckets[0].Scope.Kind != "group" || o.Buckets[0].Scope.Name != "gemini" {
		t.Fatalf("group scope lost: %+v", o)
	}
}

func TestAgyPoolKnown(t *testing.T) {
	if !knownPool("agy") {
		t.Fatal("agy must be a known pool")
	}
	if windows := RequiredWindows("agy", "", false); len(windows) != 2 || windows[0] != "5h" || windows[1] != "7d" {
		t.Fatalf("agy required windows %v", windows)
	}
}
