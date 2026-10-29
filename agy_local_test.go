package pacelever

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

var agyLocalArgv = "/Applications/Antigravity.app/Contents/Resources/language_server_macos_arm --app_data_dir /Users/x/Library/Application Support/antigravity --csrf_token fixture-csrf --extension_server_port 5678"

func agyLocalProvider(localClient, cloudClient HTTPDoer, procs []agyLocalProc, ports []int) agyProvider {
	return agyProvider{
		client:     cloudClient,
		file:       func() string { return "fixture-token" },
		localProcs: func(context.Context) []agyLocalProc { return procs },
		localPorts: func(context.Context, int) []int { return ports },
		localHTTP:  localClient,
	}
}

func TestAgyLocalQuotaSummary(t *testing.T) {
	var sawPath, sawCSRF, sawProto, sawHost string
	local := doFunc(func(r *http.Request) (*http.Response, error) {
		sawPath, sawHost = r.URL.Path, r.URL.Host
		sawCSRF = r.Header.Get("X-Codeium-Csrf-Token")
		sawProto = r.Header.Get("Connect-Protocol-Version")
		return response(200, `{"response":{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.8,"resetTime":"2026-10-29T06:18:55Z"},{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.6,"resetTime":"2026-11-05T01:18:55Z"}]}]}}`), nil
	})
	cloud := doFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("cloud source ran while the app answered")
		return nil, nil
	})
	p := agyLocalProvider(local, cloud, []agyLocalProc{{pid: 42, argv: agyLocalArgv}}, nil)
	o := p.Fetch(context.Background())
	if o.Error != "" || len(o.Buckets) != 2 {
		t.Fatalf("local buckets %+v", o)
	}
	if sawHost != "127.0.0.1:5678" || !strings.HasSuffix(sawPath, "/RetrieveUserQuotaSummary") {
		t.Fatalf("local request %s%s", sawHost, sawPath)
	}
	if sawCSRF != "fixture-csrf" || sawProto != "1" {
		t.Fatalf("connect headers csrf=%q proto=%q", sawCSRF, sawProto)
	}
	if o.Buckets[0].Scope.Name != "gemini" || o.Buckets[0].Window != "5h" || !pctClose(o.Buckets[0].UsedPct, 20) {
		t.Fatalf("local five-hour bucket %+v", o.Buckets[0])
	}
	if o.Source != "usage-api" {
		t.Fatalf("local observation source %q", o.Source)
	}
}

func TestAgyLocalHTTPSPortsBeforeExtensionPort(t *testing.T) {
	var order []string
	local := doFunc(func(r *http.Request) (*http.Response, error) {
		order = append(order, r.URL.Scheme+"://"+r.URL.Host+r.URL.Path)
		if r.URL.Port() == "9000" {
			return response(200, `{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","window":"5h","remainingFraction":0.5,"resetTime":"2026-10-29T06:18:55Z"}]}]}`), nil
		}
		return response(404, `{}`), nil
	})
	p := agyLocalProvider(local, nil, []agyLocalProc{{pid: 7, argv: agyLocalArgv}}, []int{9000})
	o := p.Fetch(context.Background())
	if o.Error != "" || len(o.Buckets) != 1 || o.Buckets[0].Scope.Name != "3p" {
		t.Fatalf("https fallback %+v", o)
	}
	if len(order) == 0 || !strings.HasPrefix(order[0], "https://127.0.0.1:9000/") {
		t.Fatalf("loopback order %v", order)
	}
	for _, u := range order {
		if !strings.HasPrefix(u, "https://127.0.0.1:") && !strings.HasPrefix(u, "http://127.0.0.1:") {
			t.Fatalf("csrf token sent off loopback: %s", u)
		}
	}
}

func TestAgyLocalUserStatusShape(t *testing.T) {
	local := doFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/RetrieveUserQuotaSummary"):
			return response(404, `{}`), nil
		case strings.HasSuffix(r.URL.Path, "/GetUserStatus"):
			return response(200, `{"userStatus":{"email":"x@example.invalid","planStatus":{"planInfo":{"planName":"pro"}},"cascadeModelConfigData":{"clientModelConfigs":[{"label":"Gemini 3.8 Flash","quotaInfo":{"remainingFraction":0.9,"resetTime":"2026-10-29T06:18:55Z"}},{"label":"Claude Sonnet 4.6","quotaInfo":{"remainingFraction":0.25,"resetTime":"2026-10-29T06:18:55Z"}}]}}}`), nil
		}
		return response(404, `{}`), nil
	})
	p := agyLocalProvider(local, nil, []agyLocalProc{{pid: 9, argv: agyLocalArgv}}, nil)
	o := p.Fetch(context.Background())
	if o.Error != "" || len(o.Buckets) != 2 {
		t.Fatalf("userStatus buckets %+v", o)
	}
	if len(matching(o.Buckets, "gemini")) != 1 || len(matching(o.Buckets, "3p")) != 1 {
		t.Fatalf("userStatus groups %+v", o.Buckets)
	}
	if o.Plan != "PRO" {
		t.Fatalf("userStatus plan %q", o.Plan)
	}
}

func TestAgyLocalFallsBackToCloud(t *testing.T) {
	for name, procs := range map[string][]agyLocalProc{
		"none":      nil,
		"no-csrf":   {{pid: 1, argv: "/Applications/Antigravity.app/language_server --app_data_dir /antigravity/ --extension_server_port 5678"}},
		"other-app": {{pid: 1, argv: "/opt/other/language_server --csrf_token fixture-csrf --extension_server_port 5678"}},
		"agy-cli":   {{pid: 1, argv: "agy models --csrf_token fixture-csrf"}},
	} {
		t.Run(name, func(t *testing.T) {
			local := doFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("a request left for a process that should be skipped")
				return nil, nil
			})
			cloud := doFunc(func(*http.Request) (*http.Response, error) {
				return response(200, `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.5,"resetTime":"2026-10-29T06:18:55Z"}]}]}`), nil
			})
			p := agyLocalProvider(local, cloud, procs, nil)
			o := p.Fetch(context.Background())
			if o.Error != "" || len(o.Buckets) != 1 {
				t.Fatalf("cloud fallback %+v", o)
			}
		})
	}
}

func TestAgyLocalRefusalFallsBackToCloud(t *testing.T) {
	local := doFunc(func(*http.Request) (*http.Response, error) {
		return response(500, `{}`), nil
	})
	var cloudCalls int
	cloud := doFunc(func(*http.Request) (*http.Response, error) {
		cloudCalls++
		return response(200, `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.5,"resetTime":"2026-10-29T06:18:55Z"}]}]}`), nil
	})
	p := agyLocalProvider(local, cloud, []agyLocalProc{{pid: 3, argv: agyLocalArgv}}, nil)
	o := p.Fetch(context.Background())
	if o.Error != "" || cloudCalls == 0 {
		t.Fatalf("refused local did not reach the cloud: %+v calls=%d", o, cloudCalls)
	}
}

func TestAgyLocalUnparseableAnswerFallsBackToCloud(t *testing.T) {
	local := doFunc(func(*http.Request) (*http.Response, error) {
		return response(200, `{"unrelated":true}`), nil
	})
	cloud := doFunc(func(*http.Request) (*http.Response, error) {
		return response(200, `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.5,"resetTime":"2026-10-29T06:18:55Z"}]}]}`), nil
	})
	p := agyLocalProvider(local, cloud, []agyLocalProc{{pid: 4, argv: agyLocalArgv}}, nil)
	o := p.Fetch(context.Background())
	if o.Error != "" || len(o.Buckets) != 1 {
		t.Fatalf("unparseable local did not reach the cloud: %+v", o)
	}
}

func TestAgyLocalIsServer(t *testing.T) {
	for _, tt := range []struct {
		argv string
		want bool
	}{
		{agyLocalArgv, true},
		{"/usr/local/bin/agy --app_data_dir /tmp/antigravity", true},
		{"/opt/codeium/language_server_macos --csrf_token x", false},
		{"language_server --manager_dir /tmp/other", false},
		{"grep language_server", false},
		{"agy models --csrf_token fixture-csrf --extension_server_port 5678", false},
	} {
		if got := agyLocalIsServer(tt.argv); got != tt.want {
			t.Fatalf("agyLocalIsServer(%q) = %v, want %v", tt.argv, got, tt.want)
		}
	}
}
