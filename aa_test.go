package pacelever

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func aaFixture(t *testing.T) []AAModel {
	t.Helper()
	data, err := os.ReadFile("testdata/aa_v2_models_20261102.json")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := AAClient{Key: "fixture-key", Client: doFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.String() != aaAPIURL || r.Header.Get("x-api-key") != "fixture-key" {
			t.Fatal("wrong AA request")
		}
		return response(200, string(data)), nil
	})}
	models, err := client.Fetch(context.Background())
	if err != nil || calls != 1 {
		t.Fatalf("fetch: calls=%d err=%v", calls, err)
	}
	return models
}

func TestAAMappingsRecordedVariantsAndGrades(t *testing.T) {
	models := aaFixture(t)
	// Cover every explicit table entry, including profiles only present remotely.
	keys := make([]aaRowKey, 0, len(aaMappings))
	for key := range aaMappings {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b aaRowKey) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
	rows := make([]CatalogRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, CatalogRow{Profile: k.Profile, ModelID: k.ModelID, Effort: k.Effort, Grade: "unrated", Score: ptr(7.5), Gate: "escalation", GateReason: ptr("fleet evidence only"), DecidedBy: "operator", DecidedAt: "2026-11-01T00:00:00Z", DeviationRef: "hk:task/2642"})
	}
	before := slices.Clone(rows)
	r := AnnotateAA(rows, models, testNow)
	if r.Annotated != len(rows) || len(r.Unmapped) != 0 || r.Changed != len(rows) {
		t.Fatalf("%s: %+v", r.Summary(true), r.Unmapped)
	}
	for i, row := range r.Rows {
		var annotation BenchmarkAnnotation
		if json.Unmarshal([]byte(row.BenchmarkAnnotation), &annotation) != nil || annotation.AAName == "" || annotation.FetchedAt != testNow.Format(time.RFC3339Nano) {
			t.Fatalf("annotation missing: %+v", row)
		}
		row.BenchmarkAnnotation, row.BenchmarkSource = "", ""
		if !reflect.DeepEqual(row, before[i]) {
			t.Fatalf("non-benchmark field changed: %+v", row)
		}
	}
	if !reflect.DeepEqual(rows, before) {
		t.Fatal("input catalog mutated")
	}
	second := AnnotateAA(r.Rows, models, testNow.Add(time.Minute))
	if second.Changed != 0 {
		t.Fatalf("timestamp alone counted as changed: %s", second.Summary(false))
	}
	var refreshed BenchmarkAnnotation
	_ = json.Unmarshal([]byte(second.Rows[0].BenchmarkAnnotation), &refreshed)
	if refreshed.FetchedAt != testNow.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Fatal("fetch time did not refresh")
	}
	// A real measurement change is counted even when the source date is equal.
	models[0].Evaluations["artificial_analysis_intelligence_index"] = json.Number("99")
	if changed := AnnotateAA(second.Rows, models, testNow.Add(2*time.Minute)); changed.Changed == 0 {
		t.Fatal("measurement change lost")
	}
}

func TestAAMappingsCoverBundledCoreAndDefaultPins(t *testing.T) {
	core := map[string]bool{"codex-sol": true, "codex-terra": true, "codex-terra61": true, "codex-luna": true, "codex-luna-max": true, "codex-terra-max": true, "opus": true, "sonnet": true, "haiku": true, "fable": true}
	c := BundledCatalog()
	r := AnnotateAA(c.Rows, aaFixture(t), testNow)
	for _, row := range r.Unmapped {
		if core[row.Profile] {
			t.Errorf("core row unmapped: %+v", row)
		}
	}
	for _, pin := range []struct{ profile, effort string }{{"fable", "high"}, {"codex-astra", "xhigh"}, {"codex-luna-max", "max"}, {"kiro-opus", "xhigh"}, {"kiro-sonnet", "high"}, {"kiro-sol", "high"}} {
		if c.Defaults[pin.profile] != pin.effort {
			t.Fatalf("%s default pin drifted", pin.profile)
		}
	}
}

func TestAAPriorsDoNotChangeGradeProposals(t *testing.T) {
	c := BundledCatalog()
	c.Rows = []CatalogRow{{Profile: "sonnet", ModelID: "claude-sonnet-5-5", Effort: "high", Pool: "claude", Grade: "B", Gate: "default", Placed: true}}
	c.Info.Source = BenchServer
	reps := gradeReps()
	for i := range reps {
		reps[i].Profile, reps[i].ModelID = "sonnet", "claude-sonnet-5-5"
	}
	before := ProposeGrades(c, reps, BenchServer, 2, 5, 0.4)
	if len(before.Changes) != 1 {
		t.Fatal("fixture has no fleet grade proposal")
	}
	c.Rows = AnnotateAA(c.Rows, aaFixture(t), testNow).Rows
	after := ProposeGrades(c, reps, BenchServer, 2, 5, 0.4)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("model priors entered fleet grade evidence")
	}
	rows, err := ApplyGrades(before, c, reps, nil, "operator", "hk:task/2642", "", testNow)
	if err != nil || rows[0].Grade != "A" || rows[0].BenchmarkAnnotation != c.Rows[0].BenchmarkAnnotation {
		t.Fatalf("AA refresh invalidated fleet proposal: %v", err)
	}
}

type aaRoundTripper func(*http.Request) (*http.Response, error)

func (f aaRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAAFetchDefaultClientRefusesRedirects(t *testing.T) {
	base := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = base })
	calls := 0
	http.DefaultTransport = aaRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		r := response(302, "private body")
		r.Header.Set("Location", "https://another-origin.example.invalid")
		return r, nil
	})
	_, err := (AAClient{Key: "fixture-key"}).Fetch(context.Background())
	if err == nil || calls != 1 {
		t.Fatalf("followed redirect: calls=%d err=%v", calls, err)
	}
}

func TestAAUnmappedNeverGuesses(t *testing.T) {
	models := aaFixture(t)
	rows := []CatalogRow{
		{Profile: "codex-sol", ModelID: "gpt-6.1-sol-new", Effort: "high"},
		{Profile: "unknown", ModelID: "gpt-6.1-sol", Effort: "high"},
		{Profile: "codex-sol", ModelID: "gpt-6.1-sol", Effort: "ultra"},
		{Profile: "codex-sol", ModelID: "gpt-6.1-sol", Effort: ""},
	}
	// Even an identical name/slug cannot substitute for the pinned ID.
	for i := range models {
		models[i].ID = "new-id-" + models[i].ID
	}
	rows = append(rows, CatalogRow{Profile: "codex-sol", ModelID: "gpt-6.1-sol", Effort: "high", BenchmarkAnnotation: "old annotation", BenchmarkSource: "fleet"})
	r := AnnotateAA(rows, models, testNow)
	if r.Annotated != 0 || len(r.Unmapped) != len(rows) || !reflect.DeepEqual(r.Rows, rows) {
		t.Fatalf("guessed: %+v", r)
	}
	models = aaFixture(t)
	for _, m := range models {
		if m.ID == aaMappings[aaKey(rows[4])] {
			models = append(models, m)
			break
		}
	}
	r = AnnotateAA(rows[4:], models, testNow)
	if r.Annotated != 0 || len(r.Unmapped) != 1 {
		t.Fatal("ambiguous ID guessed")
	}
}

func TestAAMissingMeasurementsStayUnknown(t *testing.T) {
	row := CatalogRow{Profile: "codex-sol", ModelID: "gpt-6.1-sol", Effort: "high"}
	model := AAModel{ID: aaMappings[aaKey(row)], Name: "recorded model", Evaluations: map[string]any{"artificial_analysis_coding_index": json.Number("42.5")}, Pricing: map[string]any{"price_1m_input_tokens": json.Number("0"), "price_1m_output_tokens": json.Number("-1")}, OutputTokensPerSecond: "not a number"}
	r := AnnotateAA([]CatalogRow{row}, []AAModel{model}, testNow)
	var a BenchmarkAnnotation
	if err := json.Unmarshal([]byte(r.Rows[0].BenchmarkAnnotation), &a); err != nil {
		t.Fatal(err)
	}
	if a.IntelligenceIndex != nil || a.OutputTokensPerSecond != nil || a.Price1MOutputTokens != nil || a.Price1MInputTokens == nil || *a.Price1MInputTokens != 0 || a.CodingIndex == nil || *a.CodingIndex != 42.5 {
		t.Fatalf("invented or lost measurements: %+v", a)
	}
}

func TestAAKeyOnlyFromEnvOrFile(t *testing.T) {
	isolate(t)
	t.Setenv(AAAPIKeyName, "")
	_, err := NewAAClient()
	if err == nil || err.Error() != AAAPIKeyName+" is required" {
		t.Fatalf("missing key: %v", err)
	}
	if err := AtomicWrite(filepath.Join(configBase(), "pacelever", "aa.env"), []byte("# ignored\nOTHER=wrong-key\nexport ARTIFICIAL_ANALYSIS_API_KEY='file-placeholder'\n")); err != nil {
		t.Fatal(err)
	}
	c, err := NewAAClient()
	if err != nil || c.Key != "file-placeholder" {
		t.Fatal("file key not read")
	}
	t.Setenv(AAAPIKeyName, " env-placeholder ")
	c, err = NewAAClient()
	if err != nil || c.Key != "env-placeholder" {
		t.Fatal("environment did not take precedence")
	}
}

func TestAAFailuresNeverLeakOrRetry(t *testing.T) {
	for _, status := range []int{301, 401, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			client := AAClient{Key: "sensitive-key", Client: doFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return response(status, "sensitive-key private body"), nil
			})}
			_, err := client.Fetch(context.Background())
			if err == nil || calls != 1 || strings.Contains(err.Error(), "sensitive-key") || !strings.Contains(err.Error(), AAAPIKeyName) {
				t.Fatalf("unsafe failure calls=%d err=%v", calls, err)
			}
		})
	}
	client := AAClient{Key: "sensitive-key", Client: doFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("sensitive-key") })}
	if _, err := client.Fetch(context.Background()); err == nil || strings.Contains(err.Error(), "sensitive-key") {
		t.Fatalf("transport leak: %v", err)
	}
	for _, body := range []string{`{}`, `{"data":[]}`, `{"data":null}`, `{"data":[null]}`, `{"data":[]} trailing`, "sensitive-key"} {
		client.Client = doFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		if _, err := client.Fetch(context.Background()); err == nil {
			t.Fatalf("accepted invalid AA body: %s", body)
		}
	}
}

func TestAAPriorsPersistExactIdentityAndWire(t *testing.T) {
	isolate(t)
	before := BundledCatalog()
	r := AnnotateAA(before.Rows, aaFixture(t), testNow)
	if err := SaveAAPriors(r.Rows); err != nil {
		t.Fatal(err)
	}
	c, err := (BenchClient{}).ReadCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Rows, r.Rows) {
		t.Fatal("local catalog did not attach priors")
	}
	// The store contains only annotations, never grade or gate state.
	data, err := os.ReadFile(aaPriorsPath())
	if err != nil || strings.Contains(string(data), `"grade"`) || strings.Contains(string(data), `"gate"`) {
		t.Fatal("grade state entered prior store")
	}
	for i := range c.Rows {
		if c.Rows[i].Profile != "codex-sol" || c.Rows[i].Effort != "max" {
			continue
		}
		row := c.Rows[i]
		wire := CatalogWire([]CatalogRow{row})
		decoded, err := decodeCatalog(wire)
		if err != nil || decoded[0].BenchmarkAnnotation != row.BenchmarkAnnotation || decoded[0].BenchmarkSource != row.BenchmarkSource {
			t.Fatalf("wire lost prior: %v", err)
		}
		launch, err := ResolveLaunch(c, Config{}, Request{Profile: "codex-sol", Effort: "max", Role: "worker"})
		if err != nil || launch.BenchmarkAnnotation != row.BenchmarkAnnotation {
			t.Fatalf("launch lost prior: %v", err)
		}
		c.Rows[i] = before.Rows[i]
		c.Rows[i].ModelID = "new-model"
		if err := AttachAAPriors(&c); err != nil || c.Rows[i].BenchmarkAnnotation != "" {
			t.Fatal("prior followed changed identity")
		}
	}
}

func TestAAPriorsRetainNewerServerMeasurement(t *testing.T) {
	isolate(t)
	row := CatalogRow{Profile: "sonnet", ModelID: "claude-sonnet-5-5", Effort: "high"}
	models := aaFixture(t)
	local := AnnotateAA([]CatalogRow{row}, models, testNow)
	if err := SaveAAPriors(local.Rows); err != nil {
		t.Fatal(err)
	}
	server := AnnotateAA([]CatalogRow{row}, models, testNow.Add(time.Minute))
	var annotation BenchmarkAnnotation
	_ = json.Unmarshal([]byte(server.Rows[0].BenchmarkAnnotation), &annotation)
	// A newer time can sort before the local stamp when its timezone differs.
	annotation.FetchedAt = testNow.Add(time.Minute).In(time.FixedZone("server", -7*3600)).Format(time.RFC3339Nano)
	data, _ := json.Marshal(annotation)
	server.Rows[0].BenchmarkAnnotation = string(data)
	c := Catalog{Rows: server.Rows}
	if err := AttachAAPriors(&c); err != nil {
		t.Fatal(err)
	}
	if c.Rows[0].BenchmarkAnnotation != string(data) {
		t.Fatal("local prior replaced newer server data")
	}
	older := AnnotateAA([]CatalogRow{row}, models, testNow.Add(-time.Minute))
	c.Rows = older.Rows
	if err := AttachAAPriors(&c); err != nil || c.Rows[0].BenchmarkAnnotation != local.Rows[0].BenchmarkAnnotation {
		t.Fatal("newer local prior was not attached")
	}
}
