package pacelever

import (
	"os"
	"path/filepath"
	"strings"
)

// Provider account identity. Every source that measures an account names it
// through these helpers, so the same login gives the same key whether it was
// read by the API, the CLI, the shared store or the gate.

// accountFP is the fingerprint of one provider account. An empty account
// gives an empty fingerprint.
func accountFP(pool, account string) string { return fingerprint(pool+"-account", account) }

// accountKeyFP is the fingerprint half of a "pool:fp" account key.
func accountKeyFP(key string) string {
	_, fp, _ := strings.Cut(key, ":")
	return fp
}

// publishAccount attributes o to account fp for sharing; an empty fp leaves o
// unattributed.
func (o *Observation) publishAccount(fp string) {
	if fp != "" {
		o.PublishFP, o.PublishFPKind = fp, FPKindAccount
	}
}

// localAccountFP resolves this host's account before any provider request,
// using exactly the hash the provider attaches to its observation.
func localAccountFP(pool string, selectors map[string]string) string {
	home, _ := os.UserHomeDir()
	switch pool {
	case "codex":
		return accountFP("codex", str(codexTokens(codexHome(home, selectors))["account_id"]))
	case "claude":
		_, identity, _, shareable := claudeLocations(home, selectors)
		if !shareable {
			return ""
		}
		return claudeAccountFP(identity)
	case "devin":
		return devinAccountFP()
	}
	return ""
}

// codexHome is the Codex CLI home: the selector, then CODEX_HOME, then ~/.codex.
func codexHome(home string, selectors map[string]string) string {
	if dir := selectors["CODEX_HOME"]; dir != "" {
		return dir
	}
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".codex")
}

// codexTokens is the token block of the Codex login file.
func codexTokens(dir string) map[string]any {
	return object(readObject(filepath.Join(dir, "auth.json"))["tokens"])
}

// claudeAccountFP is the account recorded in a Claude identity file.
func claudeAccountFP(identityPath string) string {
	return accountFP("claude", str(object(readObject(identityPath)["oauthAccount"])["accountUuid"]))
}
