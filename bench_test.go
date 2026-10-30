package pacelever

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeBench(t *testing.T, f doFunc) BenchClient {
	t.Helper()
	isolate(t)
	return BenchClient{URL: "https://catalog.example.invalid", Token: "test-placeholder", Config: Config{Bench: BenchConfig{Backend: "handoffkeep"}}, Client: f, Now: func() time.Time { return testNow }}
}
func TestBenchCatalogLadder(t *testing.T) {
	calls := 0
	fail := false
	b := fakeBench(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if fail {
			return nil, fmt.Errorf("sensitive endpoint error")
		}
		if !strings.Contains(r.URL.RawQuery, "include_retired") {
			t.Fatal("retirement query missing")
		}
		return response(200, `{"catalog":[{"profile":"sonnet","effort":"high","model_id":"claude-sonnet-5-5","pool":"claude","grade":"A","gate":"default"}]}`), nil
	})
	c, e := b.ReadCatalog(context.Background())
	if e != nil || c.Info.Source != "server" {
		t.Fatalf("server: %v %+v", e, c.Info)
	}
	if len(c.Rows) < 2 {
		t.Fatal("partial canon lost unseeded profiles")
	}
	_, e = b.ReadCatalog(context.Background())
	if e != nil || calls != 1 {
		t.Fatalf("cache miss %d %v", calls, e)
	}
	fail = true
	b.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	c, e = b.ReadCatalog(context.Background())
	if e != nil || c.Info.Source != "cache" || c.Info.Stale {
		t.Fatalf("last-good cache: %v %+v", e, c.Info)
	}
	b.Now = func() time.Time { return testNow.Add(48 * time.Hour) }
	c, e = b.ReadCatalog(context.Background())
	if e != nil || c.Info.Source != "cache-stale" || !c.Info.Stale {
		t.Fatalf("stale cache: %v %+v", e, c.Info)
	}
	b.URL = "https://different.example.invalid"
	c, e = b.ReadCatalog(context.Background())
	if e == nil || c.Info.Source != "snapshot" || !c.Info.Stale {
		t.Fatalf("endpoint binding failed: %v %+v", e, c.Info)
	}
}
func TestBenchWireWhitelist(t *testing.T) {
	b := fakeBench(t, func(r *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(r.Body)
		var v struct {
			Catalog []map[string]any `json:"catalog"`
		}
		if json.Unmarshal(data, &v) != nil {
			t.Fatal("invalid wire")
		}
		for _, row := range v.Catalog {
			for _, key := range []string{"placed", "family", "billing", "group"} {
				if _, ok := row[key]; ok {
					t.Fatalf("local field %s reached server", key)
				}
			}
		}
		return response(200, `{"upserted":1}`), nil
	})
	r := CatalogRow{Profile: "example", ModelID: "example-model", Effort: "high", Pool: "claude", Grade: "A", Gate: "default", Placed: true, DecidedAt: testNow.Format(time.RFC3339), DecidedBy: "operator", DeviationRef: "hk:task/1"}
	if e := b.PushCatalog(context.Background(), []CatalogRow{r}); e != nil {
		t.Fatal(e)
	}
}
func TestBackendCredentialIsolation(t *testing.T) {
	isolate(t)
	config := filepath.Join(configBase(), "handoffkeep/config.env")
	if e := AtomicWrite(config, []byte("HANDOFFKEEP_URL=https://stored.example.invalid\nHANDOFFKEEP_TOKEN=test-placeholder\nHANDOFFKEEP_CF_ACCESS_CLIENT_ID=placeholder-id\nHANDOFFKEEP_CF_ACCESS_CLIENT_SECRET=placeholder-secret\n")); e != nil {
		t.Fatal(e)
	}
	t.Setenv("HANDOFFKEEP_CONFIG", config)
	t.Setenv("HANDOFFKEEP_URL", "https://override.example.invalid")
	t.Setenv("HANDOFFKEEP_TOKEN", "")
	b := NewBenchClient(Config{})
	if b.Token != "" || b.CFClientSecret != "" {
		t.Fatal("stored credentials followed env endpoint")
	}
	b.Token = "test-placeholder"
	b.URL = "http://backend.example.invalid"
	if b.allowed("catalog") == nil {
		t.Fatal("plaintext allowed without opt-in")
	}
	b.Config.Bench.AllowPlaintextReps = true
	if b.allowed("reps") != nil || b.allowed("catalog") == nil {
		t.Fatal("per-use opt-ins crossed")
	}
}
func TestRepWriteAheadRetry(t *testing.T) {
	fail := true
	var ids []int64
	b := fakeBench(t, func(r *http.Request) (*http.Response, error) {
		var body struct {
			Reps []Rep `json:"reps"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		ids = append(ids, body.Reps[0].OriginID)
		if fail {
			return response(409, "private failure body"), nil
		}
		return response(200, `{"ids":[42],"upserted":1}`), nil
	})
	r, e := b.AddRep(context.Background(), Rep{Profile: "sonnet", ModelID: "claude-sonnet-5-5", TaskRef: "hk:task/1", Tier: "T1", Role: "impl", Grade: "A", Rounds: 1, Completed: ptr(1), Effort: "high"})
	if e == nil {
		t.Fatal("conflict silently succeeded")
	}
	rows, e2 := localReps()
	if e2 != nil || len(rows) != 1 || rows[0].OriginID != r.OriginID {
		t.Fatal("intent lost")
	}
	fail = false
	retried, e := b.RetryRep(context.Background(), r)
	if e != nil || retried.ID != 42 || len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("retry changed identity: %+v %v", ids, e)
	}
}

// A stored or foreign rep whose outcome fields fail validation is listed as
// pending instead of refusing the whole listing.
func TestRepsListToleratesInvalidStoredOutcome(t *testing.T) {
	isolate(t)
	good := Rep{LocalID: 1, Profile: "sonnet", ModelID: "model-a", TaskRef: "task-a", Notes: `{"experiment_arm":"downshift","experiment_id":"e-1"}`, RecordedAt: testNow.Format(time.RFC3339)}
	bad := Rep{LocalID: 2, Profile: "sonnet", ModelID: "model-a", TaskRef: "task-b", Notes: `{"experiment_arm":"control"}`, RecordedAt: testNow.Add(time.Minute).Format(time.RFC3339)}
	if e := WriteJSON(repsPath(), []Rep{good, bad}); e != nil {
		t.Fatal(e)
	}
	rows, source, e := (BenchClient{}).ReadReps(context.Background(), "")
	if e != nil || source != BenchLocal || len(rows) != 2 {
		t.Fatalf("stored outcome blocked listing: %v %q %+v", e, source, rows)
	}
	for _, r := range rows {
		wantPending := r.LocalID == 2
		if r.Pending != wantPending {
			t.Fatalf("pending mismatch: %+v", r)
		}
	}
}

// With no remote configured nothing was read, so ReadRows reports no usable
// source with its error; ReadReps answers from the local reps alone, and a
// configured handoffkeep backend that is unreachable stays an error.
func TestBenchSourceMatchesError(t *testing.T) {
	isolate(t)
	rows, source, e := (BenchClient{}).ReadRows(context.Background(), "reps")
	if e == nil || source != BenchUnavailable || rows != nil {
		t.Fatalf("no backend: %v %q %+v", e, source, rows)
	}
	if ex, e := (BenchClient{}).ReadAreaExceptions(context.Background()); e != nil || ex != nil {
		t.Fatalf("local-only area exceptions: %v %+v", e, ex)
	}
	var cfg Config
	cfg.Bench.Backend = "handoffkeep"
	if _, e = (BenchClient{Config: cfg}).ReadAreaExceptions(context.Background()); e == nil {
		t.Fatal("unreachable handoffkeep area exceptions read as empty")
	}
	_, source, e = (BenchClient{Config: cfg}).ReadReps(context.Background(), "")
	if e == nil || source != BenchUnavailable {
		t.Fatalf("unreachable handoffkeep: %v %q", e, source)
	}
}
func TestBundledCatalogMonotonic(t *testing.T) {
	if e := ValidateMonotonic(BundledCatalog().Rows); e != nil {
		t.Fatal(e)
	}
}

func TestValidateMonotonicReportsFirstProfileByName(t *testing.T) {
	var rows []CatalogRow
	for _, profile := range []string{"zeta", "mid", "alpha"} {
		rows = append(rows, CatalogRow{Profile: profile, Effort: "low", Grade: "A"}, CatalogRow{Profile: profile, Effort: "high", Grade: "B"})
	}
	for range 20 {
		e := ValidateMonotonic(rows)
		if e == nil || !strings.Contains(e.Error(), "for alpha") {
			t.Fatalf("violation not reported for the first profile by name: %v", e)
		}
	}
}

func TestDecodeCatalogInfersAgyGroupForNewProfiles(t *testing.T) {
	row := func(profile, model string) map[string]any {
		return map[string]any{"profile": profile, "effort": "", "model_id": model, "pool": "agy", "grade": "B", "gate": "default"}
	}
	rows, err := decodeCatalog([]map[string]any{
		row("agy-new-flash", "gemini-9-flash-high"),
		row("agy-new-sonnet", "claude-sonnet-4-6"),
		row("agy-new-oss", "gpt-oss-120b-medium"),
		row("agy-new-mystery", "mystery-model"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"agy-new-flash": "gemini", "agy-new-sonnet": "3p", "agy-new-oss": "3p", "agy-new-mystery": ""}
	for _, r := range rows {
		if r.Group != want[r.Profile] {
			t.Errorf("%s group=%q want %q", r.Profile, r.Group, want[r.Profile])
		}
	}
}
