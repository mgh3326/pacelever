package pacelever

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Recorded from GET /v1/billing?format=credits.
const grokBillingFixture = `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-22T00:00:00Z","end":"2026-10-29T00:00:00Z"},"creditUsagePercent":91,"productUsage":[{"product":"GrokBuild","usagePercent":5},{"product":"GrokImagine","usagePercent":3}]}}`

func writeGrokAuth(t *testing.T, home, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".grok")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, "auth.json")
	if e := os.WriteFile(path, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}

func grokTestRecord(t *testing.T, home, wantFP string) grokRecord {
	t.Helper()
	t.Setenv("GROK_HOME", "")
	record, ok := selectGrokRecord(grokRecords(home, nil), wantFP, fixedNow())
	if !ok {
		t.Fatal("no grok record selected")
	}
	return record
}

func TestGrokSettingsPlan(t *testing.T) {
	var settingsCalls int
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/billing":
			return response(200, grokBillingFixture), nil
		case "/v1/settings":
			settingsCalls++
			if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("x-grok-client-mode") != "build" {
				t.Fatal("settings request lost the grok credentials")
			}
			return response(200, `{"subscription_tier_display":"SuperGrok Heavy"}`), nil
		}
		return response(404, `{}`), nil
	})
	o := fetchGrok(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" || settingsCalls != 1 {
		t.Fatalf("grok settings calls=%d %+v", settingsCalls, o)
	}
	if o.Plan != "SuperGrok Heavy" {
		t.Fatalf("grok plan %q", o.Plan)
	}
}

func TestGrokSettingsOptional(t *testing.T) {
	body := strings.Replace(grokBillingFixture, `"creditUsagePercent":91`, `"creditUsagePercent":91,"subscriptionTier":"SUPERGROK_HEAVY"`, 1)
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/settings" {
			return response(500, `oops`), nil
		}
		return response(200, body), nil
	})
	o := fetchGrok(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 3 {
		t.Fatalf("settings failure broke usage %+v", o)
	}
	if o.Plan != "SuperGrok Heavy" {
		t.Fatalf("billing plan fallback %q", o.Plan)
	}
}

func TestGrokPlanName(t *testing.T) {
	for input, want := range map[string]string{
		"":                "",
		"SUPERGROK_HEAVY": "SuperGrok Heavy",
		"heavy":           "SuperGrok Heavy",
		"supergrok":       "SuperGrok",
		"Grok Team":       "Grok Team",
		"  GROK_TEAM  ":   "GROK_TEAM",
	} {
		if got := grokPlanName(input); got != want {
			t.Fatalf("plan %q = %q, want %q", input, got, want)
		}
	}
}

func TestGrokProductNames(t *testing.T) {
	for input, want := range map[string]string{
		"GrokBuild":      "Build",
		"GrokImagine":    "Imagine",
		"Grok":           "Grok",
		"SomeNewProduct": "Some New Product",
		"":               "",
	} {
		if got := grokProductName(input); got != want {
			t.Fatalf("product %q = %q, want %q", input, got, want)
		}
	}
	if name := grokProductName("GrokVoice"); name != "Voice" {
		t.Fatalf("product GrokVoice = %q", name)
	}
}

func TestGrokOnDemandAndPrepaid(t *testing.T) {
	body := `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-10-29T00:00:00Z"},"creditUsagePercent":10,"onDemandCap":{"val":100},"onDemandUsed":{"val":25},"prepaidBalance":{"val":"249"}}}`
	client := doFunc(func(r *http.Request) (*http.Response, error) { return response(200, body), nil })
	o := fetchGrok(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 2 {
		t.Fatalf("grok extra buckets %+v", o)
	}
	onDemand := o.Buckets[1]
	if onDemand.Label != "On-Demand" || onDemand.Scope.Kind != "model" || !pctClose(onDemand.UsedPct, 25) || onDemand.Window != "7d" {
		t.Fatalf("on-demand bucket %+v", onDemand)
	}
	if !strings.Contains(o.Buckets[0].Note, "prepaid balance 2.49 USD") {
		t.Fatalf("prepaid balance note %q", o.Buckets[0].Note)
	}
}

func TestGrokEmptyOnDemandAndPrepaidOmitted(t *testing.T) {
	body := `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-10-29T00:00:00Z"},"creditUsagePercent":10,"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"prepaidBalance":{"val":0}}}`
	client := doFunc(func(r *http.Request) (*http.Response, error) { return response(200, body), nil })
	o := fetchGrok(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 1 {
		t.Fatalf("empty caps must not invent a bucket: %+v", o)
	}
	if strings.Contains(o.Buckets[0].Note, "prepaid") {
		t.Fatalf("empty prepaid shown: %q", o.Buckets[0].Note)
	}
}

// Every grok bucket carries a percentage, so the shared publisher accepts the
// observation and the read-back keeps the model buckets.
func TestGrokObservationIsPublishableShared(t *testing.T) {
	body := `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-10-29T00:00:00Z"},"creditUsagePercent":10,"onDemandCap":{"val":100},"onDemandUsed":{"val":25},"prepaidBalance":{"val":"249"},"productUsage":[{"product":"GrokBuild","usagePercent":5}]}}`
	client := doFunc(func(r *http.Request) (*http.Response, error) { return response(200, body), nil })
	o := fetchGrok(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" {
		t.Fatalf("grok observation %+v", o)
	}
	x := &SharedObservation{Kind: "auto", MeasuredAt: testNow, Plan: o.Plan}
	for _, b := range o.Buckets {
		if b.UsedPct == nil {
			t.Fatalf("bucket without a percentage cannot be published: %+v", b)
		}
		x.Buckets = append(x.Buckets, SharedBucket{Window: b.Window, UsedPct: *b.UsedPct, Scope: b.Scope.Kind, Model: b.Scope.Name})
	}
	back, ok := sharedAuto(x, "grok", "grok:0123456789abcdef", testNow, time.Minute, false)
	if !ok || len(back.Buckets) != len(o.Buckets) {
		t.Fatalf("shared read-back %+v ok=%v", back, ok)
	}
	if back.Buckets[1].Scope.Kind != "model" || back.Buckets[1].Scope.Name != "Build" {
		t.Fatalf("shared read-back lost the model scope: %+v", back.Buckets[1])
	}
}

func TestGrokBillingPeriodEndFallback(t *testing.T) {
	body := `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-01T00:00:00Z"},"billingPeriodStart":"2026-10-01T00:00:00Z","billingPeriodEnd":"2026-10-08T00:00:00Z","creditUsagePercent":10}}`
	client := doFunc(func(r *http.Request) (*http.Response, error) { return response(200, body), nil })
	o := fetchGrok(context.Background(), "fixture-token", client, fixedNow)
	if o.Error != "" || o.Buckets[0].ResetsAt != "2026-10-08T00:00:00Z" {
		t.Fatalf("grok reset fallback %+v", o)
	}
}

func TestGrokRefreshOnExpiry(t *testing.T) {
	home := t.TempDir()
	path := writeGrokAuth(t, home, `{"https://auth.x.ai::fixture":{"key":"stale-token","refresh_token":"fixture-refresh","expires_at":"2020-01-01T00:00:00Z","oidc_issuer":"https://auth.x.ai/","oidc_client_id":"fixture-client","user_id":"fixture-user"}}`)
	before, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	record := grokTestRecord(t, home, "")
	refreshed := false
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth2/token" {
			if r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Fatalf("refresh request %s %s", r.Method, r.Header.Get("Content-Type"))
			}
			if r.URL.String() != "https://auth.x.ai/oauth2/token" {
				t.Fatalf("refresh URL %s", r.URL.String())
			}
			raw, _ := io.ReadAll(r.Body)
			form, e := url.ParseQuery(string(raw))
			if e != nil {
				t.Fatal(e)
			}
			if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "fixture-refresh" || form.Get("client_id") != "fixture-client" {
				t.Fatalf("refresh form %q", string(raw))
			}
			refreshed = true
			return response(200, `{"access_token":"fresh-token","refresh_token":"fresh-refresh","expires_in":3600}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer fresh-token" {
			t.Fatalf("billing used %q", r.Header.Get("Authorization"))
		}
		return response(200, grokBillingFixture), nil
	})
	o := fetchGrokRecord(context.Background(), record, client, fixedNow)
	if o.Error != "" || !refreshed || len(o.Buckets) != 3 {
		t.Fatalf("expired login was not refreshed: refreshed=%v %+v", refreshed, o)
	}
	after, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refreshed token was written back to the CLI login file")
	}
}

func TestGrokRefreshOnAuthFailure(t *testing.T) {
	record := grokRecord{Token: "stale-token", RefreshToken: "fixture-refresh", Issuer: "https://auth.x.ai", ClientID: "fixture-client", HasExpiry: true, Expires: testNow.Add(48 * time.Hour)}
	var billingCalls, refreshCalls int
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth2/token" {
			refreshCalls++
			return response(200, `{"access_token":"fresh-token","expires_in":3600}`), nil
		}
		if r.URL.Path != "/v1/billing" {
			return response(200, `{}`), nil
		}
		billingCalls++
		if r.Header.Get("Authorization") == "Bearer stale-token" {
			return response(401, `{}`), nil
		}
		return response(200, grokBillingFixture), nil
	})
	o := fetchGrokRecord(context.Background(), record, client, fixedNow)
	if o.Error != "" || billingCalls != 2 || refreshCalls != 1 || len(o.Buckets) != 3 {
		t.Fatalf("401 refresh: billing=%d refresh=%d %+v", billingCalls, refreshCalls, o)
	}
}

func TestGrokUsableLoginSurvivesRefreshOutage(t *testing.T) {
	record := grokRecord{Token: "still-good", RefreshToken: "fixture-refresh", Issuer: "https://auth.x.ai", HasExpiry: true, Expires: testNow.Add(200 * time.Second)}
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth2/token" {
			return response(500, `{}`), nil
		}
		if r.URL.Path == "/v1/settings" {
			return response(200, `{}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer still-good" {
			t.Fatalf("billing used %q", r.Header.Get("Authorization"))
		}
		return response(200, grokBillingFixture), nil
	})
	o := fetchGrokRecord(context.Background(), record, client, fixedNow)
	if o.Error != "" || len(o.Buckets) != 3 {
		t.Fatalf("a still-valid login was lost to a refresh outage: %+v", o)
	}
}

func TestGrokRefreshExpiryHandling(t *testing.T) {
	base := grokRecord{Token: "stale-token", RefreshToken: "fixture-refresh", Issuer: "https://auth.x.ai", HasExpiry: true, Expires: testNow.Add(-time.Hour)}
	refresh := func(body string) (grokRecord, bool) {
		client := doFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		next, _, ok := grokRefresh(context.Background(), base, client, fixedNow)
		return next, ok
	}
	// No stated lifetime: the old expiry must not survive, or every probe
	// would treat the renewed login as due again.
	next, ok := refresh(`{"access_token":"fresh-token"}`)
	if !ok || next.Token != "fresh-token" || next.HasExpiry || grokRefreshDue(next, testNow) {
		t.Fatalf("renewed login kept a stale expiry: %+v", next)
	}
	// A numeric-string lifetime is accepted.
	next, ok = refresh(`{"access_token":"fresh-token","expires_in":"3600"}`)
	if !ok || !next.HasExpiry || !next.Expires.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("renewed login expiry: %+v", next)
	}
}

func TestGrokRefreshRefused(t *testing.T) {
	record := grokRecord{Token: "stale-token", RefreshToken: "fixture-refresh", Issuer: "https://auth.x.ai", HasExpiry: true, Expires: testNow.Add(-time.Hour)}
	billingCalled := false
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth2/token" {
			return response(400, `{"error":"invalid_grant"}`), nil
		}
		billingCalled = true
		return response(200, grokBillingFixture), nil
	})
	o := fetchGrokRecord(context.Background(), record, client, fixedNow)
	if o.ErrorKind != "auth_failed" || !strings.Contains(o.Error, "grok login") {
		t.Fatalf("refused refresh %+v", o)
	}
	if billingCalled {
		t.Fatal("billing was called with a login that could not be refreshed")
	}
}

func TestGrokRefreshedLoginStillRefused(t *testing.T) {
	record := grokRecord{Token: "stale-token", RefreshToken: "fixture-refresh", Issuer: "https://auth.x.ai", HasExpiry: true, Expires: testNow.Add(48 * time.Hour)}
	var billingCalls int
	client := doFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth2/token" {
			return response(200, `{"access_token":"fresh-token","expires_in":3600}`), nil
		}
		billingCalls++
		return response(403, `{}`), nil
	})
	o := fetchGrokRecord(context.Background(), record, client, fixedNow)
	if o.ErrorKind != "auth_failed" || !strings.Contains(o.Error, "grok login") || billingCalls != 2 {
		t.Fatalf("refused after refresh: calls=%d %+v", billingCalls, o)
	}
}

func TestGrokExpiredWithoutRefreshToken(t *testing.T) {
	home := t.TempDir()
	writeGrokAuth(t, home, `{"https://auth.x.ai::fixture":{"key":"stale-token","expires_at":"2020-01-01T00:00:00Z"}}`)
	t.Setenv("GROK_HOME", "")
	if record, ok := selectGrokRecord(grokRecords(home, nil), "", fixedNow()); ok {
		t.Fatalf("expired login without a refresh token was used: %q", record.Token)
	}
}

func TestGrokRefreshDue(t *testing.T) {
	base := grokRecord{Token: "t", RefreshToken: "r", Issuer: "https://auth.x.ai", HasExpiry: true}
	cases := []struct {
		name   string
		record grokRecord
		want   bool
	}{
		{"within skew", func() grokRecord { r := base; r.Expires = testNow.Add(200 * time.Second); return r }(), true},
		{"past expiry", func() grokRecord { r := base; r.Expires = testNow.Add(-time.Minute); return r }(), true},
		{"outside skew", func() grokRecord { r := base; r.Expires = testNow.Add(10 * time.Minute); return r }(), false},
		{"no expiry", func() grokRecord { r := base; r.HasExpiry = false; return r }(), false},
		{"no refresh token", func() grokRecord { r := base; r.RefreshToken = ""; r.Expires = testNow.Add(-time.Hour); return r }(), false},
		{"no issuer", func() grokRecord { r := base; r.Issuer = ""; r.Expires = testNow.Add(-time.Hour); return r }(), false},
		{"plain http issuer", func() grokRecord {
			r := base
			r.Issuer = "http://auth.x.ai"
			r.Expires = testNow.Add(-time.Hour)
			return r
		}(), false},
	}
	for _, c := range cases {
		if got := grokRefreshDue(c.record, testNow); got != c.want {
			t.Fatalf("%s: due=%v want %v", c.name, got, c.want)
		}
	}
}

func TestGrokRecordSelection(t *testing.T) {
	now := fixedNow()
	live := grokRecord{Token: "live", Expires: now.Add(time.Hour), HasExpiry: true}
	stale := grokRecord{Token: "stale", RefreshToken: "r", Issuer: "https://auth.x.ai", Expires: now.Add(-time.Hour), HasExpiry: true}
	record, ok := selectGrokRecord([]grokRecord{stale, live}, "", now)
	if !ok || record.Token != "live" {
		t.Fatalf("a stale refreshable record displaced a live one: %+v", record)
	}
	record, ok = selectGrokRecord([]grokRecord{stale}, "", now)
	if !ok || record.Token != "stale" {
		t.Fatalf("a refreshable expired record was not selectable: %+v", record)
	}
	if _, ok := selectGrokRecord([]grokRecord{live}, "ffffffffffffffff", now); ok {
		t.Fatal("a record for another account was used")
	}
}
