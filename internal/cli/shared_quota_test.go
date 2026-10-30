package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pl "github.com/mgh3326/pacelever"
)

func TestSharedQuotaManualOnlyStatus(t *testing.T) {
	isolated(t)
	now := time.Now().UTC()
	expiry := now.Add(time.Hour)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/latest") {
			t.Errorf("manual-only pool must not acquire or probe: %s", r.Method)
			w.WriteHeader(500)
			return
		}
		response := pl.SharedLatest{}
		if strings.Contains(r.URL.Path, "anthropic-api:") {
			response.Manual = []pl.SharedObservation{{ID: 1, Kind: "manual", Window: "credit", MeasuredAt: now, ExpiresAt: &expiry, Buckets: []pl.SharedBucket{{Window: "credit", UsedPct: 30, Scope: "account"}}}}
		} else {
			response.Observation = &pl.SharedObservation{Kind: "auto", MeasuredAt: now, Plan: "fixture", Buckets: []pl.SharedBucket{{Window: "7d", UsedPct: 20, Scope: "account"}}}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer h.Close()
	t.Setenv("QUOTA_TEST_CREDENTIAL", "fixture-token")
	cfg := pl.Config{Bench: pl.BenchConfig{AllowPlaintextQuotaShare: true}, Quota: pl.QuotaConfig{URL: h.URL, CredentialEnv: "QUOTA_TEST_CREDENTIAL", Accounts: map[string]string{"grok": "0123456789abcdef", "anthropic-api": "fedcba9876543210"}, MeasurableAccounts: []string{"anthropic-api:fedcba9876543210"}}}
	app := App{Config: cfg, Collector: pl.Collector{Config: cfg}}
	in, err := app.inputs(context.Background(), pl.Catalog{}, []string{"grok"})
	if err != nil {
		t.Fatal(err)
	}
	payload := statusPayload(in)
	data, _ := json.Marshal(payload)
	var result struct {
		Providers []struct {
			ID, Status, Source string
			Buckets            []pl.Bucket
		}
	}
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	for _, p := range result.Providers {
		if p.ID == "anthropic-api" {
			if p.Status != "ok" || p.Source != "operator" || len(p.Buckets) != 1 || p.Buckets[0].UsedPct == nil || *p.Buckets[0].UsedPct != 30 {
				t.Fatalf("manual-only status: %s", data)
			}
			return
		}
	}
	t.Fatal("manual-only row missing")
}

// refresh publishes: a presence record the store refuses is reported on stderr
// and fails the command, after the observation itself was served.
func TestRefreshFailsWhenPresencePublishIsRefused(t *testing.T) {
	isolated(t)
	now := time.Now().UTC()
	refuse := true
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET":
			_ = json.NewEncoder(w).Encode(pl.SharedLatest{Observation: &pl.SharedObservation{Kind: "auto", MeasuredAt: now, Plan: "fixture", Buckets: []pl.SharedBucket{{Window: "7d", UsedPct: 20, Scope: "account"}}}})
		case r.URL.Path == "/v1/quota/presence" && refuse:
			w.WriteHeader(500)
		default:
			w.WriteHeader(204)
		}
	}))
	defer h.Close()
	t.Setenv("QUOTA_TEST_CREDENTIAL", "fixture-token")
	cfg := pl.Config{Bench: pl.BenchConfig{AllowPlaintextQuotaShare: true}, Quota: pl.QuotaConfig{URL: h.URL, CredentialEnv: "QUOTA_TEST_CREDENTIAL", HostID: "fixture-host", Accounts: map[string]string{"grok": "0123456789abcdef"}, MeasurableAccounts: []string{"grok:0123456789abcdef"}}}
	for _, tc := range []struct {
		refuse bool
		code   int
	}{{true, pl.Unmeasurable}, {false, 0}} {
		refuse = tc.refuse
		var out, errOut strings.Builder
		app := App{Out: &out, Err: &errOut, Config: cfg, Bench: pl.NewBenchClient(cfg)}
		app.Collector = pl.Collector{Config: cfg, CachePath: t.TempDir() + "/cache.json", Report: app.report}
		a, _ := parse([]string{"refresh", "grok"})
		code, err := app.run(context.Background(), a)
		if code != tc.code || (err != nil) != tc.refuse || strings.Contains(errOut.String(), "warning: "+pl.MsgPresencePublishFailed) != tc.refuse {
			t.Fatalf("refused=%v: code=%d err=%v stderr=%q", tc.refuse, code, err, errOut.String())
		}
	}
}
