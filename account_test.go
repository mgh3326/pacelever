package pacelever

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexHomePrecedence(t *testing.T) {
	t.Setenv("CODEX_HOME", "/env/codex")
	if got := codexHome("/home/u", map[string]string{"CODEX_HOME": "/sel/codex"}); got != "/sel/codex" {
		t.Fatalf("selector not preferred: %s", got)
	}
	if got := codexHome("/home/u", nil); got != "/env/codex" {
		t.Fatalf("environment not used: %s", got)
	}
	t.Setenv("CODEX_HOME", "")
	if got := codexHome("/home/u", nil); got != filepath.Join("/home/u", ".codex") {
		t.Fatalf("default not used: %s", got)
	}
}

func TestLocalCodexAccountMatchesPublishedKey(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"tokens":{"access_token":"fixture","account_id":"acct-fixture"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	local := localAccountFP("codex", map[string]string{"CODEX_HOME": dir})
	var o Observation
	o.publishAccount(accountFP("codex", "acct-fixture"))
	if local == "" || local != o.PublishFP || o.PublishFPKind != FPKindAccount {
		t.Fatalf("local key %q and published key %q/%q differ", local, o.PublishFP, o.PublishFPKind)
	}
	if local != fingerprint("codex-account", "acct-fixture") {
		t.Fatal("account fingerprint changed its stored form")
	}
}

func TestPublishAccountIgnoresEmpty(t *testing.T) {
	var o Observation
	o.publishAccount(accountFP("codex", ""))
	if o.PublishFP != "" || o.PublishFPKind != "" {
		t.Fatalf("empty account attributed: %+v", o)
	}
	if accountKeyFP("codex:abc") != "abc" || accountKeyFP("codex") != "" {
		t.Fatal("account key split")
	}
}
