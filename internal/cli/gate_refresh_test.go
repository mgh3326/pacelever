package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pl "github.com/mgh3326/pacelever"
)

// memoryHK is an in-memory shared quota store: no network.
type memoryHK struct {
	mu        sync.Mutex
	latest    pl.SharedLatest
	holder    string
	failWrite bool
	reads     int
	leases    int
	published int
}

func (m *memoryHK) Latest(context.Context, string) (pl.SharedLatest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	return m.latest, nil
}
func (m *memoryHK) Lease(_ context.Context, _, holder string, _ int) (pl.SharedLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leases++
	if m.holder == "" || m.holder == holder {
		m.holder = holder
	}
	return pl.SharedLease{Holder: m.holder, Acquired: m.holder == holder}, nil
}
func (m *memoryHK) Publish(_ context.Context, _, holder string, x pl.SharedObservation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWrite || m.holder != holder {
		return errors.New("refused")
	}
	m.published++
	x.ID = int64(m.published)
	m.latest.Observation = &x
	return nil
}
func (m *memoryHK) Release(_ context.Context, _, holder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holder == holder {
		m.holder = ""
	}
	return nil
}

const (
	refreshOrg = "org-fixture"
	otherOrg   = "org-other-fixture"
)

// devinHost logs this host in to org and returns its derived account key.
func devinHost(t *testing.T, org string) string {
	t.Helper()
	isolated(t)
	t.Setenv("PACELEVER_HOST_ID", "fixture-host")
	data, config := filepath.Join(os.Getenv("XDG_DATA_HOME"), "devin", "credentials.toml"), filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "devin", "config.json")
	if err := pl.AtomicWrite(data, []byte("windsurf_api_key = 'fixture-token'\n")); err != nil {
		t.Fatal(err)
	}
	if err := pl.AtomicWrite(config, []byte(`{"devin":{"org_id":"`+org+`"}}`)); err != nil {
		t.Fatal(err)
	}
	return devinKey(org)
}

// devinKey is the documented account key: pool plus the first 8 bytes of
// SHA-256 over "devin-account|<org>".
func devinKey(org string) string {
	sum := sha256.Sum256([]byte("devin-account|" + org))
	return "devin:" + hex.EncodeToString(sum[:8])
}

func gateAccount(t *testing.T, hk *memoryHK, measurable []string, profile, key string) (int, error, map[string]any) {
	t.Helper()
	cfg := pl.Config{Quota: pl.QuotaConfig{MeasurableAccounts: measurable, WaitS: .001}}
	var out, errOut bytes.Buffer
	app := App{Config: cfg, Out: &out, Err: &errOut, Collector: pl.Collector{Config: cfg, Shared: hk, CachePath: filepath.Join(t.TempDir(), "cache.json")}}
	v, err := parse([]string{"gate", "-m", profile, "--account-key", key, "--json"})
	if err != nil {
		t.Fatal(err)
	}
	code, err := app.gate(context.Background(), v)
	record := map[string]any{}
	_ = json.Unmarshal(out.Bytes(), &record)
	return code, err, record
}

func TestGateAccountFreeDevinPublishedAndAdmitted(t *testing.T) {
	key := devinHost(t, refreshOrg)
	hk := &memoryHK{}
	code, err, record := gateAccount(t, hk, []string{key}, "devin-swe2-medium", key)
	if err != nil || code != pl.Admitted || record["ok"] != true || record["gate_basis"] != "free-bucket:swe-2-medium" {
		t.Fatalf("code=%d err=%v record=%v", code, err, record)
	}
	wire, _ := json.Marshal(hk.latest.Observation)
	if hk.published != 1 || !strings.Contains(string(wire), `"free_bucket":{"state":"available"}`) || !strings.Contains(string(wire), `"buckets":[]`) || strings.Contains(string(wire), "used_pct") {
		t.Fatalf("published=%d wire=%s", hk.published, wire)
	}
	// The next gate reads the fresh free state and does not measure again.
	code, err, _ = gateAccount(t, hk, nil, "devin-swe2-medium", key)
	if err != nil || code != pl.Admitted || hk.published != 1 {
		t.Fatalf("second gate: code=%d err=%v published=%d", code, err, hk.published)
	}
}

func TestGateAccountStaleSameAccountRefreshesThenAdmits(t *testing.T) {
	key := devinHost(t, refreshOrg)
	stale := &pl.SharedObservation{ID: 1, Kind: "auto", MeasuredAt: time.Now().Add(-time.Hour), Buckets: []pl.SharedBucket{}, FreeBucket: &pl.FreeBucketState{State: pl.FreeBucketAvailable}}
	hk := &memoryHK{latest: pl.SharedLatest{Observation: stale}}
	code, err, record := gateAccount(t, hk, []string{key}, "devin-swe2-medium", key)
	if err != nil || code != pl.Admitted || record["account_key"] != key {
		t.Fatalf("code=%d err=%v record=%v", code, err, record)
	}
	if hk.published != 1 || !hk.latest.Observation.MeasuredAt.After(stale.MeasuredAt) {
		t.Fatalf("stale reading was not replaced: published=%d", hk.published)
	}
}

func TestGateAccountStaleOtherAccountStillRefused(t *testing.T) {
	local := devinHost(t, refreshOrg)
	other := devinKey(otherOrg)
	stale := &pl.SharedObservation{ID: 1, Kind: "auto", MeasuredAt: time.Now().Add(-time.Hour), Buckets: []pl.SharedBucket{}, FreeBucket: &pl.FreeBucketState{State: pl.FreeBucketAvailable}}
	hk := &memoryHK{latest: pl.SharedLatest{Observation: stale}}
	code, err, record := gateAccount(t, hk, []string{local, other}, "devin-swe2-medium", other)
	if err == nil || code != pl.Unmeasurable || record["ok"] == true || !strings.Contains(err.Error(), "does not hold the account") {
		t.Fatalf("code=%d err=%v record=%v", code, err, record)
	}
	if hk.leases != 0 || hk.published != 0 {
		t.Fatalf("a different account was measured: leases=%d published=%d", hk.leases, hk.published)
	}
}

func TestGateAccountStaleRefusedWhenHostCannotMeasure(t *testing.T) {
	key := devinHost(t, refreshOrg)
	for _, tc := range []struct {
		name       string
		measurable []string
		failWrite  bool
		want       string
	}{
		{"not measurable here", nil, false, "may not measure"},
		{"publish refused", []string{key}, true, "local refresh failed (shared_publish_failed)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hk := &memoryHK{failWrite: tc.failWrite}
			code, err, record := gateAccount(t, hk, tc.measurable, "devin-swe2-medium", key)
			if err == nil || code != pl.Unmeasurable || record["ok"] == true || !strings.Contains(err.Error(), tc.want) || hk.published != 0 {
				t.Fatalf("code=%d err=%v record=%v published=%d", code, err, record, hk.published)
			}
		})
	}
}

func TestGateAccountFreeStateGatesTheFreeRung(t *testing.T) {
	key := devinHost(t, refreshOrg)
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name string
		free *pl.FreeBucketState
		want int
	}{
		{"available", &pl.FreeBucketState{State: pl.FreeBucketAvailable}, pl.Admitted},
		{"exhausted", &pl.FreeBucketState{State: pl.FreeBucketExhausted, ResetsAt: &reset}, pl.Blocked},
		{"unknown", &pl.FreeBucketState{State: pl.FreeBucketUnknown}, pl.Unmeasurable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := &pl.SharedObservation{ID: 1, Kind: "auto", MeasuredAt: time.Now(), Buckets: []pl.SharedBucket{}, FreeBucket: tc.free}
			hk := &memoryHK{latest: pl.SharedLatest{Observation: fresh}}
			code, _, record := gateAccount(t, hk, nil, "devin-swe2-medium", key)
			if code != tc.want || (tc.want != pl.Admitted && record["ok"] == true) {
				t.Fatalf("code=%d want=%d record=%v", code, tc.want, record)
			}
		})
	}
}
