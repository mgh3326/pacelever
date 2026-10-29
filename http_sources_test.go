package pacelever

import (
	"context"
	"io"
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
}
