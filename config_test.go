package pacelever

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolate(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	for _, k := range []string{"PACELEVER_CONFIG", "PACELEVER_CACHE"} {
		t.Setenv(k, "")
	}
}
func TestConfigLoadAndEdit(t *testing.T) {
	isolate(t)
	b := []byte("[pools.retired]\nsubscribed = false\n[pools.claude]\nclass = 'preserve'\nuntil = 2027-01-01\nboost = 3\n[pools.codex]\nsubscribed = false\n[profiles.sonnet]\nsubscribed = true\n[bench]\nallow_plaintext_catalog = true\n[custom]\nretain = 'keep-me'\n")
	if e := AtomicWrite(ConfigPath(), b); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig()
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "retired") {
		t.Fatal(c.Warnings)
	}
	if c.Pool("claude", testNow).Class != "preserve" {
		t.Fatal("date override lost")
	}
	if c.Subscribed("codex-luna", "codex") {
		t.Fatal("subscription lost")
	}
	if e = EditConfig(func(raw map[string]any) error {
		raw["settings"] = map[string]any{"altlane_pace_multiple": 1.5}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	c, e = LoadConfig()
	if e != nil || c.Raw["custom"].(map[string]any)["retain"] != "keep-me" || c.PaceThreshold() != 1.5 {
		t.Fatalf("config keys lost: %v", e)
	}
}
func TestConfigLegacyPathStillLoadsWithWarning(t *testing.T) {
	isolate(t)
	path := filepath.Join(configBase(), "scopefuel", "config.toml")
	if e := AtomicWrite(path, []byte("[pools.claude]\nsubscribed = false\n")); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig()
	if e != nil {
		t.Fatal(e)
	}
	if c.Subscribed("", "claude") {
		t.Fatal("legacy config not loaded")
	}
	found := false
	for _, w := range c.Warnings {
		if strings.Contains(w, "legacy scopefuel config") {
			found = true
		}
	}
	if !found {
		t.Fatalf("migration warning missing: %v", c.Warnings)
	}
	// Policy edits still write only the pacelever file.
	if e = EditConfig(func(raw map[string]any) error {
		raw["settings"] = map[string]any{"altlane_pace_multiple": 1.5}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(path)
	if string(after) != "[pools.claude]\nsubscribed = false\n" {
		t.Fatal("legacy config modified")
	}
}

func TestConfigMalformedDoesNotLeak(t *testing.T) {
	isolate(t)
	sentinel := "DO-NOT-PRINT-CREDENTIAL"
	if e := AtomicWrite(ConfigPath(), []byte("token = '"+sentinel)); e != nil {
		t.Fatal(e)
	}
	_, e := LoadConfig()
	if e == nil || strings.Contains(e.Error(), sentinel) {
		t.Fatalf("unsafe error: %v", e)
	}
}

func TestCreditMaxAgeConfig(t *testing.T) {
	isolate(t)
	if e := AtomicWrite(ConfigPath(), []byte("[pools.anthropic-api]\ncredit_max_age_s = 7200\n")); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig()
	if e != nil || c.CreditMaxAge("anthropic-api").Hours() != 2 {
		t.Fatalf("configured max age %v %v", c.CreditMaxAge("anthropic-api"), e)
	}
	if c.CreditMaxAge("claude").Hours() != 72 {
		t.Fatalf("default max age %v", c.CreditMaxAge("claude"))
	}
	for _, value := range []string{"-1", "inf", "nan", "1e12"} {
		if e := AtomicWrite(ConfigPath(), []byte("[pools.anthropic-api]\ncredit_max_age_s = "+value+"\n")); e != nil {
			t.Fatal(e)
		}
		if _, e = LoadConfig(); e == nil {
			t.Fatalf("credit_max_age_s = %s accepted", value)
		}
	}
}

func TestManualAuditFieldsPreserved(t *testing.T) {
	isolate(t)
	path := filepath.Join(CacheDir(), "manual.json")
	data := `{"schema":"scopefuel.manual.v1","latest":{"claude:5h":"old"},"history":[{"event":"set","manual_observation_id":"old","pool":"claude","window":"5h","used_pct":10,"measured_at":"2026-10-28T00:00:00Z","expires_at":"2026-10-28T01:00:00Z","account_ref":{"verification":"unverified","local_only":true},"execution_context":{"kind":"fixture"}}]}`
	if e := AtomicWrite(path, []byte(data)); e != nil {
		t.Fatal(e)
	}
	s, e := LoadManual()
	if e != nil {
		t.Fatal(e)
	}
	if e = WriteJSON(filepath.Join(CacheDir(), "manual.json"), s); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(filepath.Join(CacheDir(), "manual.json"))
	if !strings.Contains(string(after), "execution_context") || !strings.Contains(string(after), "account_ref") {
		t.Fatal("legacy audit fields lost")
	}
}

func TestExplorationConfigData(t *testing.T) {
	isolate(t)
	data := `[pools.claude.exploration]
fraction = 0.05
experiment_id = "cohort-fixture"
model_order = ["opus", "sonnet", "haiku"]
`
	if e := AtomicWrite(ConfigPath(), []byte(data)); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig()
	if e != nil || !c.Pools["claude"].Exploration.Active() || c.Pools["claude"].Exploration.Fraction != 0.05 {
		t.Fatalf("policy data %v", e)
	}
	if e = AtomicWrite(ConfigPath(), []byte("[pools.claude.exploration]\nfraction = 1.5\nexperiment_id = 'cohort-fixture'\n")); e != nil {
		t.Fatal(e)
	}
	if _, e = LoadConfig(); e == nil {
		t.Fatal("invalid exploration fraction accepted")
	}
}

func TestQuotaPoolFreshTTLConfig(t *testing.T) {
	isolate(t)
	if got := (QuotaConfig{}).freshTTL("claude"); got != 15*time.Minute {
		t.Fatalf("default claude fresh TTL %v", got)
	}
	if got := (QuotaConfig{}).freshTTL("grok"); got != 5*time.Minute {
		t.Fatalf("default grok fresh TTL %v", got)
	}
	if e := AtomicWrite(ConfigPath(), []byte("[quota]\nfresh_ttl_s = 240\n[quota.pool_fresh_ttl_s]\nclaude = 1200\ngrok = 60\n")); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig()
	if e != nil {
		t.Fatal(e)
	}
	if got := c.Quota.freshTTL("claude"); got != 20*time.Minute {
		t.Fatalf("pool override %v", got)
	}
	if got := c.Quota.freshTTL("grok"); got != time.Minute {
		t.Fatalf("pool override %v", got)
	}
	if got := c.Quota.freshTTL("codex"); got != 4*time.Minute {
		t.Fatalf("global fallback %v", got)
	}
	for _, data := range []string{
		"[quota.pool_fresh_ttl_s]\nunknown = 60\n",
		"[quota.pool_fresh_ttl_s]\nclaude = -1\n",
		"[quota.pool_fresh_ttl_s]\nclaude = 0\n",
		"[quota.pool_fresh_ttl_s]\nclaude = nan\n",
	} {
		if e := AtomicWrite(ConfigPath(), []byte(data)); e != nil {
			t.Fatal(e)
		}
		if _, e := LoadConfig(); e == nil {
			t.Fatalf("invalid pool fresh TTL accepted: %s", data)
		}
	}
}
