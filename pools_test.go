package pacelever

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"
	"time"
)

// The table must reproduce the per-pool switches it replaced.
func TestPoolTableMatchesPreviousRules(t *testing.T) {
	windows := map[string][]string{
		"claude": {"5h", "7d"}, "codex": {"5h", "7d"}, "kimi": {"5h", "7d"}, "devin": {"7d"}, "grok": {"7d"},
		"agy": {"5h", "7d"}, "kiro": {"30d"}, "clinepass": {"5h", "7d", "30d"}, "commandcode": {"5h", "7d"},
	}
	ttl := map[string]time.Duration{"claude": 900 * time.Second, "agy": 300 * time.Second, "devin": 180 * time.Second, "commandcode": 180 * time.Second}
	for _, pool := range ProviderIDs {
		if got := RequiredWindows(pool, "", false); !slices.Equal(got, windows[pool]) {
			t.Errorf("%s windows = %v, want %v", pool, got, windows[pool])
		}
		want := ttl[pool]
		if want == 0 {
			want = 60 * time.Second
		}
		if got := traitsOf(pool).cacheTTL(); got != want {
			t.Errorf("%s cache ttl = %v, want %v", pool, got, want)
		}
		local := pool == "claude" || pool == "codex" || pool == "devin"
		if traitsOf(pool).LocalIdentity != local {
			t.Errorf("%s local identity = %t", pool, !local)
		}
		required := pool == "claude" || pool == "codex"
		if traitsOf(pool).RequireAccount != required {
			t.Errorf("%s require account = %t", pool, !required)
		}
	}
	if RequiredWindows("unknown", "", false) != nil || traitsOf("unknown").cacheTTL() != defaultCacheTTL {
		t.Error("unknown pool gained traits")
	}
	if got := RequiredWindows("codex", "", false); &got[0] == &traitsOf("codex").Windows[0] {
		t.Error("RequiredWindows exposes the table slice")
	}
}

// The herdr credential hashes the home selectors in this order; changing it
// would move every scoped cache file.
func TestHomeSelectorOrderKeepsHerdrCredential(t *testing.T) {
	if got := homeSelectors(); !slices.Equal(got, []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "KIMI_CODE_HOME"}) {
		t.Fatalf("home selector order %v", got)
	}
	e, err := ParseHerdrEvent(`{"data":{"pane_id":"p","display_agent":"Codex","env":{"HOME":"/h","CODEX_HOME":"/c","KIMI_CODE_HOME":"/k","GROK_HOME":"/g"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("HOME=/h|CODEX_HOME=/c|GROK_HOME=/g|KIMI_CODE_HOME=/k|"))
	if e.Credential != hex.EncodeToString(sum[:5]) {
		t.Fatalf("credential changed: %s", e.Credential)
	}
}

func TestCLIHomePrecedence(t *testing.T) {
	t.Setenv("GROK_HOME", " ")
	if got := cliHome("grok", "/h", nil); got != "/h/.grok" {
		t.Fatalf("blank GROK_HOME used: %q", got)
	}
	t.Setenv("GROK_HOME", "/env")
	if got := cliHome("grok", "/h", map[string]string{"GROK_HOME": "/sel"}); got != "/sel" {
		t.Fatalf("selector not preferred: %q", got)
	}
	if got := cliHome("devin", "/h", nil); got != "" {
		t.Fatalf("pool without a home variable: %q", got)
	}
}
