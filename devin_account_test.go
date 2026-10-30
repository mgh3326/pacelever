package pacelever

import (
	"os"
	"path/filepath"
	"testing"
)

func writeDevinLogin(t *testing.T) string {
	t.Helper()
	if err := AtomicWrite(filepath.Join(dataBase(), "devin", "credentials.toml"), []byte("windsurf_api_key = 'fixture-token'\n")); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(filepath.Join(configBase(), "devin", "config.json"), []byte(`{"devin":{"org_id":"org-fixture"}}`)); err != nil {
		t.Fatal(err)
	}
	return fingerprint("devin-account", "org-fixture")
}

func TestDevinAccountFingerprint(t *testing.T) {
	for _, tc := range []struct {
		name, credentials, config, want string
	}{
		{"login", "windsurf_api_key = 'fixture-token'", `{"devin":{"org_id":"org-fixture"}}`, fingerprint("devin-account", "org-fixture")},
		{"rotated token", "windsurf_api_key = 'rotated-fixture-token'", `{"devin":{"org_id":"org-fixture"}}`, fingerprint("devin-account", "org-fixture")},
		{"other organization", "windsurf_api_key = 'fixture-token'", `{"devin":{"org_id":"org-other-fixture"}}`, fingerprint("devin-account", "org-other-fixture")},
		{"token only", "windsurf_api_key = 'fixture-token'", `{}`, ""},
		{"no login", "", `{"devin":{"org_id":"org-fixture"}}`, ""},
		{"empty token", "windsurf_api_key = '   '", `{"devin":{"org_id":"org-fixture"}}`, ""},
		{"malformed credentials", "[", `{"devin":{"org_id":"org-fixture"}}`, ""},
		{"malformed identity", "windsurf_api_key = 'fixture-token'", `{`, ""},
		{"non-string identity", "windsurf_api_key = 'fixture-token'", `{"devin":{"org_id":12}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			if err := AtomicWrite(filepath.Join(dataBase(), "devin", "credentials.toml"), []byte(tc.credentials)); err != nil {
				t.Fatal(err)
			}
			if err := AtomicWrite(filepath.Join(configBase(), "devin", "config.json"), []byte(tc.config)); err != nil {
				t.Fatal(err)
			}
			if got := localAccountFP("devin", nil); got != tc.want {
				t.Fatalf("fingerprint = %q, want %q", got, tc.want)
			}
		})
	}
	// A selected organization left behind after logout is not a login.
	isolate(t)
	writeDevinLogin(t)
	if err := os.Remove(filepath.Join(dataBase(), "devin", "credentials.toml")); err != nil {
		t.Fatal(err)
	}
	if got := localAccountFP("devin", nil); got != "" {
		t.Fatal("logged-out identity resolved")
	}
}

func TestDevinAccountKeyRequiresMatchingLogin(t *testing.T) {
	isolate(t)
	fp := writeDevinLogin(t)
	c := Collector{Config: Config{Quota: QuotaConfig{MeasurableAccounts: []string{"devin:" + fp}}}}
	if key, allowed := c.accountKey("devin"); key != "devin:"+fp || !allowed {
		t.Fatalf("derived key %q, allowed=%v", key, allowed)
	}
	// The derived key is authoritative: a configured fingerprint can neither
	// rename nor veto it, so a mismatch never blocks measurement.
	c.Config.Quota.Accounts = map[string]string{"devin": "0123456789abcdef"}
	c.Config.Quota.MeasurableAccounts = []string{"devin:0123456789abcdef"}
	if key, allowed := c.accountKey("devin"); key != "devin:"+fp || !allowed {
		t.Fatalf("configured fingerprint vetoed derived login: %q, %v", key, allowed)
	}
	if err := os.Remove(filepath.Join(dataBase(), "devin", "credentials.toml")); err != nil {
		t.Fatal(err)
	}
	// Without the login the configured fingerprint is no substitute: the key
	// is empty, not merely disallowed.
	if key, allowed := c.accountKey("devin"); key != "" || allowed {
		t.Fatalf("configured key without login: %q, %v", key, allowed)
	}
	c.Config.Quota.Accounts = nil
	if key, _ := c.accountKey("devin"); key != "" {
		t.Fatal("unknown login must keep an empty key")
	}
}
