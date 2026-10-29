package pacelever

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeUsageFixtures(t *testing.T) {
	for _, tt := range []struct {
		file string
		want []float64
	}{
		{"claude_usage.txt", []float64{6, 10, 0}},
		{"claude_usage_left.txt", []float64{6, 10, 0}},
	} {
		t.Run(tt.file, func(t *testing.T) {
			data, e := os.ReadFile(filepath.Join("testdata", "screens", tt.file))
			if e != nil {
				t.Fatal(e)
			}
			o := ParseClaudeUsageScreen(string(data), testNow)
			if o.Error != "" || o.Source != "cli:/usage" || len(o.Buckets) != len(tt.want) {
				t.Fatalf("invalid result %+v", o)
			}
			for i, want := range tt.want {
				if o.Buckets[i].UsedPct == nil || *o.Buckets[i].UsedPct != want {
					t.Fatalf("bucket %d = %+v want %g", i, o.Buckets[i], want)
				}
			}
			if o.Buckets[0].Window != "5h" || o.Buckets[0].Scope.Kind != "account" || o.Buckets[0].ResetsAt != "2026-10-29T14:40:00Z" {
				t.Fatalf("session bucket %+v", o.Buckets[0])
			}
			if o.Buckets[1].Window != "7d" || o.Buckets[1].Scope.Kind != "account" || o.Buckets[1].ResetsAt != "2026-10-31T17:00:00Z" {
				t.Fatalf("weekly bucket %+v", o.Buckets[1])
			}
			if o.Buckets[2].Window != "7d" || o.Buckets[2].Scope.Kind != "model" || o.Buckets[2].Scope.Name != "Fable" || o.Buckets[2].ResetsAt != "2026-10-31T17:00:00Z" {
				t.Fatalf("model bucket %+v", o.Buckets[2])
			}
		})
	}
}

func TestClaudeUsageRejectsIncompleteOutput(t *testing.T) {
	o := ParseClaudeUsageScreen("Current week (all models): 10% used · resets Nov 1 at 2am (Asia/Seoul)\n", testNow)
	if o.Error == "" || o.ErrorKind != "parse_error" || len(o.Buckets) != 0 {
		t.Fatalf("incomplete output accepted: %+v", o)
	}
}

func TestClaudeProviderCLIOnceAfterAPIFailure(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	fixture, e := os.ReadFile(filepath.Join("testdata", "screens", "claude_usage.txt"))
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	p := ClaudeProvider{
		API: HTTPProvider{ID: "claude", URL: "https://provider.example.invalid/usage", Client: doFunc(func(*http.Request) (*http.Response, error) { return response(429, "do not leak"), nil }), RateLimitWaitS: 300},
		CLI: ClaudeCLIProvider{
			Now: func() time.Time { return testNow },
			Run: func(ctx context.Context, binary, dir string, env map[string]string) (string, error) {
				calls++
				if binary != "fake-claude" || env["CLAUDE_CONFIG_DIR"] != "fixture-config" {
					t.Fatalf("unexpected command context %q %+v", binary, env)
				}
				entries, e := os.ReadDir(dir)
				if e != nil || len(entries) != 0 {
					t.Fatalf("CLI did not receive a dedicated empty directory: %v %v", entries, e)
				}
				return string(fixture), nil
			},
			Binary:  "fake-claude",
			Env:     map[string]string{"CLAUDE_CONFIG_DIR": "fixture-config"},
			Timeout: time.Second,
		},
	}
	o := p.Fetch(context.Background())
	if calls != 1 || o.Error != "" || o.Source != "cli:/usage" || len(o.Buckets) != 3 {
		t.Fatalf("fallback result calls=%d %+v", calls, o)
	}
}

func TestClaudeProviderKeepsAPIFailureWhenCLIFails(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, tt := range []struct {
		name string
		run  func(context.Context, string, string, map[string]string) (string, error)
		kind string
	}{
		{"missing", func(context.Context, string, string, map[string]string) (string, error) {
			return "", errors.New("missing")
		}, "rate_limited"},
		{"parse", func(context.Context, string, string, map[string]string) (string, error) { return "not usage", nil }, "auth"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := ClaudeProvider{
				API: HTTPProvider{ID: "claude", URL: "https://provider.example.invalid/usage", Client: doFunc(func(*http.Request) (*http.Response, error) {
					if tt.kind == "auth" {
						return response(401, "do not leak"), nil
					}
					return response(429, "do not leak"), nil
				}), RateLimitWaitS: 300},
				CLI: ClaudeCLIProvider{Run: tt.run, Now: func() time.Time { return testNow }},
			}
			o := p.Fetch(context.Background())
			if o.Error == "" || o.ErrorKind != tt.kind || o.Source != "usage-api" || len(o.Buckets) != 0 {
				t.Fatalf("CLI failure replaced API failure: %+v", o)
			}
		})
	}
}

func TestClaudeProviderDoesNotRunCLIAfterAPISuccess(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := ClaudeProvider{
		API: HTTPProvider{ID: "claude", URL: "https://provider.example.invalid/usage", Client: doFunc(func(*http.Request) (*http.Response, error) {
			return response(200, `{"five_hour":{"utilization":12,"resets_at":"2026-10-29T00:00:00Z"},"seven_day":{"utilization":20}}`), nil
		})},
		CLI: ClaudeCLIProvider{Run: func(context.Context, string, string, map[string]string) (string, error) {
			t.Fatal("CLI ran after API success")
			return "", nil
		}},
	}
	o := p.Fetch(context.Background())
	if o.Error != "" || o.Source != "usage-api" || len(o.Buckets) != 2 {
		t.Fatalf("API success not returned: %+v", o)
	}
}

func TestClaudeAccountFingerprintAppliesToCLIResult(t *testing.T) {
	dir := t.TempDir()
	identity := filepath.Join(dir, ".claude.json")
	if e := os.WriteFile(identity, []byte(`{"oauthAccount":{"accountUuid":"fixture-account"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	o := claudeAccountFingerprint(ProviderFunc(func(context.Context) Observation {
		return Observation{ID: "claude", Source: "cli:/usage", Buckets: []Bucket{{Window: "7d", UsedPct: Pct(10)}}}
	}), identity, true).Fetch(context.Background())
	if o.Source != "cli:/usage" || o.AccountFP == "" || o.AccountFPKind != "account" {
		t.Fatalf("CLI result lost account fingerprint: %+v", o)
	}
}
