package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pl "github.com/mgh3326/pacelever"
)

func TestGateAccountUsesSharedJudgment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		used  float64
		class string
		want  int
	}{
		{"free account", 10, "spend", pl.Admitted},
		{"tight account", 100, "spend", pl.Blocked},
		{"preserve cutoff", 95, "preserve", pl.Blocked},
		{"pace warning", 60, "spend", pl.Admitted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolated(t)
			now := time.Now().UTC()
			reset := now.Add(5 * 24 * time.Hour)
			key := "claude:0123456789abcdef"
			reads := 0
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				if r.Method != "GET" || r.URL.Path != "/v1/quota/accounts/"+key+"/latest" {
					t.Errorf("unexpected shared request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				_ = json.NewEncoder(w).Encode(pl.SharedLatest{Observation: &pl.SharedObservation{Kind: "auto", MeasuredAt: now, Buckets: []pl.SharedBucket{{Window: "7d", UsedPct: tc.used, Scope: "account", ResetsAt: &reset}}}})
			}))
			defer h.Close()
			t.Setenv("QUOTA_TEST_CREDENTIAL", "fixture-token")
			cfg := pl.Config{Bench: pl.BenchConfig{AllowPlaintextQuotaShare: true}, Quota: pl.QuotaConfig{URL: h.URL, CredentialEnv: "QUOTA_TEST_CREDENTIAL", Accounts: map[string]string{"claude": "fedcba9876543210"}}, Pools: map[string]pl.PoolPolicy{"claude": {Class: tc.class}}}
			var out, errOut bytes.Buffer
			app := App{Config: cfg, Out: &out, Err: &errOut, Collector: pl.Collector{Config: cfg, Providers: map[string]pl.Provider{"claude": pl.ProviderFunc(func(context.Context) pl.Observation {
				t.Error("named account must never probe the spawner")
				return pl.Observation{}
			})}}}
			v, err := parse([]string{"gate", "-m", "sonnet", "--account-key", key, "--json"})
			if err != nil {
				t.Fatal(err)
			}
			code, err := app.gate(context.Background(), v)
			if err != nil || code != tc.want || reads != 1 {
				t.Fatalf("code=%d want=%d reads=%d err=%v stderr=%s", code, tc.want, reads, err, errOut.String())
			}
			var receipt struct {
				Schema     string `json:"schema"`
				AccountKey string `json:"account_key"`
				pl.Decision
			}
			if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
				t.Fatal(err)
			}
			baseline := pl.Gate(pl.Inputs{Config: cfg, Now: now, Observations: []pl.Observation{{ID: "claude", Buckets: []pl.Bucket{{Window: "7d", UsedPct: pl.Pct(tc.used), Scope: pl.Scope{Kind: "account"}, ResetsAt: reset.Format(time.RFC3339Nano)}}}}}, pl.Request{Profile: "sonnet"})
			if receipt.Schema != "scopefuel.gate.v1" || receipt.AccountKey != key || receipt.ExitCode != baseline.ExitCode || receipt.OK != baseline.OK || receipt.UsedPct == nil || *receipt.UsedPct != *baseline.UsedPct {
				t.Fatalf("account gate differs from canonical gate: %s baseline=%+v", out.String(), baseline)
			}
		})
	}
}

func TestGateAccountFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "stale", "future", "invalid bucket", "manual kind", "manual only", "http error", "wrong pool", "invalid key", "empty key", "empty equals key", "malformed blocked key", "no backend"} {
		t.Run(mode, func(t *testing.T) {
			isolated(t)
			now := time.Now().UTC()
			reads := 0
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				if r.Method != "GET" {
					t.Error("account gate must be read-only")
				}
				x := pl.SharedLatest{Observation: &pl.SharedObservation{Kind: "auto", MeasuredAt: now, Buckets: []pl.SharedBucket{{Window: "7d", UsedPct: 10, Scope: "account"}}}}
				switch mode {
				case "missing", "manual only":
					x.Observation = nil
				case "stale":
					x.Observation.MeasuredAt = now.Add(-time.Hour)
				case "future":
					x.Observation.MeasuredAt = now.Add(time.Hour)
				case "invalid bucket":
					x.Observation.Buckets[0].UsedPct = 101
				case "manual kind":
					x.Observation.Kind = "manual"
				case "http error":
					w.WriteHeader(503)
					return
				}
				if mode == "manual only" {
					exp := now.Add(time.Hour)
					x.Manual = []pl.SharedObservation{{ID: 1, Kind: "manual", Window: "7d", MeasuredAt: now, ExpiresAt: &exp, Buckets: []pl.SharedBucket{{Window: "7d", UsedPct: 10, Scope: "account"}}}}
				}
				_ = json.NewEncoder(w).Encode(x)
			}))
			defer h.Close()
			t.Setenv("QUOTA_TEST_CREDENTIAL", "fixture-token")
			cfg := pl.Config{Bench: pl.BenchConfig{AllowPlaintextQuotaShare: true}, Quota: pl.QuotaConfig{URL: h.URL, CredentialEnv: "QUOTA_TEST_CREDENTIAL"}}
			if mode == "no backend" {
				cfg.Quota.URL = ""
			}
			var out, errOut bytes.Buffer
			app := App{Config: cfg, Out: &out, Err: &errOut, Collector: pl.Collector{Config: cfg}}
			key := "claude:0123456789abcdef"
			if mode == "wrong pool" {
				key = "codex:0123456789abcdef"
			}
			if mode == "invalid key" || mode == "malformed blocked key" {
				key = "claude:invalid"
			}
			if mode == "empty key" || mode == "empty equals key" {
				key = ""
			}
			if mode == "malformed blocked key" {
				app.Config.Pools = map[string]pl.PoolPolicy{"claude": {Class: "exclude"}}
			}
			argv := []string{"gate", "-m", "sonnet", "--account-key", key, "--json"}
			if mode == "empty equals key" {
				argv = []string{"gate", "-m", "sonnet", "--account-key=", "--json"}
			}
			v, _ := parse(argv)
			code, err := app.gate(context.Background(), v)
			want := pl.Unmeasurable
			if mode == "wrong pool" || mode == "invalid key" || mode == "empty key" || mode == "empty equals key" || mode == "malformed blocked key" {
				want = pl.Invalid
			}
			if err == nil || code != want || strings.Contains(out.String(), `"ok": true`) {
				t.Fatalf("code=%d want=%d err=%v output=%s", code, want, err, out.String())
			}
			if (want == pl.Invalid || mode == "no backend") && reads != 0 {
				t.Fatalf("invalid request made %d reads", reads)
			}
		})
	}
}
