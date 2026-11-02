package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	pl "github.com/mgh3326/pacelever"
)

func aaTestApp(t *testing.T, out, errOut *bytes.Buffer) (*App, *int) {
	t.Helper()
	isolated(t)
	fixture, err := os.ReadFile("../../testdata/aa_v2_models_20261102.json")
	if err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	client := &pl.AAClient{Key: "fixture-placeholder", Client: httpFunc(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.Header.Get("x-api-key") != "fixture-placeholder" {
			t.Fatal("AA key header missing")
		}
		return httpReply(string(fixture)), nil
	})}
	return &App{Out: out, Err: errOut, AA: client, Now: func() time.Time { return time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC) }}, calls
}

func TestAASyncCLIDryRunAndPersistence(t *testing.T) {
	var out, errOut bytes.Buffer
	app, calls := aaTestApp(t, &out, &errOut)
	v, _ := parse([]string{"bench", "aa-sync", "--dry-run", "--json"})
	code, err := app.bench(context.Background(), v)
	if err != nil || code != 0 || *calls != 1 {
		t.Fatalf("dry-run: code=%d calls=%d err=%v", code, *calls, err)
	}
	var preview struct {
		Catalog   []pl.CatalogRow    `json:"catalog"`
		Annotated int                `json:"annotated"`
		Changed   int                `json:"changed"`
		Unmapped  []pl.AAUnmappedRow `json:"unmapped"`
	}
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil || preview.Annotated == 0 || len(preview.Unmapped) == 0 || preview.Changed != preview.Annotated {
		t.Fatalf("invalid preview: %v %s", err, out.String())
	}
	base, err := app.Bench.ReadCatalog(context.Background())
	if err != nil || !reflect.DeepEqual(base.Rows, pl.BundledCatalog().Rows) {
		t.Fatal("dry-run persisted annotations")
	}
	if !strings.Contains(errOut.String(), fmt.Sprintf("aa-sync annotated=%d unmapped=%d changed=%d dry_run=true\n", preview.Annotated, len(preview.Unmapped), preview.Changed)) {
		t.Fatalf("missing summary: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	path := filepath.Join(t.TempDir(), "catalog.json")
	cfg := pl.Config{Bench: pl.BenchConfig{Backend: "handoffkeep"}}
	payload, _ := json.Marshal(map[string]any{"catalog": pl.CatalogWire(pl.BundledCatalog().Rows)})
	app.Bench = pl.BenchClient{Config: cfg, URL: "https://recorded-catalog.example.invalid", Token: "fixture-placeholder", Now: app.Now, Client: httpFunc(func(*http.Request) (*http.Response, error) { return httpReply(string(payload)), nil })}
	v, _ = parse([]string{"bench", "aa-sync", "--out", path})
	code, err = app.bench(context.Background(), v)
	if err != nil || code != 0 || *calls != 2 {
		t.Fatalf("sync: %d %v", code, err)
	}
	if !strings.Contains(out.String(), "unmapped devin-fusion-sol61@") || !strings.Contains(out.String(), "dry_run=false\n") {
		t.Fatalf("missing text report: %s", out.String())
	}
	var saved struct {
		Catalog []pl.CatalogRow `json:"catalog"`
	}
	if err := pl.ReadJSON(path, &saved); err != nil || !reflect.DeepEqual(saved.Catalog, preview.Catalog) {
		t.Fatalf("artifact lost metadata: %v", err)
	}
	c, err := app.Bench.ReadCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range c.Rows {
		if row.BenchmarkAnnotation != preview.Catalog[i].BenchmarkAnnotation || row.Grade != base.Rows[i].Grade {
			t.Fatalf("persistence changed wrong fields: %+v", row)
		}
	}
	// Prove catalog and launch JSON expose the prior the planner can read.
	for _, argv := range [][]string{{"bench", "catalog", "list", "--json"}, {"policy", "launch", "sonnet", "--effort", "high", "--role", "worker", "--json"}} {
		out.Reset()
		v, _ := parse(argv)
		code, err := app.run(context.Background(), v)
		if err != nil || code != 0 || !strings.Contains(out.String(), `"benchmark_annotation"`) || !strings.Contains(out.String(), `"benchmark_source": "artificialanalysis:v2:2026-11-02"`) {
			t.Fatalf("prior not exposed: %d %v %s", code, err, out.String())
		}
	}
	out.Reset()
	errOut.Reset()
	v, _ = parse([]string{"bench", "aa-sync", "--dry-run"})
	code, err = app.bench(context.Background(), v)
	if code != 0 || err != nil || !strings.Contains(out.String(), "changed=0 dry_run=true") {
		t.Fatalf("unchanged sync: %d %v %s", code, err, out.String())
	}
}

func TestAACatalogListGolden(t *testing.T) {
	var out, errOut bytes.Buffer
	app, _ := aaTestApp(t, &out, &errOut)
	v, _ := parse([]string{"bench", "aa-sync"})
	if _, err := app.bench(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	v, _ = parse([]string{"bench", "catalog", "list"})
	if _, err := app.bench(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	path := "../../testdata/aa_catalog_list.golden"
	if os.Getenv("UPDATE_AA_GOLDEN") == "1" {
		if err := os.WriteFile(path, out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Fatalf("catalog output differs from golden:\n%s", out.String())
	}
}

func TestAASyncMissingKeyExit(t *testing.T) {
	isolated(t)
	t.Setenv(pl.AAAPIKeyName, "")
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"bench", "aa-sync", "--dry-run"}, &out, &errOut, strings.NewReader(""), "test", "test")
	if code != pl.Invalid || out.Len() != 0 || errOut.String() != "error: ARTIFICIAL_ANALYSIS_API_KEY is required\n" {
		t.Fatalf("missing-key contract: code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
}

func TestAASyncFailureDoesNotPersist(t *testing.T) {
	var out, errOut bytes.Buffer
	app, calls := aaTestApp(t, &out, &errOut)
	app.AA.Client = httpFunc(func(*http.Request) (*http.Response, error) { *calls++; return nil, fmt.Errorf("fixture-placeholder") })
	v, _ := parse([]string{"bench", "aa-sync"})
	code, err := app.bench(context.Background(), v)
	if code != pl.Invalid || err == nil || strings.Contains(err.Error(), "fixture-placeholder") || *calls != 1 {
		t.Fatalf("unsafe failure: %d %v", code, err)
	}
	c, _ := app.Bench.ReadCatalog(context.Background())
	if !reflect.DeepEqual(c.Rows, pl.BundledCatalog().Rows) {
		t.Fatal("failed sync changed catalog")
	}
}

func TestAAExportRefusesSnapshotAndCacheFallbacks(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		remote, seed bool
		age          time.Duration
	}{
		{name: "local snapshot"},
		{name: "unavailable server", remote: true},
		{name: "fresh cache with unavailable server", remote: true, seed: true},
		{name: "last-good cache", remote: true, seed: true, age: 2 * time.Hour},
		{name: "stale cache", remote: true, seed: true, age: 48 * time.Hour},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			app, aaCalls := aaTestApp(t, &out, &errOut)
			if scenario.remote {
				cfg := pl.Config{Bench: pl.BenchConfig{Backend: "handoffkeep"}}
				payload, _ := json.Marshal(map[string]any{"catalog": pl.CatalogWire(pl.BundledCatalog().Rows)})
				available := true
				now := app.now()
				app.Bench = pl.BenchClient{Config: cfg, URL: "https://recorded-catalog.example.invalid", Token: "fixture-placeholder", Now: func() time.Time { return now }, Client: httpFunc(func(*http.Request) (*http.Response, error) {
					if !available {
						return nil, fmt.Errorf("server unavailable")
					}
					return httpReply(string(payload)), nil
				})}
				if scenario.seed {
					if _, err := app.Bench.ReadCatalog(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				available = false
				now = now.Add(scenario.age)
			}
			path := filepath.Join(t.TempDir(), "catalog.json")
			v, _ := parse([]string{"bench", "aa-sync", "--out", path})
			code, err := app.bench(context.Background(), v)
			if code != pl.Invalid || err == nil || *aaCalls != 0 {
				t.Fatalf("unsafe export accepted: code=%d err=%v AA calls=%d", code, err, *aaCalls)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("refused export wrote an artifact")
			}
			if _, err := os.Stat(filepath.Join(os.Getenv("XDG_DATA_HOME"), "pacelever", "aa-priors.json")); !os.IsNotExist(err) {
				t.Fatal("refused export persisted priors")
			}
		})
	}
}

func TestAAExportRefreshesServerGradesGatesAndRetirements(t *testing.T) {
	var out, errOut bytes.Buffer
	app, aaCalls := aaTestApp(t, &out, &errOut)
	rows := pl.BundledCatalog().Rows
	cfg := pl.Config{Bench: pl.BenchConfig{Backend: "handoffkeep"}}
	serverCalls := 0
	app.Bench = pl.BenchClient{Config: cfg, URL: "https://recorded-catalog.example.invalid", Token: "fixture-placeholder", Now: app.Now, Client: httpFunc(func(r *http.Request) (*http.Response, error) {
		serverCalls++
		if r.Method != "GET" {
			t.Fatal("export attempted a catalog write")
		}
		payload, _ := json.Marshal(map[string]any{"catalog": pl.CatalogWire(rows)})
		return httpReply(string(payload)), nil
	})}
	if _, err := app.Bench.ReadCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if rows[i].Profile == "sonnet" && rows[i].Effort == "high" {
			rows[i].Grade = "B"
			rows[i].Gate = "escalation"
			stamp := "2026-11-02T04:59:00Z"
			rows[i].RetiredAt = &stamp
		}
	}
	path := filepath.Join(t.TempDir(), "catalog.json")
	v, _ := parse([]string{"bench", "aa-sync", "--dry-run", "--out", path})
	code, err := app.bench(context.Background(), v)
	if code != 0 || err != nil || serverCalls != 2 || *aaCalls != 1 {
		t.Fatalf("export did not refresh catalog: code=%d err=%v server=%d AA=%d", code, err, serverCalls, *aaCalls)
	}
	var saved struct {
		Catalog []pl.CatalogRow `json:"catalog"`
	}
	if err := pl.ReadJSON(path, &saved); err != nil {
		t.Fatal(err)
	}
	for _, row := range saved.Catalog {
		if row.Profile == "sonnet" && row.Effort == "high" {
			if row.Grade != "B" || row.Gate != "escalation" || row.RetiredAt == nil {
				t.Fatalf("export reverted server policy: %+v", row)
			}
			return
		}
	}
	t.Fatal("retired server row missing from export")
}

func TestAACorruptPriorWarningAndRepair(t *testing.T) {
	var out, errOut bytes.Buffer
	app, _ := aaTestApp(t, &out, &errOut)
	path := filepath.Join(os.Getenv("XDG_DATA_HOME"), "pacelever", "aa-priors.json")
	if err := pl.AtomicWrite(path, []byte("truncated prior cache")); err != nil {
		t.Fatal(err)
	}
	v, _ := parse([]string{"policy", "launch", "sonnet", "--effort", "high", "--role", "worker", "--json"})
	code, err := app.run(context.Background(), v)
	if code != 0 || err != nil || !strings.Contains(out.String(), `"profile": "sonnet"`) {
		t.Fatalf("optional cache broke launch: %d %v", code, err)
	}
	if !strings.Contains(errOut.String(), "warning: cannot read Artificial Analysis priors") || strings.Contains(errOut.String(), "catalog unavailable") {
		t.Fatalf("wrong cache warning: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	v, _ = parse([]string{"bench", "aa-sync"})
	code, err = app.bench(context.Background(), v)
	if code != 0 || err != nil {
		t.Fatalf("sync could not repair optional cache: %d %v", code, err)
	}
	var priors []map[string]any
	if err := pl.ReadJSON(path, &priors); err != nil || len(priors) == 0 {
		t.Fatalf("cache not repaired: %v", err)
	}
}
