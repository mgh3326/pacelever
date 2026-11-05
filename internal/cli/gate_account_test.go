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
			now := pl.ClockNow(nil)
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
			now := pl.ClockNow(nil)
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

// runtug gates a credit seat with --account-key. A credit account has only the
// operator's shared credit entry, so that entry is the judgment: live admits,
// exhausted blocks, missing or expired is unmeasurable. Nothing is probed,
// leased or published.
func TestGateAccountCreditPool(t *testing.T) {
	for _, tc := range []struct {
		name    string
		used    float64
		expires time.Duration
		manual  bool
		want    int
	}{
		{"live credit", 20.04, 14 * 24 * time.Hour, true, pl.Admitted},
		{"exhausted credit", 100, 14 * 24 * time.Hour, true, pl.Blocked},
		{"expired credit", 20, -time.Minute, true, pl.Unmeasurable},
		{"no credit entry", 0, 0, false, pl.Unmeasurable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolated(t)
			now := pl.ClockNow(nil)
			expiry := now.Add(tc.expires)
			key := "anthropic-api:fedcba9876543210"
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/quota/accounts/"+key+"/latest" {
					t.Errorf("unexpected shared request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				latest := pl.SharedLatest{}
				if tc.manual {
					latest.Manual = []pl.SharedObservation{{ID: 9, Kind: "manual", Window: "credit", MeasuredAt: now.Add(-time.Hour), ExpiresAt: &expiry, Buckets: []pl.SharedBucket{{Window: "credit", UsedPct: tc.used, Scope: "account"}}}}
				}
				_ = json.NewEncoder(w).Encode(latest)
			}))
			defer h.Close()
			t.Setenv("QUOTA_TEST_CREDENTIAL", "fixture-token")
			cfg := pl.Config{Bench: pl.BenchConfig{AllowPlaintextQuotaShare: true}, Quota: pl.QuotaConfig{URL: h.URL, CredentialEnv: "QUOTA_TEST_CREDENTIAL", Accounts: map[string]string{"anthropic-api": "fedcba9876543210"}}}
			var out, errOut bytes.Buffer
			app := App{Config: cfg, Out: &out, Err: &errOut, Collector: pl.Collector{Config: cfg}}
			v, err := parse([]string{"gate", "-m", "pi-sonnet55", "--effort", "high", "--role", "worker", "--account-key", key, "--json"})
			if err != nil {
				t.Fatal(err)
			}
			code, err := app.gate(context.Background(), v)
			if err != nil || code != tc.want {
				t.Fatalf("code=%d want=%d err=%v out=%s stderr=%s", code, tc.want, err, out.String(), errOut.String())
			}
		})
	}
}

// An exhausted local credit entry never decides a named credit account: with
// --account-key the gate reads no local manual store, so the key's live shared
// entry admits (review r4437955482).
func TestGateAccountCreditIgnoresLocalExhaustion(t *testing.T) {
	isolated(t)
	now := pl.ClockNow(nil)
	local := pl.ManualObservation{Pool: "anthropic-api", MeasuredAt: now.Format(time.RFC3339), Reason: "console", Credit: &pl.Credit{RemainingUSD: 0, TotalUSD: 200, ExpiresOn: now.Add(14 * 24 * time.Hour).Format(time.RFC3339)}}
	if _, err := pl.AddManual(local, 0, now); err != nil {
		t.Fatal(err)
	}
	if d := pl.Gate(pl.Inputs{Now: now, Manual: mustLoadManual(t)}, pl.Request{Profile: "pi-sonnet55", Effort: "high", Role: "worker"}); d.ExitCode != pl.Blocked {
		t.Fatalf("fixture: local entry should be exhausted, got %d %s", d.ExitCode, d.Reason)
	}
	expiry := now.Add(14 * 24 * time.Hour)
	key := "anthropic-api:fedcba9876543210"
	reads := 0
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		_ = json.NewEncoder(w).Encode(pl.SharedLatest{Manual: []pl.SharedObservation{{ID: 9, Kind: "manual", Window: "credit", MeasuredAt: now.Add(-time.Hour), ExpiresAt: &expiry, Buckets: []pl.SharedBucket{{Window: "credit", UsedPct: 20, Scope: "account"}}}}})
	}))
	defer h.Close()
	t.Setenv("QUOTA_TEST_CREDENTIAL", "fixture-token")
	cfg := pl.Config{Bench: pl.BenchConfig{AllowPlaintextQuotaShare: true}, Quota: pl.QuotaConfig{URL: h.URL, CredentialEnv: "QUOTA_TEST_CREDENTIAL", Accounts: map[string]string{"anthropic-api": "fedcba9876543210"}}}
	var out, errOut bytes.Buffer
	app := App{Config: cfg, Out: &out, Err: &errOut, Collector: pl.Collector{Config: cfg}}
	v, err := parse([]string{"gate", "-m", "pi-sonnet55", "--effort", "high", "--role", "worker", "--account-key", key, "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := app.gate(context.Background(), v); err != nil || code != pl.Admitted || reads != 1 {
		t.Fatalf("code=%d reads=%d err=%v out=%s", code, reads, err, out.String())
	}
}

func mustLoadManual(t *testing.T) pl.ManualStore {
	t.Helper()
	s, err := pl.LoadManual()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
