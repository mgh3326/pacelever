package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	pl "github.com/mgh3326/pacelever"
)

func TestGateProcessAlternativeLease(t *testing.T) {
	devin := devinHost(t, refreshOrg)
	now := pl.ClockNow(nil)
	// The separately built production executable does not install TestMain's offset.
	if offset := os.Getenv("PACELEVER_TEST_CLOCK_OFFSET"); offset != "" {
		d, err := pl.Duration(offset)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(-d)
	}
	const grok = "grok:0123456789abcdef"
	var mu sync.Mutex
	held := false
	var unexpected []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v1/quota/presence" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/quota/accounts/"+grok+"/latest":
			_ = json.NewEncoder(w).Encode(pl.SharedLatest{Observation: &pl.SharedObservation{Kind: pl.SharedAuto, MeasuredAt: now, Buckets: []pl.SharedBucket{{Window: "7d", UsedPct: 60, Scope: pl.ScopeAccount}}}})
		case r.Method == "GET" && r.URL.Path == "/v1/quota/accounts/"+devin+"/latest":
			unexpected = append(unexpected, r.URL.Path)
			_ = json.NewEncoder(w).Encode(pl.SharedLatest{})
		case r.Method == "POST" && r.URL.Path == "/v1/quota/accounts/"+devin+"/lease":
			unexpected = append(unexpected, r.URL.Path)
			if held {
				_ = json.NewEncoder(w).Encode(pl.SharedLease{Holder: "other", ExpiresAt: now.Add(time.Minute)})
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_ = json.NewEncoder(w).Encode(pl.SharedLatest{})
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	binary := filepath.Join(dir, "pacelever")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/pacelever")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	config := filepath.Join(dir, "config.toml")
	data := fmt.Sprintf("[bench]\nallow_plaintext_quota_share = true\n[quota]\nurl = %q\ncredential_env = 'QUOTA_TEST_CREDENTIAL'\nwait_s = 10\nmeasurable_accounts = [%q, %q]\n[quota.accounts]\ngrok = '0123456789abcdef'\n", server.URL, grok, devin)
	if err := os.WriteFile(config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var baseline map[string]any
	for _, occupied := range []bool{true, false} {
		mu.Lock()
		held = occupied
		unexpected = nil
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, binary, "gate", "-m", "grok", "--effort", "low", "--role", "worker", "--json")
		cmd.Env = []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "XDG_CONFIG_HOME=" + os.Getenv("XDG_CONFIG_HOME"), "XDG_DATA_HOME=" + os.Getenv("XDG_DATA_HOME"), "XDG_CACHE_HOME=" + filepath.Join(dir, fmt.Sprint(occupied)), "PACELEVER_CONFIG=" + config, "PACELEVER_HOST_ID=fixture-host", "QUOTA_TEST_CREDENTIAL=fixture-token"}
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		timedOut := ctx.Err() != nil
		cancel()
		if timedOut || err != nil {
			t.Fatalf("held=%v timeout=%v err=%v output=%s stderr=%s", occupied, timedOut, err, out.String(), errOut.String())
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("invalid verdict: %v: %s", err, out.String())
		}
		if got["ok"] != true {
			t.Fatalf("gate refused: %s", out.String())
		}
		delete(got, "generated_at")
		if baseline == nil {
			baseline = got
		} else if !reflect.DeepEqual(baseline, got) {
			t.Fatalf("verdict changed: %v vs %v", baseline, got)
		}
		mu.Lock()
		requests := strings.Join(unexpected, ", ")
		mu.Unlock()
		if requests != "" {
			t.Fatalf("alternative was not omitted: %s", requests)
		}
	}
}
