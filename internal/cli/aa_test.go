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
