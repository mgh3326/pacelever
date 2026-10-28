package pacelever

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolate(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	for _, k := range []string{"PACELEVER_CONFIG", "PACELEVER_CACHE", "SCOPEFUEL_CACHE", "SCOPEFUEL_BENCH_DB"} {
		t.Setenv(k, "")
	}
}
func TestConfigLegacy(t *testing.T) {
	isolate(t)
	legacy := filepath.Join(configBase(), "scopefuel", "config.toml")
	b := []byte("[pools.agy]\nsubscribed = false\n[pools.claude]\nclass = 'preserve'\nuntil = 2027-01-01\nboost = 3\n[pools.codex]\nsubscribed = false\n[profiles.sonnet]\nsubscribed = true\n[bench]\nallow_plaintext_catalog = true\n[custom]\nretain = 'keep-me'\n")
	if e := AtomicWrite(legacy, b); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig()
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "agy") {
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
	after, _ := os.ReadFile(legacy)
	if string(after) != string(b) {
		t.Fatal("legacy config changed")
	}
	c, e = LoadConfig()
	if e != nil || c.Raw["custom"].(map[string]any)["retain"] != "keep-me" || c.PaceThreshold() != 1.5 {
		t.Fatalf("config keys lost: %v", e)
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

func TestLegacyManualAuditPreserved(t *testing.T) {
	isolate(t)
	path := filepath.Join(filepath.Dir(legacyCachePath()), "manual.json")
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
