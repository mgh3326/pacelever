package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	pl "github.com/mgh3326/pacelever"
)

type gateLeaseStore struct {
	mu             sync.Mutex
	now            time.Time
	elapsed        time.Duration
	held           string
	requestedStale bool
	cancel         context.CancelFunc
	leases         []string
	leaseSeen      chan struct{}
}

func (s *gateLeaseStore) clock() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now.Add(s.elapsed)
}
func (s *gateLeaseStore) Latest(_ context.Context, key string) (pl.SharedLatest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasPrefix(key, "grok:") && !s.requestedStale {
		return pl.SharedLatest{Observation: &pl.SharedObservation{Kind: pl.SharedAuto, MeasuredAt: s.now, Buckets: []pl.SharedBucket{{Window: "7d", UsedPct: 60, Scope: pl.ScopeAccount}}}}, nil
	}
	return pl.SharedLatest{}, nil
}
func (s *gateLeaseStore) Lease(_ context.Context, key, holder string, _ int) (pl.SharedLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases = append(s.leases, key)
	if strings.HasPrefix(key, s.held+":") {
		if s.leaseSeen != nil {
			close(s.leaseSeen)
		} else {
			s.elapsed += 60 * time.Second
			s.cancel()
		}
		return pl.SharedLease{Holder: "other", ExpiresAt: s.now.Add(60 * time.Second)}, nil
	}
	s.cancel()
	return pl.SharedLease{Holder: holder, Acquired: true}, nil
}
func (*gateLeaseStore) Publish(context.Context, string, string, pl.SharedObservation) error {
	return nil
}
func (*gateLeaseStore) Release(context.Context, string, string) error { return nil }

func TestGateAlternativeLeaseDoesNotDelayVerdict(t *testing.T) {
	devin := devinHost(t, refreshOrg)
	const grok = "grok:0123456789abcdef"
	cfg := pl.Config{Quota: pl.QuotaConfig{Accounts: map[string]string{"grok": "0123456789abcdef"}, MeasurableAccounts: []string{grok, devin}, WaitS: 60}}
	var baseline map[string]any
	for _, held := range []string{"devin", ""} {
		ctx, cancel := context.WithCancel(context.Background())
		store := &gateLeaseStore{now: pl.ClockNow(nil), held: held, cancel: cancel}
		var out, errOut bytes.Buffer
		app := App{Config: cfg, Out: &out, Err: &errOut, Now: store.clock, Collector: pl.Collector{Config: cfg, Shared: store, CachePath: filepath.Join(t.TempDir(), "cache.json"), Now: store.clock}}
		v, err := parse([]string{"gate", "-m", "grok", "--effort", "low", "--role", "worker", "--json"})
		if err != nil {
			t.Fatal(err)
		}
		start := store.clock()
		code, err := app.gate(ctx, v)
		elapsed := store.clock().Sub(start)
		cancel()
		if elapsed > 2*time.Second {
			t.Fatalf("alternative lease delayed gate by %v", elapsed)
		}
		if err != nil || code != pl.Admitted {
			t.Fatalf("held=%q code=%d err=%v out=%s stderr=%s", held, code, err, out.String(), errOut.String())
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		delete(got, "generated_at")
		if baseline == nil {
			baseline = got
		} else if !reflect.DeepEqual(baseline, got) {
			t.Fatalf("verdict changed: %v vs %v", baseline, got)
		}
		if len(store.leases) != 0 {
			t.Fatalf("alternative was not omitted: %v", store.leases)
		}
	}
}

func TestGateRequestedLeaseWaitsForContext(t *testing.T) {
	isolated(t)
	const grok = "grok:0123456789abcdef"
	cfg := pl.Config{Quota: pl.QuotaConfig{Accounts: map[string]string{"grok": "0123456789abcdef"}, MeasurableAccounts: []string{grok}, WaitS: 60}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &gateLeaseStore{now: pl.ClockNow(nil), held: "grok", requestedStale: true, cancel: cancel, leaseSeen: make(chan struct{})}
	var out, errOut bytes.Buffer
	app := App{Config: cfg, Out: &out, Err: &errOut, Now: store.clock, Collector: pl.Collector{Config: cfg, Shared: store, CachePath: filepath.Join(t.TempDir(), "cache.json"), Now: store.clock}}
	v, err := parse([]string{"gate", "-m", "grok", "--effort", "low", "--role", "worker", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { code, _ := app.gate(ctx, v); done <- code }()
	<-store.leaseSeen
	select {
	case code := <-done:
		t.Fatalf("gate returned %d before cancellation", code)
	default:
	}
	cancel()
	if code := <-done; code != pl.Unmeasurable {
		t.Fatalf("gate returned %d after cancellation", code)
	}
}
