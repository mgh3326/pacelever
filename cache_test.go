package pacelever

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCollectorSingleFlight(t *testing.T) {
	isolate(t)
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var calls atomic.Int32
	c := Collector{Providers: map[string]Provider{"claude": ProviderFunc(func(context.Context) Observation {
		calls.Add(1)
		close(started)
		<-release
		return observation("claude", 10)
	})}, Now: func() time.Time { return testNow }}
	done := make(chan error, 1)
	go func() { _, e := c.Collect(context.Background(), []string{"claude"}); done <- e }()
	<-started
	second, e := c.Collect(context.Background(), []string{"claude"})
	if e != nil || second[0].ErrorKind != "probe_in_progress" || calls.Load() != 1 {
		t.Fatalf("single-flight failed: %+v %v", second, e)
	}
	close(release)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	fresh, e := c.Collect(context.Background(), []string{"claude"})
	if e != nil || fresh[0].Error != "" || calls.Load() != 1 {
		t.Fatal("fresh cache did not suppress provider")
	}
}
func TestCommandcodeCollectsSynchronously(t *testing.T) {
	isolate(t)
	var probed atomic.Int32
	c := Collector{Providers: map[string]Provider{"commandcode": ProviderFunc(func(context.Context) Observation { probed.Add(1); return observation("commandcode", 1) })}, Now: func() time.Time { return testNow }}
	rows, e := c.Collect(context.Background(), []string{"commandcode"})
	if e != nil || probed.Load() != 1 || rows[0].Error != "" {
		t.Fatalf("command code did not collect synchronously %+v %v", rows, e)
	}
}
func TestLegacySnapshotCompatibility(t *testing.T) {
	isolate(t)
	legacy := legacyCachePath()
	entry := map[string]CacheEntry{"claude": {FetchedAt: float64(testNow.Unix()), Result: observation("claude", 10)}}
	if e := WriteJSON(legacy, entry); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(legacy)
	var called bool
	c := Collector{Providers: map[string]Provider{"claude": ProviderFunc(func(context.Context) Observation { called = true; return observation("claude", 50) })}, Now: func() time.Time { return testNow }}
	rows, e := c.Collect(context.Background(), []string{"claude"})
	if e != nil || called || *rows[0].Buckets[0].UsedPct != 10 {
		t.Fatalf("legacy miss %v", e)
	}
	after, _ := os.ReadFile(legacy)
	if string(before) != string(after) {
		t.Fatal("legacy snapshot changed")
	}
	if _, e = os.Stat(filepath.Join(CacheDir(), "snapshots.json")); !os.IsNotExist(e) {
		t.Fatal("read created cache")
	}
}

func TestLegacyBackoffDoesNotBlockOwnProvider(t *testing.T) {
	isolate(t)
	entry := `{"fetched_at":%d,"result":{"id":"%s","buckets":[{"window":"7d","used_pct":10,"scope":{"kind":"account"}}]},"last_error":"provider backoff active","last_error_at":%d,"negative_until":%d}`
	legacy := fmt.Sprintf(`{"agy":`+entry+`,"codex":`+entry+`}`,
		testNow.Add(-5*time.Hour).Unix(), "agy", testNow.Add(-time.Minute).Unix(), testNow.Add(time.Hour).Unix(),
		testNow.Add(-5*time.Hour).Unix(), "codex", testNow.Add(-time.Minute).Unix(), testNow.Add(time.Hour).Unix())
	if e := AtomicWrite(legacyCachePath(), []byte(legacy)); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	c := Collector{Providers: map[string]Provider{"agy": ProviderFunc(func(context.Context) Observation {
		calls.Add(1)
		return observation("agy", 40)
	})}, Now: func() time.Time { return testNow }}
	rows, e := c.Collect(context.Background(), []string{"agy"})
	if e != nil || calls.Load() != 1 || rows[0].Error != "" || rows[0].Stale || *rows[0].Buckets[0].UsedPct != 40 {
		t.Fatalf("legacy backoff blocked the own provider: %+v %v", rows, e)
	}
	// A pool without a native provider still honours the imported backoff.
	rows, e = c.Collect(context.Background(), []string{"codex"})
	if e != nil || rows[0].Error != "provider backoff active" || !rows[0].Stale {
		t.Fatalf("imported backoff lost without a provider: %+v %v", rows, e)
	}
	// When the native fetch fails, the imported measurement serves as stale.
	c.Providers["codex"] = ProviderFunc(func(context.Context) Observation {
		return Observation{ID: "codex", Error: "usage HTTP 429", ErrorKind: "rate_limited"}
	})
	rows, e = c.Collect(context.Background(), []string{"codex"})
	if e != nil || rows[0].Error != "usage HTTP 429" || rows[0].ErrorKind != "rate_limited" || !rows[0].Stale {
		t.Fatalf("failed native fetch did not fall back: %+v %v", rows, e)
	}
	if len(rows[0].Buckets) != 1 || *rows[0].Buckets[0].UsedPct != 10 {
		t.Fatalf("imported measurement lost: %+v", rows[0])
	}
}
func TestOwnBackoffStillBlocks(t *testing.T) {
	isolate(t)
	if e := WriteJSON(CachePath(), map[string]CacheEntry{"agy": {
		NegativeUntil: float64(testNow.Add(time.Hour).Unix()),
		LastErrorAt:   float64(testNow.Add(-time.Minute).Unix()),
		LastError:     &Observation{Error: "usage HTTP 429", ErrorKind: "rate_limited"},
	}}); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	c := Collector{Providers: map[string]Provider{"agy": ProviderFunc(func(context.Context) Observation {
		calls.Add(1)
		return observation("agy", 40)
	})}, Now: func() time.Time { return testNow }}
	rows, e := c.Collect(context.Background(), []string{"agy"})
	if e != nil || calls.Load() != 0 || rows[0].Error != "provider backoff active" || rows[0].ErrorKind != "rate_limited" {
		t.Fatalf("own backoff bypassed: %+v %v", rows, e)
	}
}
func TestLegacyTextFailureMetadata(t *testing.T) {
	isolate(t)
	path := legacyCachePath()
	body := `{"claude":{"fetched_at":1793145600,"result":{"id":"claude","pool_class":"spend","buckets":[{"window":"7d","used_pct":10,"scope":{"kind":"account"}}]},"last_error":"legacy probe failure","last_error_at":1793145610},"retired":{"result":"retired provider shape","last_error":["ignored"]}}`
	if e := AtomicWrite(path, []byte(body)); e != nil {
		t.Fatal(e)
	}
	entries, e := LoadSnapshots()
	if e != nil || len(entries) != 1 || entries["claude"].LastError == nil || *entries["claude"].Result.Buckets[0].UsedPct != 10 {
		t.Fatalf("legacy failure metadata rejected: %v", e)
	}
	native := CachePath()
	if e = WriteJSON(native, entries); e != nil {
		t.Fatal(e)
	}
	entries, e = LoadSnapshots()
	if e != nil || entries["claude"].LastError == nil {
		t.Fatalf("structured failure metadata rejected: %v", e)
	}
}

func TestFailedRefreshRetainsLegacyMeasurementAndIdentity(t *testing.T) {
	for _, same := range []bool{true, false} {
		name := "same account"
		if !same {
			name = "changed account"
		}
		t.Run(name, func(t *testing.T) {
			isolate(t)
			old := observation("claude", 10)
			old.AccountFP = "fp-original"
			old.AccountFPKind = "account"
			if !same {
				old = observation("claude", 99)
				old.AccountFP = "fp-original"
				old.AccountFPKind = "account"
			}
			if e := WriteJSON(legacyCachePath(), map[string]CacheEntry{"claude": {FetchedAt: float64(testNow.Unix()), Result: old}}); e != nil {
				t.Fatal(e)
			}
			currentFP := "fp-original"
			if !same {
				currentFP = "fp-current"
			}
			c := Collector{TTL: time.Minute, Now: func() time.Time { return testNow.Add(5 * time.Minute) }, Providers: map[string]Provider{"claude": ProviderFunc(func(context.Context) Observation {
				return Observation{ID: "claude", Error: "usage HTTP 429", ErrorKind: "rate_limited", AccountFP: currentFP, AccountFPKind: "account"}
			})}}
			for attempt := 0; attempt < 2; attempt++ {
				rows, e := c.Collect(context.Background(), []string{"claude"})
				if e != nil {
					t.Fatal(e)
				}
				d := Gate(Inputs{Now: testNow.Add(5 * time.Minute), Observations: rows}, Request{Profile: "sonnet"})
				if same {
					if d.ExitCode != 0 || !d.StaleAccepted {
						t.Fatalf("last good measurement lost: %+v", d)
					}
				} else if d.ExitCode != 4 {
					t.Fatalf("old account quota used after identity change: %+v", d)
				}
			}
			entries, e := LoadSnapshots()
			if e != nil || entries["claude"].FetchedAt != float64(testNow.Unix()) || len(entries["claude"].Result.Buckets) != 2 {
				t.Fatal("failed probe replaced last successful snapshot")
			}
		})
	}
}
