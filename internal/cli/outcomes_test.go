package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	pl "github.com/mgh3326/pacelever"
)

type httpFunc func(*http.Request) (*http.Response, error)

func (f httpFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func httpReply(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}
func TestMergewakeStructuredNotesAndTextReceipts(t *testing.T) {
	isolated(t)
	var posted map[string]any
	cfg := pl.Config{Bench: pl.BenchConfig{Backend: "handoffkeep"}}
	client := httpFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/bench/catalog":
			return httpReply(`{"catalog":[{"profile":"sonnet","effort":"high","model_id":"claude-sonnet-5-5","pool":"claude","grade":"A","gate":"default"}]}`), nil
		case r.Method == "PUT" && r.URL.Path == "/v1/bench/reps":
			var payload struct {
				Reps []map[string]any `json:"reps"`
			}
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				t.Fatal("bad rep request")
			}
			posted = payload.Reps[0]
			return httpReply(`{"ids":[42]}`), nil
		case r.Method == "GET" && r.URL.Path == "/v1/bench/reps":
			copy := map[string]any{}
			for k, v := range posted {
				copy[k] = v
			}
			copy["id"] = 42
			data, _ := json.Marshal(map[string]any{"reps": []any{copy}})
			return httpReply(string(data)), nil
		}
		return nil, fmt.Errorf("unexpected fixture request")
	})
	var out, err bytes.Buffer
	app := App{Out: &out, Err: &err, Config: cfg, Bench: pl.BenchClient{Config: cfg, URL: "https://catalog.example.invalid", Token: "test-placeholder", Client: client}}
	notes := `{"source":"mergewake","followup":false,"outcome":{"outcome_id":"fixture-outcome","decision":"landed","finalized":true,"first_ci_green":true,"findings_by_severity":{"low":1},"reverted_within_7d":false,"time_to_merge_s":300,"experiment_id":"cohort-fixture","arm":"downshift"}}`
	v, e := parse([]string{"reps", "add", "--profile", "sonnet", "--model", "claude-sonnet-5-5", "--effort", "high", "--task", "hk:task/1", "--tier", "T1", "--role", "impl", "--grade", "A", "--notes", notes})
	if e != nil {
		t.Fatal(e)
	}
	code, e := app.run(context.Background(), v)
	if code != 0 || e != nil || strings.TrimSpace(out.String()) != "recorded rep id=srv:42" {
		t.Fatalf("receipt %d %v out=%s err=%s", code, e, out.String(), err.String())
	}
	if _, exists := posted["first_ci_green"]; exists {
		t.Fatal("new field sent outside backward-compatible notes")
	}
	out.Reset()
	v, _ = parse([]string{"reps", "list", "--profile", "sonnet", "--limit", "100000"})
	code, e = app.run(context.Background(), v)
	if e != nil || code != 0 || !strings.HasPrefix(out.String(), "id=srv:42 ") {
		t.Fatalf("list %d %v %s", code, e, out.String())
	}
	i := strings.Index(out.String(), " notes=")
	if i < 0 {
		t.Fatal("notes missing")
	}
	var envelope struct {
		Source  string `json:"source"`
		Outcome struct {
			ID string `json:"outcome_id"`
		} `json:"outcome"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(out.String()[i+7:])), &envelope) != nil || envelope.Source != "mergewake" || envelope.Outcome.ID != "fixture-outcome" {
		t.Fatalf("publisher cannot decode receipt notes: %s", out.String())
	}
	out.Reset()
	v, _ = parse([]string{"reps", "list", "--json"})
	code, e = app.run(context.Background(), v)
	var jsonReply struct {
		Reps []pl.Rep `json:"reps"`
	}
	if e != nil || code != 0 || json.Unmarshal(out.Bytes(), &jsonReply) != nil || len(jsonReply.Reps) != 1 || jsonReply.Reps[0].ExperimentArm != "downshift" || jsonReply.Reps[0].FirstCIGreen == nil {
		t.Fatalf("first-class JSON fields missing: %s %v", out.String(), e)
	}
}
func TestPolicyLaunchExplorationAndLabel(t *testing.T) {
	isolated(t)
	cfg := pl.Config{Pools: map[string]pl.PoolPolicy{"claude": {Exploration: pl.ExplorationPolicy{Fraction: 1, ExperimentID: "cohort-fixture"}}}}
	now := time.Now().UTC()
	var out, err bytes.Buffer
	app := App{Out: &out, Err: &err, Config: cfg, Bench: pl.BenchClient{}, Collector: pl.Collector{Config: cfg, Providers: map[string]pl.Provider{"claude": pl.ProviderFunc(func(context.Context) pl.Observation {
		return pl.Observation{ID: "claude", Source: "fixture", Buckets: []pl.Bucket{{Window: "5h", UsedPct: pl.Pct(10), Scope: pl.Scope{Kind: "account"}, ResetsAt: now.Add(3 * time.Hour).Format(time.RFC3339)}, {Window: "7d", UsedPct: pl.Pct(10), Scope: pl.Scope{Kind: "account"}, ResetsAt: now.Add(144 * time.Hour).Format(time.RFC3339)}}}
	})}}}
	v, _ := parse([]string{"policy", "launch", "sonnet", "--effort", "high", "--role", "worker", "--task", "hk:task/1", "--grade", "C", "--json"})
	code, e := app.run(context.Background(), v)
	if e != nil || code != 0 {
		t.Fatalf("launch %d %v %s", code, e, err.String())
	}
	var d pl.LaunchDecision
	if json.Unmarshal(out.Bytes(), &d) != nil || d.Profile != "sonnet" || d.Effort != "medium" || d.Label.Arm != "downshift" || d.Label.ExperimentID == nil {
		t.Fatalf("experiment label missing: %s", out.String())
	}
}
