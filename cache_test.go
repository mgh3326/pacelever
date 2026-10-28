package pacelever

import (
	"context"
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
func TestCommandcodeDeferredBudget(t *testing.T) {
	isolate(t)
	var deferred, probed atomic.Int32
	c := Collector{Providers: map[string]Provider{"commandcode": ProviderFunc(func(context.Context) Observation { probed.Add(1); return observation("commandcode", 1) })}, DeferredRefresh: func(pool string) error { deferred.Add(1); return nil }, Now: func() time.Time { return testNow }}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	rows, e := c.Collect(ctx, []string{"commandcode"})
	if e != nil || deferred.Load() != 1 || probed.Load() != 0 || rows[0].ErrorKind != "probe_in_progress" {
		t.Fatalf("short budget started probe %+v %v", rows, e)
	}
	c.Worker = true
	if _, e = c.Refresh(context.Background(), "commandcode"); e != nil || probed.Load() != 1 {
		t.Fatal("independent worker did not probe")
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

func TestLegacyTextFailureMetadata(t *testing.T) {
	isolate(t)
	path := legacyCachePath()
	body := `{"claude":{"fetched_at":1793145600,"result":{"id":"claude","pool_class":"spend","buckets":[{"window":"7d","used_pct":10,"scope":{"kind":"account"}}]},"last_error":"legacy probe failure","last_error_at":1793145610},"agy":{"result":"retired provider shape","last_error":["ignored"]}}`
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
