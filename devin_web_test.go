package pacelever

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevinWebFlatShape(t *testing.T) {
	body := `{"daily_percentage":0.25,"daily_reset_at":"2026-11-01T00:00:00Z","weekly_percentage":55,"weekly_reset_at":1770500000,"plan_name":"teams"}`
	var seen *http.Request
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		seen = r
		return response(200, body), nil
	})
	o := fetchDevinWeb(context.Background(), "fixture-token", "org-fixture", client, fixedNow)
	if o.Error != "" || o.ID != "devin" || o.Source != "usage-api" || o.Plan != "Teams" {
		t.Fatalf("devin flat observation %+v", o)
	}
	if len(o.Buckets) != 2 {
		t.Fatalf("devin flat buckets %+v", o.Buckets)
	}
	if o.Buckets[0].Window != "1d" || !pctClose(o.Buckets[0].UsedPct, 25) || o.Buckets[0].ResetsAt != "2026-11-01T00:00:00Z" {
		t.Fatalf("daily bucket %+v", o.Buckets[0])
	}
	if o.Buckets[1].Window != "7d" || !pctClose(o.Buckets[1].UsedPct, 55) || o.Buckets[1].ResetsAt != "2026-02-07T21:33:20Z" {
		t.Fatalf("weekly bucket %+v", o.Buckets[1])
	}
	if seen == nil || seen.Header.Get("Authorization") != "Bearer fixture-token" || seen.Header.Get("x-cog-org-id") != "org-fixture" {
		t.Fatal("devin web headers missing")
	}
	if seen.URL.Path != "/api/org-fixture/billing/quota/usage" {
		t.Fatalf("devin web path %q", seen.URL.Path)
	}
}

func TestDevinWebNestedShape(t *testing.T) {
	body := `{"quota_usage":{"daily_quota":{"used_percent":0.4,"reset_at":"2026-11-01T00:00:00Z"},"weekly_quota":{"remaining_percent":30,"next_reset_at":1770500000000}}}`
	o := fetchDevinWeb(context.Background(), "fixture-token", "org-fixture", doFunc(func(*http.Request) (*http.Response, error) {
		return response(200, body), nil
	}), fixedNow)
	if o.Error != "" || len(o.Buckets) != 2 {
		t.Fatalf("devin nested observation %+v", o)
	}
	if !pctClose(o.Buckets[0].UsedPct, 40) || o.Buckets[0].ResetsAt != "2026-11-01T00:00:00Z" {
		t.Fatalf("nested daily bucket %+v", o.Buckets[0])
	}
	if !pctClose(o.Buckets[1].UsedPct, 70) || o.Buckets[1].ResetsAt != "2026-02-07T21:33:20Z" {
		t.Fatalf("nested weekly bucket %+v", o.Buckets[1])
	}
}

func TestDevinWebUsedLimitShape(t *testing.T) {
	body := `{"quota_usage":{"daily_quota":{"used":"2","limit":"5"},"weekly_quota":{"limit":"100","remaining":"23"}}}`
	o := parseDevin(mustJSON(t, body))
	if o.Error != "" || len(o.Buckets) != 2 {
		t.Fatalf("devin used/limit observation %+v", o)
	}
	if !pctClose(o.Buckets[0].UsedPct, 40) || !pctClose(o.Buckets[1].UsedPct, 77) {
		t.Fatalf("devin used/limit buckets %+v", o.Buckets)
	}
}

func TestDevinWebRemainingPercentBoundary(t *testing.T) {
	o := parseDevin(mustJSON(t, `{"quota_usage":{"daily_quota":{"remaining_percent":1},"weekly_quota":{"remaining_percent":0.5}}}`))
	if o.Error != "" || len(o.Buckets) != 2 {
		t.Fatalf("devin remaining observation %+v", o)
	}
	if !pctClose(o.Buckets[0].UsedPct, 0) || !pctClose(o.Buckets[1].UsedPct, 50) {
		t.Fatalf("devin remaining buckets %+v", o.Buckets)
	}
}

func TestDevinWebHidesDailyQuota(t *testing.T) {
	o := parseDevin(mustJSON(t, `{"hide_daily_quota":true,"daily_percentage":80,"weekly_percentage":30}`))
	if o.Error != "" || len(o.Buckets) != 1 || o.Buckets[0].Window != "7d" || !pctClose(o.Buckets[0].UsedPct, 30) {
		t.Fatalf("devin hidden daily %+v", o)
	}
}

func TestDevinWebNoRecognizedWindows(t *testing.T) {
	o := parseDevin(mustJSON(t, `{"unexpected":true}`))
	if o.ErrorKind != "parse_error" || o.Error == "" || len(o.Buckets) != 0 {
		t.Fatalf("devin unknown shape %+v", o)
	}
}

func TestDevinWebIgnoresMalformedPercentages(t *testing.T) {
	for _, body := range []string{
		`{"daily_percentage":-5,"weekly_percentage":"nope"}`,
		`{"quota_usage":{"weekly_quota":{"remaining_percent":150}}}`,
		`{"quota_usage":{"weekly_quota":{"used":-1,"limit":10}}}`,
	} {
		o := parseDevin(mustJSON(t, body))
		if o.ErrorKind != "parse_error" || len(o.Buckets) != 0 {
			t.Fatalf("malformed percentage admitted: %s -> %+v", body, o)
		}
	}
}

func TestDevinWebClampsOverCap(t *testing.T) {
	o := parseDevin(mustJSON(t, `{"weekly_percentage":150}`))
	if o.Error != "" || len(o.Buckets) != 1 || !pctClose(o.Buckets[0].UsedPct, 100) {
		t.Fatalf("over-cap percentage %+v", o)
	}
}

func TestDevinWebAuthFailureRedacted(t *testing.T) {
	token, org := "fixture-token-do-not-print", "org-do-not-print"
	o := fetchDevinWeb(context.Background(), token, org, doFunc(func(*http.Request) (*http.Response, error) {
		return response(401, `{"token":"`+token+`","org":"`+org+`"}`), nil
	}), fixedNow)
	if o.ErrorKind != "auth_failed" {
		t.Fatalf("devin auth kind %+v", o)
	}
	if strings.Contains(o.Error, token) || strings.Contains(o.Error, org) {
		t.Fatal("devin credential leaked into the error")
	}
}

func TestDevinWebRejectsMalformedOrg(t *testing.T) {
	called := false
	o := fetchDevinWeb(context.Background(), "fixture-token", "org/../../elsewhere", doFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return response(200, `{}`), nil
	}), fixedNow)
	if called || o.ErrorKind != "credentials" {
		t.Fatalf("malformed organization reached the endpoint %+v", o)
	}
}

func TestDevinWebProviderNeedsTokenAndOrg(t *testing.T) {
	t.Setenv("DEVIN_BEARER_TOKEN", "")
	cfg := Config{Pools: map[string]PoolPolicy{"devin": {OrgID: "org-fixture"}}}
	if devinWebProvider(cfg, nil, fixedNow) != nil {
		t.Fatal("devin web provider built without a token")
	}
	t.Setenv("DEVIN_BEARER_TOKEN", "fixture-token")
	cfg.Pools["devin"] = PoolPolicy{OrgID: "not-an-org"}
	if devinWebProvider(cfg, nil, fixedNow) != nil {
		t.Fatal("devin web provider built with a malformed org")
	}
}

func TestDevinProviderPrefersWeb(t *testing.T) {
	t.Setenv("DEVIN_BEARER_TOKEN", "fixture-token")
	cfg := Config{Pools: map[string]PoolPolicy{"devin": {OrgID: "org-fixture"}}}
	preferred := devinWebProvider(cfg, doFunc(func(*http.Request) (*http.Response, error) {
		return response(200, `{"weekly_percentage":42}`), nil
	}), fixedNow)
	if preferred == nil {
		t.Fatal("devin web provider not built")
	}
	// An unavailable CLI proves the web answer was returned without a probe.
	p := PTYProvider{ID: "devin", Binary: filepath.Join(t.TempDir(), "missing-devin"), Preferred: preferred}
	o := p.Fetch(context.Background())
	if o.Error != "" || o.Source != "usage-api" || len(o.Buckets) != 1 || !pctClose(o.Buckets[0].UsedPct, 42) {
		t.Fatalf("web answer not preferred %+v", o)
	}
}

func TestDevinProviderFallsBackToCLI(t *testing.T) {
	isolate(t)
	preferred := ProviderFunc(func(context.Context) Observation {
		return Observation{ID: "devin", Error: "usage response has no recognized windows", ErrorKind: "parse_error"}
	})
	p := PTYProvider{ID: "devin", Binary: filepath.Join(t.TempDir(), "missing-devin"), Preferred: preferred}
	o := p.Fetch(context.Background())
	if o.Source != "cli:/usage" || o.ErrorKind != "probe_error" {
		t.Fatalf("failed web source did not fall back to the CLI %+v", o)
	}
}

func TestDefaultProvidersWiresDevinWeb(t *testing.T) {
	t.Setenv("DEVIN_BEARER_TOKEN", "fixture-token")
	cfg := Config{Pools: map[string]PoolPolicy{"devin": {OrgID: "org-fixture"}}}
	provider, ok := DefaultProviders(cfg)["devin"].(PTYProvider)
	if !ok || provider.Preferred == nil {
		t.Fatal("devin web source is not wired ahead of the CLI probe")
	}
}

func TestConfigDevinOrgID(t *testing.T) {
	isolate(t)
	if e := AtomicWrite(ConfigPath(), []byte("[pools.devin]\norg_id = \"org-fixture\"\n")); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig()
	if e != nil || c.Pools["devin"].OrgID != "org-fixture" {
		t.Fatalf("devin org_id not loaded: %v", e)
	}
}
