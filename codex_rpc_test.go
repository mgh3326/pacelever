package pacelever

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRPC replays recorded JSON-RPC lines; no test spawns a real CLI.
type fakeRPC struct {
	lines  []string
	sent   []string
	recvAt int
	sendE  error
	closed bool
}

func (f *fakeRPC) Send(b []byte) error {
	if f.sendE != nil {
		return f.sendE
	}
	f.sent = append(f.sent, string(b))
	return nil
}

func (f *fakeRPC) Recv() ([]byte, error) {
	if f.recvAt >= len(f.lines) {
		return nil, errors.New("fixture exhausted")
	}
	line := f.lines[f.recvAt]
	f.recvAt++
	return []byte(line), nil
}

func (f *fakeRPC) Close() { f.closed = true }

func stubRPC(t *testing.T, f *fakeRPC, e error) {
	t.Helper()
	// The path is never executed — the transport is stubbed — but binary
	// resolution must succeed on machines without a codex CLI.
	t.Setenv("PACELEVER_CODEX_BIN", "fixture-codex")
	prev := codexRPCStart
	codexRPCStart = func(context.Context, string, string, string) (codexRPCTransport, error) {
		if e != nil {
			return nil, e
		}
		return f, nil
	}
	t.Cleanup(func() { codexRPCStart = prev })
}

func TestCodexRPCMapsRateLimits(t *testing.T) {
	f := &fakeRPC{lines: []string{
		`{"jsonrpc":"2.0","method":"sessionConfigured"}`, // notification skipped
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"rateLimits":{"planType":"pro","primary":{"usedPercent":94,"resetsAt":1790500839,"windowDurationMins":10080},"secondary":{"usedPercent":12,"resetsAt":1789980000,"windowDurationMins":300}},"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":1}},"codex_spark":{"limitName":"Codex Spark","primary":{"usedPercent":40,"resetsAt":1789980000,"windowDurationMins":300},"secondary":{"usedPercent":5,"windowDurationMins":10080}}}}}`,
		`{"id":3,"result":{"account":{"email":"fixture@example.invalid"}}}`,
	}}
	stubRPC(t, f, nil)
	o, ok := fetchCodexRPC(context.Background(), t.TempDir(), Config{}, t.TempDir())
	if !ok || o.Error != "" || o.Source != "usage-rpc" || o.Plan != "pro" {
		t.Fatalf("rpc observation %+v ok=%v", o, ok)
	}
	if len(o.Buckets) != 4 {
		t.Fatalf("buckets %+v", o.Buckets)
	}
	if o.Buckets[0].Window != "7d" || *o.Buckets[0].UsedPct != 94 {
		t.Fatalf("primary window (a weekly window is legal) %+v", o.Buckets[0])
	}
	if o.Buckets[1].Window != "5h" || o.Buckets[1].ResetsAt == "" {
		t.Fatalf("secondary %+v", o.Buckets[1])
	}
	if o.Buckets[2].Scope.Name != "Spark" || o.Buckets[3].Scope.Name != "Spark" {
		t.Fatalf("per-limit names %+v", o.Buckets)
	}
	if o.PublishFP != fingerprint("codex-account", "fixture@example.invalid") {
		t.Fatalf("identity fingerprint %q", o.PublishFP)
	}
	if len(f.sent) != 4 || !strings.Contains(f.sent[0], `"initialize"`) || !strings.Contains(f.sent[1], `"initialized"`) || !strings.Contains(f.sent[2], `"account/rateLimits/read"`) || !strings.Contains(f.sent[3], `"account/read"`) {
		t.Fatalf("exchange order %+v", f.sent)
	}
	if !f.closed {
		t.Fatal("transport not closed")
	}
}

func TestCodexRPCFailures(t *testing.T) {
	for name, lines := range map[string][]string{
		"auth error":          {`{"id":1,"result":{}}`, `{"id":2,"error":{"code":-32600,"message":"codex account authentication required to read rate limits"}}`},
		"empty non-free plan": {`{"id":1,"result":{}}`, `{"id":2,"result":{"rateLimits":{"planType":"pro"}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRPC{lines: lines}
			stubRPC(t, f, nil)
			if _, ok := fetchCodexRPC(context.Background(), t.TempDir(), Config{}, t.TempDir()); ok {
				t.Fatalf("%s must fail so the API fallback runs", name)
			}
		})
	}
}

func TestCodexRPCFreePlan(t *testing.T) {
	f := &fakeRPC{lines: []string{
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"rateLimits":{"planType":"free"}}}`,
		`{"id":3,"result":{"account":null,"requiresOpenaiAuth":false}}`,
	}}
	stubRPC(t, f, nil)
	o, ok := fetchCodexRPC(context.Background(), t.TempDir(), Config{}, t.TempDir())
	if !ok || len(o.Buckets) != 1 || *o.Buckets[0].UsedPct != 0 || o.Plan != "free" {
		t.Fatalf("free plan %+v ok=%v", o, ok)
	}
}

func TestCodexLimitNameShortInputs(t *testing.T) {
	for in, want := range map[string]string{
		"o3":          "o3",
		"Codex":       "Codex",
		"codex":       "codex",
		"codex_spark": "Spark",
		"Codex Spark": "Spark",
	} {
		if got := codexLimitName(in); got != want {
			t.Fatalf("codexLimitName(%q)=%q want %q", in, got, want)
		}
	}
}

func codexAuthHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := `{"tokens":{"access_token":"fixture-token","account_id":"fixture-acct"}}`
	if e := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	return dir
}

func TestFetchCodexRPCPreferredAndAPIFallback(t *testing.T) {
	dir := codexAuthHome(t)
	sel := map[string]string{"CODEX_HOME": dir}
	called := 0
	api := doFunc(func(r *http.Request) (*http.Response, error) {
		called++
		return response(200, `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000}}}`), nil
	})

	// RPC answers: the API is never called.
	f := &fakeRPC{lines: []string{
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":55,"windowDurationMins":300,"resetsAt":1789980000}}}}`,
		`{"id":3,"result":{"account":{"email":"fixture@example.invalid"}}}`,
	}}
	stubRPC(t, f, nil)
	o := fetchCodex(context.Background(), t.TempDir(), sel, Config{}, api, nil)
	if called != 0 || o.Source != "usage-rpc" || *o.Buckets[0].UsedPct != 55 {
		t.Fatalf("rpc path %+v calls=%d", o, called)
	}
	if o.PublishFP != fingerprint("codex-account", "fixture-acct") {
		t.Fatalf("file credential identity must win over rpc email %q", o.PublishFP)
	}

	// RPC fails: the API fallback answers.
	stubRPC(t, nil, errors.New("no binary"))
	o = fetchCodex(context.Background(), t.TempDir(), sel, Config{}, api, nil)
	if called != 1 || o.Source != "usage-api" || *o.Buckets[0].UsedPct != 25 {
		t.Fatalf("api fallback %+v calls=%d", o, called)
	}
}

func TestFetchCodexNoTokenNoSpawn(t *testing.T) {
	dir := t.TempDir()
	spawned := false
	prev := codexRPCStart
	codexRPCStart = func(context.Context, string, string, string) (codexRPCTransport, error) {
		spawned = true
		return &fakeRPC{}, nil
	}
	t.Cleanup(func() { codexRPCStart = prev })
	o := fetchCodex(context.Background(), t.TempDir(), map[string]string{"CODEX_HOME": dir}, Config{}, nil, nil)
	if spawned || o.ErrorKind != "credentials" {
		t.Fatalf("no login must not spawn the CLI %+v spawned=%v", o, spawned)
	}
}

func TestCodexAPIHeadersResetAfterAndCredits(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	body := `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":1793232000}},"credits":{"has_credits":true,"balance":"250"}}`
	p := HTTPProvider{ID: "codex", URL: "https://provider.example.invalid/usage", Now: func() time.Time { return now }, Client: doFunc(func(*http.Request) (*http.Response, error) {
		r := response(200, body)
		r.Header.Set("x-codex-primary-used-percent", "77")
		return r, nil
	})}
	o := p.Fetch(context.Background())
	if o.Error != "" || len(o.Buckets) != 3 {
		t.Fatalf("codex api %+v", o)
	}
	if *o.Buckets[0].UsedPct != 77 {
		t.Fatalf("header percent must override body %+v", o.Buckets[0])
	}
	if o.Buckets[0].ResetsAt != now.Add(time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("reset_after_seconds %q", o.Buckets[0].ResetsAt)
	}
	credits := o.Buckets[2]
	if credits.Label != "credits" || *credits.UsedPct != 75 || !strings.Contains(credits.Note, "250.00") {
		t.Fatalf("credits bucket %+v", credits)
	}
}
