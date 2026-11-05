package pacelever

import (
	"path/filepath"
	"slices"
	"time"
)

// poolTraits are the per-pool facts that more than one part of pacelever
// relies on. Each fact is written once here, so the gate, the cache, the
// shared store and the herdr bridge cannot disagree about a pool.
type poolTraits struct {
	Pool string
	// Windows are the quota windows a reading must cover before plan or
	// manual-evidence adjustments (RequiredWindows).
	Windows []string
	// CacheTTL keeps a fresh reading; zero means defaultCacheTTL.
	CacheTTL time.Duration
	// LocalIdentity: the account is this host's own login, never a configured
	// fingerprint.
	LocalIdentity bool
	// RequireAccount: a measurement that carries no account fingerprint is
	// never shared.
	RequireAccount bool
	// HomeEnv names the CLI's home directory variable and HomeDir its default
	// under $HOME.
	HomeEnv, HomeDir string
}

const defaultCacheTTL = 60 * time.Second

// poolTable is ordered: homeSelectors and anything hashed from it follow this
// order, so it must not be re-sorted.
var poolTable = []poolTraits{
	{Pool: "claude", Windows: []string{"5h", "7d"}, CacheTTL: 900 * time.Second, LocalIdentity: true, RequireAccount: true, HomeEnv: "CLAUDE_CONFIG_DIR", HomeDir: ".claude"},
	{Pool: "codex", Windows: []string{"5h", "7d"}, LocalIdentity: true, RequireAccount: true, HomeEnv: "CODEX_HOME", HomeDir: ".codex"},
	{Pool: "grok", Windows: []string{"7d"}, HomeEnv: "GROK_HOME", HomeDir: ".grok"},
	{Pool: "kimi", Windows: []string{"5h", "7d"}, HomeEnv: "KIMI_CODE_HOME", HomeDir: ".kimi"},
	{Pool: "devin", Windows: []string{"7d"}, CacheTTL: 180 * time.Second, LocalIdentity: true},
	{Pool: "kiro", Windows: []string{"30d"}},
	{Pool: "agy", Windows: []string{"5h", "7d"}, CacheTTL: 300 * time.Second},
	{Pool: "clinepass", Windows: []string{"5h", "7d", "30d"}},
	// The HTTP windows are the measurable requirement; credits are
	// informational for stale acceptance.
	{Pool: "commandcode", Windows: []string{"5h", "7d"}, CacheTTL: 180 * time.Second},
}

// traitsOf is the pool's entry; an unknown pool has none of the traits.
func traitsOf(pool string) poolTraits {
	if i := slices.IndexFunc(poolTable, func(t poolTraits) bool { return t.Pool == pool }); i >= 0 {
		return poolTable[i]
	}
	return poolTraits{Pool: pool}
}

// cacheTTL is how long a fresh reading of pool is reused.
func (t poolTraits) cacheTTL() time.Duration {
	if t.CacheTTL > 0 {
		return t.CacheTTL
	}
	return defaultCacheTTL
}

// homeSelectors are the CLI home variables, in poolTable order.
func homeSelectors() []string {
	var out []string
	for _, t := range poolTable {
		if t.HomeEnv != "" {
			out = append(out, t.HomeEnv)
		}
	}
	return out
}

// cliHome is a pool's CLI home: its selector, then the environment, then the
// default under home. A pool without a home variable returns "".
func cliHome(pool, home string, selectors map[string]string) string {
	t := traitsOf(pool)
	if t.HomeEnv == "" {
		return ""
	}
	if dir := selectors[t.HomeEnv]; dir != "" {
		return dir
	}
	if dir := FirstEnv(t.HomeEnv); dir != "" {
		return dir
	}
	return filepath.Join(home, t.HomeDir)
}
