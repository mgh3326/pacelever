package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pl "github.com/mgh3326/pacelever"
)

type benchDoFunc func(*http.Request) (*http.Response, error)

func (f benchDoFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestUnratedRemoteProfileNotRecommended(t *testing.T) {
	isolated(t)
	var out bytes.Buffer
	a := App{Out: &out, Bench: pl.BenchClient{URL: "https://catalog.example.invalid", Token: "fixture", Client: benchDoFunc(func(*http.Request) (*http.Response, error) {
		return httpReply(`{"catalog":[{"profile":"omp-terra61","effort":"high","model_id":"openai/gpt-6.1-terra","pool":"openai-api","grade":"unrated","gate":"default"}]}`), nil
	})}}
	v, err := parse([]string{"--list-recommend-profiles"})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := a.run(context.Background(), v); code != 0 || err != nil {
		t.Fatalf("recommend profile list: code=%d err=%v", code, err)
	}
	if strings.Contains(out.String(), "omp-terra61") || !strings.Contains(out.String(), "sonnet") {
		t.Fatalf("remote unrated profile entered placed-profile list: %s", out.String())
	}
}

func TestUnratedRepAddCLI(t *testing.T) {
	for _, grade := range []string{"A", "unrated"} {
		for _, dry := range []bool{false, true} {
			t.Run(grade+map[bool]string{true: "/dry-run"}[dry], func(t *testing.T) {
				isolated(t)
				var out bytes.Buffer
				a := App{Out: &out}
				argv := []string{"reps", "add", "--profile", "codex-sol", "--model", "gpt-6.1-sol", "--effort", "high", "--task", "hk:task/2639", "--tier", "T1", "--role", "impl", "--grade", grade, "--rounds", "1", "--blockers-found", "0", "--completed", "1", "--json"}
				if dry {
					argv = append(argv, "--dry-run")
				}
				v, err := parse(argv)
				if err != nil {
					t.Fatal(err)
				}
				code, err := a.reps(context.Background(), v)
				if grade == "unrated" {
					if code != pl.Invalid || err == nil || !strings.Contains(err.Error(), "invalid rep grade") || out.Len() != 0 {
						t.Fatalf("unrated task accepted: code=%d err=%v out=%s", code, err, out.String())
					}
				} else if code != 0 || err != nil || !strings.Contains(out.String(), `"table_grade": "unrated"`) {
					t.Fatalf("rated task on unrated model rejected: code=%d err=%v out=%s", code, err, out.String())
				}
			})
		}
	}
}

func TestBenchCatalogListsUnrated(t *testing.T) {
	isolated(t)
	for _, asJSON := range []bool{false, true} {
		var out bytes.Buffer
		a := App{Out: &out}
		argv := []string{"bench", "catalog", "list"}
		if asJSON {
			argv = append(argv, "--json")
		}
		v, err := parse(argv)
		if err != nil {
			t.Fatal(err)
		}
		if code, err := a.bench(context.Background(), v); code != 0 || err != nil {
			t.Fatalf("catalog list: code=%d err=%v", code, err)
		}
		if asJSON {
			var body struct{ Catalog []pl.CatalogRow }
			if err := json.Unmarshal(out.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, row := range body.Catalog {
				if row.Grade == "unrated" {
					count++
				}
			}
			if count != 3 {
				t.Fatalf("bundled unrated rows=%d want=3", count)
			}
		} else if !strings.Contains(out.String(), "unrated codex-sol@low ") {
			t.Fatalf("catalog text omitted unrated: %s", out.String())
		}
	}
}

func TestBenchUnratedWrites(t *testing.T) {
	for _, command := range []string{"push-catalog", "grades set"} {
		for _, dry := range []bool{false, true} {
			for _, grade := range []string{"unrated", "unknown"} {
				t.Run(command+"/"+grade+map[bool]string{true: "/dry-run"}[dry], func(t *testing.T) {
					isolated(t)
					var out bytes.Buffer
					calls := 0
					a := App{Out: &out, Bench: pl.BenchClient{URL: "https://catalog.example.invalid", Token: "fixture", Client: benchDoFunc(func(r *http.Request) (*http.Response, error) {
						calls++
						var body map[string][]map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						key := "catalog"
						if command == "grades set" {
							key = "grades"
							if r.URL.Path != "/v1/bench/grades" {
								t.Fatalf("wrong grade endpoint: %s", r.URL.Path)
							}
						}
						if len(body[key]) != 1 || body[key][0]["grade"] != "unrated" {
							t.Fatalf("bad grade payload: %v", body)
						}
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"upserted":1}`))}, nil
					})}}
					argv := []string{"bench", "grades", "set", "--profile", "example", "--grade", grade, "--deviation-ref", "hk:task/2639"}
					if command == "push-catalog" {
						path := filepath.Join(t.TempDir(), "catalog.json")
						data := `{"catalog":[{"profile":"example","effort":"high","model_id":"example-model","pool":"claude","grade":"` + grade + `","gate":"default","decided_by":"operator","decided_at":"2026-11-02T00:00:00Z","deviation_ref":"hk:task/2639"}]}`
						if err := os.WriteFile(path, []byte(data), 0600); err != nil {
							t.Fatal(err)
						}
						argv = []string{"bench", "push-catalog", path}
					}
					if dry {
						argv = append(argv, "--dry-run")
					}
					v, err := parse(argv)
					if err != nil {
						t.Fatal(err)
					}
					code, err := a.bench(context.Background(), v)
					if grade == "unknown" {
						if err == nil || calls != 0 {
							t.Fatalf("invalid grade accepted: code=%d calls=%d err=%v", code, calls, err)
						}
					} else if code != 0 || err != nil || (dry && (calls != 0 || !strings.Contains(out.String(), `"grade": "unrated"`))) || (!dry && calls != 1) {
						t.Fatalf("unrated refused: code=%d calls=%d out=%s err=%v", code, calls, out.String(), err)
					}
				})
			}
		}
	}
}
