package pacelever

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type doFunc func(*http.Request) (*http.Response, error)

func (f doFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}
func TestHTTPProviders(t *testing.T) {
	for _, tt := range []struct {
		id, body string
		windows  int
		first    float64
	}{
		{"claude", `{"five_hour":{"utilization":12,"resets_at":"2026-10-29T00:00:00Z"},"seven_day":{"utilization":20}}`, 2, 12},
		{"codex", `{"plan_type":"prolite","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_at":1793232000}},"additional_rate_limits":[{"limit_name":"a-model","rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":18000}}}]}`, 2, 25},
		{"clinepass", `{"data":{"limits":[{"type":"weekly","percentUsed":50,"resetsAt":"2026-11-01T00:00:00Z"}]}}`, 1, 50},
	} {
		t.Run(tt.id, func(t *testing.T) {
			p := HTTPProvider{ID: tt.id, URL: "https://provider.example.invalid/usage", Client: doFunc(func(*http.Request) (*http.Response, error) { return response(200, tt.body), nil })}
			o := p.Fetch(context.Background())
			if o.Error != "" || len(o.Buckets) != tt.windows || *o.Buckets[0].UsedPct != tt.first {
				t.Fatalf("invalid result %+v", o)
			}
		})
	}
}
func TestProviderErrorsRedacted(t *testing.T) {
	sentinel := "credential-value-must-not-escape"
	p := HTTPProvider{ID: "claude", URL: "https://provider.example.invalid/usage", Client: doFunc(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("%s", sentinel) })}
	o := p.Fetch(context.Background())
	if strings.Contains(o.Error, sentinel) || o.ErrorKind != "network" {
		t.Fatal("unsafe network error")
	}
	p.Client = doFunc(func(*http.Request) (*http.Response, error) { return response(429, sentinel), nil })
	o = p.Fetch(context.Background())
	if strings.Contains(o.Error, sentinel) || o.ErrorKind != "rate_limited" {
		t.Fatal("unsafe HTTP error")
	}
}
func TestCommandcodeExplicitBinary(t *testing.T) {
	isolate(t)
	cfg := Config{Pools: map[string]PoolPolicy{"commandcode": {Bin: "/configured/cmd"}}}
	if ResolveBinary("commandcode", cfg) != "/configured/cmd" {
		t.Fatal("config ignored")
	}
	t.Setenv("PACELEVER_COMMANDCODE_BIN", "/explicit/cmd")
	if ResolveBinary("commandcode", cfg) != "/explicit/cmd" {
		t.Fatal("explicit path ignored")
	}
}

func TestOAuthChoiceWithDecodedNumbers(t *testing.T) {
	expired := map[string]any{"accessToken": "fixture-file-placeholder", "expiresAt": json.Number("1")}
	fresh := map[string]any{"accessToken": "fixture-keychain-placeholder", "expiresAt": float64(testNow.Add(time.Hour).UnixMilli())}
	chosen := chooseOAuth(expired, fresh, testNow)
	if chosen["expiresAt"] != fresh["expiresAt"] {
		t.Fatal("expired file preferred over current keychain context")
	}
	expires, ok := finiteNumber(json.Number("1000"))
	if !ok || expires != 1000 {
		t.Fatal("JSON expiry lost numeric meaning")
	}
}
