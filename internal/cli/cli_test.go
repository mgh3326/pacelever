package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	pl "github.com/mgh3326/pacelever"
)

func isolated(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("HANDOFFKEEP_CONFIG", filepath.Join(base, "absent.env"))
	for _, key := range []string{"PACELEVER_CONFIG", "PACELEVER_CACHE", "HANDOFFKEEP_URL", "HANDOFFKEEP_TOKEN", "PACELEVER_E6_ARM"} {
		t.Setenv(key, "")
	}
}
func TestHelp(t *testing.T) {
	var out, err bytes.Buffer
	code := Run(context.Background(), []string{"--help"}, &out, &err, strings.NewReader(""), "dev", "unknown")
	if code != 0 || !strings.Contains(out.String(), "5 role-denied; 6 unsubscribed") {
		t.Fatalf("help %d %s", code, out.String())
	}
}
func TestLaunchCLIGoldens(t *testing.T) {
	isolated(t)
	data, e := os.ReadFile("../../testdata/scopefuel_golden.json")
	if e != nil {
		t.Fatal(e)
	}
	var fixture struct {
		Cases []struct {
			Request    pl.Request `json:"request"`
			State      string     `json:"state"`
			LaunchCode int        `json:"launch_code"`
			Launch     any        `json:"launch"`
			Change     string     `json:"intentional_change"`
		} `json:"cases"`
	}
	if e = json.Unmarshal(data, &fixture); e != nil {
		t.Fatal(e)
	}
	for _, tt := range fixture.Cases {
		if tt.State != "" {
			continue
		}
		t.Run(tt.Request.Profile+"/"+tt.Request.Effort+"/"+tt.Request.Role, func(t *testing.T) {
			argv := []string{"policy", "launch", tt.Request.Profile, "--json"}
			if tt.Request.Effort != "" {
				argv = append(argv, "--effort", tt.Request.Effort)
			}
			if tt.Request.Role != "" {
				argv = append(argv, "--role", tt.Request.Role)
			}
			if tt.Request.Purpose != "" {
				argv = append(argv, "--purpose", tt.Request.Purpose)
			}
			var out, err bytes.Buffer
			code := Run(context.Background(), argv, &out, &err, strings.NewReader(""), "dev", "unknown")
			if tt.Change != "" {
				if code != 0 {
					t.Fatalf("builder launch %d: %s", code, err.String())
				}
				return
			}
			if code != tt.LaunchCode {
				t.Fatalf("code %d want %d: %s", code, tt.LaunchCode, err.String())
			}
			if code == 0 {
				var got any
				if json.Unmarshal(out.Bytes(), &got) != nil {
					t.Fatalf("invalid launch JSON: %s", out.String())
				}
				m := got.(map[string]any)
				label, ok := m["label"].(map[string]any)
				if !ok || label["arm"] != "baseline" || label["profile"] != m["profile"] || label["effort"] != m["effort"] {
					t.Fatal("baseline label missing")
				}
				delete(m, "label")
				if !reflect.DeepEqual(got, tt.Launch) {
					t.Fatalf("JSON differs: %s", out.String())
				}
			}
		})
	}
}
func TestGateStreamsAndAudit(t *testing.T) {
	isolated(t)
	now := time.Now().UTC()
	var out, err bytes.Buffer
	app := App{Out: &out, Err: &err, Commit: "unknown", Bench: pl.BenchClient{}, Collector: pl.Collector{Providers: map[string]pl.Provider{"claude": pl.ProviderFunc(func(context.Context) pl.Observation {
		return pl.Observation{ID: "claude", Buckets: []pl.Bucket{{Label: "7d", Window: "7d", UsedPct: pl.Pct(10), Scope: pl.Scope{Kind: "account"}, ResetsAt: now.Add(5 * 24 * time.Hour).Format(time.RFC3339)}}}
	})}}}
	path := filepath.Join(t.TempDir(), "gate.json")
	v, _ := parse([]string{"gate", "-m", "sonnet", "--gate-output", path})
	code, e := app.run(context.Background(), v)
	if code != 0 || e != nil {
		t.Fatalf("gate %d %v %s", code, e, err.String())
	}
	if !strings.HasPrefix(out.String(), "profile=sonnet pool=claude used_pct=10.0 class=spend\n") {
		t.Fatalf("arbiter header changed: %s", out.String())
	}
	var audit map[string]any
	if e = pl.ReadJSON(path, &audit); e != nil || audit["schema"] != "scopefuel.gate.v1" {
		t.Fatal("audit missing")
	}
	out.Reset()
	err.Reset()
	v, _ = parse([]string{"gate", "-m", "codex-astra", "--role", "worker"})
	code, e = app.run(context.Background(), v)
	if e != nil || code != 5 || out.Len() != 0 || err.Len() == 0 {
		t.Fatalf("denial streams %d %v out=%s err=%s", code, e, out.String(), err.String())
	}
}
func TestStatusProviderContract(t *testing.T) {
	now := time.Now().UTC()
	payload := statusPayload(pl.Inputs{Now: now, Observations: []pl.Observation{{ID: "claude", Source: "fixture", Buckets: []pl.Bucket{{Window: "7d", UsedPct: pl.Pct(10)}}}}})
	data, _ := json.Marshal(payload)
	var parsed struct {
		Schema    string `json:"schema"`
		Providers []struct {
			ID      string  `json:"id"`
			Status  string  `json:"status"`
			Source  *string `json:"source"`
			Buckets []struct {
				Window   string   `json:"window"`
				UsedPct  *float64 `json:"used_pct"`
				ResetsAt *string  `json:"resets_at"`
			} `json:"buckets"`
		} `json:"providers"`
	}
	if json.Unmarshal(data, &parsed) != nil || parsed.Schema != "scopefuel.v1" || parsed.Providers[0].Status != "ok" {
		t.Fatalf("quota collector contract: %s", data)
	}
}
func TestGateSwe2FreeBucket(t *testing.T) {
	isolated(t)
	app := App{Commit: "unknown", Bench: pl.BenchClient{}, Collector: pl.Collector{Providers: map[string]pl.Provider{}}}
	gate := func(args ...string) (int, string, string) {
		var out, err bytes.Buffer
		app.Out, app.Err = &out, &err
		v, _ := parse(append([]string{"gate", "-m", "devin-swe2", "--role", "worker"}, args...))
		code, e := app.run(context.Background(), v)
		if e != nil {
			t.Fatal(e)
		}
		return code, out.String(), err.String()
	}
	if code, out, _ := gate(); code != 0 || !strings.Contains(out, `free_tag="available (free)"`) || !strings.Contains(out, "available (free)") {
		t.Fatalf("no record: %d %s", code, out)
	}
	observe := func(args ...string) string {
		var out, err bytes.Buffer
		app.Out, app.Err = &out, &err
		v, _ := parse(append([]string{"observe", "devin-free-limit"}, args...))
		if code, e := app.run(context.Background(), v); code != 0 || e != nil {
			t.Fatalf("observe %d %v", code, e)
		}
		return err.String()
	}
	// An unreadable reset holds the rung for the conservative default.
	now := time.Now().UTC()
	log := observe("--unparsed", "--observed", now.Format(time.RFC3339), "--rung", "devin-swe2", "--source", "job:fixture")
	hold := now.Add(pl.UnknownResetHold).Format(time.RFC3339)
	if !strings.Contains(log, "reset time unknown") || !strings.Contains(log, hold) {
		t.Fatalf("unknown reset not logged: %q", log)
	}
	code, _, stderr := gate()
	if code != 3 || !strings.HasPrefix(stderr, "free limit exceeded until "+hold) {
		t.Fatalf("active exceed: %d %q", code, stderr)
	}
	app.Out = &bytes.Buffer{}
	var jsonOut bytes.Buffer
	app.Out, app.Err = &jsonOut, &bytes.Buffer{}
	v, _ := parse([]string{"gate", "-m", "devin-swe2", "--role", "worker", "--json"})
	if code, e := app.run(context.Background(), v); code != 3 || e != nil || !strings.Contains(jsonOut.String(), "free limit exceeded until "+hold) {
		t.Fatalf("json: %d %v %s", code, e, jsonOut.String())
	}
	observe("--clear")
	if code, _, _ := gate(); code != 0 {
		t.Fatalf("cleared: %d", code)
	}
}
func TestObserveFreeLimitNeverStoresEmptyReset(t *testing.T) {
	isolated(t)
	app := App{Commit: "unknown", Bench: pl.BenchClient{}, Collector: pl.Collector{Providers: map[string]pl.Provider{}}}
	run := func(args ...string) error {
		app.Out, app.Err = &bytes.Buffer{}, &bytes.Buffer{}
		v, _ := parse(append([]string{"observe", "devin-free-limit"}, args...))
		_, e := app.run(context.Background(), v)
		return e
	}
	gateCode := func() int {
		app.Out, app.Err = &bytes.Buffer{}, &bytes.Buffer{}
		v, _ := parse([]string{"gate", "-m", "devin-swe2", "--role", "worker"})
		code, _ := app.run(context.Background(), v)
		return code
	}
	for _, args := range [][]string{
		{"--unparsed", "--observed", "garbage", "--source", "job:x"},
		{"--observed", "garbage", "--source", "job:x"},
		{"--source", "job:x"},
		{"--unparsed", "--reset", "garbage", "--source", "job:x"},
	} {
		if e := run(args...); e == nil {
			t.Fatalf("%v stored silently", args)
		}
		if gateCode() != 0 {
			t.Fatalf("%v left a record behind", args)
		}
	}
	// No --observed with an unreadable reset: observed defaults to now.
	if e := run("--unparsed", "--source", "job:x"); e != nil {
		t.Fatal(e)
	}
	if gateCode() != 3 {
		t.Fatal("default hold not applied")
	}
	var s struct {
		Records map[string]pl.LimitRecord `json:"records"`
	}
	if e := pl.ReadJSON(filepath.Join(pl.CacheDir(), "devin-free-limit.json"), &s); e != nil || len(s.Records) != 1 {
		t.Fatalf("store: %v %+v", e, s)
	}
	for _, r := range s.Records {
		if r.ResetAt == "" || r.ObservedAt == "" {
			t.Fatalf("empty time stored: %+v", r)
		}
	}
}
func TestFreeTagTextKeepsLegacyFormat(t *testing.T) {
	for tag, want := range map[string]string{"Free": "Free", "observed": "observed", "available (free)": `"available (free)"`} {
		if got := freeTagText(tag); got != want {
			t.Fatalf("%q: %s", tag, got)
		}
	}
}
