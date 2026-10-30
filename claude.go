package pacelever

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/text/unicode/norm"
)

// parseClaudeAPI reads the OAuth usage endpoint: the 5h and 7d account
// windows plus any per-model weekly limits.
func parseClaudeAPI(o *Observation, raw map[string]json.RawMessage) {
	for _, axis := range []struct{ key, window string }{{"five_hour", "5h"}, {"seven_day", "7d"}} {
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if json.Unmarshal(raw[axis.key], &w) == nil {
			o.Buckets = append(o.Buckets, accountBucket(axis.window, validated(w.Utilization), w.ResetsAt))
		}
	}
	var limits []struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    string   `json:"resets_at"`
		Scope       struct {
			Model struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	}
	_ = json.Unmarshal(raw["limits"], &limits)
	for _, w := range limits {
		if w.Scope.Model.DisplayName != "" {
			o.Buckets = append(o.Buckets, modelBucket(w.Scope.Model.DisplayName, "7d", validated(w.Utilization), w.ResetsAt))
		}
	}
}

func claudeLocations(home string, selectors map[string]string) (credential, identity, service string, shareable bool) {
	ccd := selectors["CLAUDE_CONFIG_DIR"]
	if ccd == "" {
		ccd = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	secure, hasSecure := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	dir := ccd
	if hasSecure {
		dir = secure
	}
	if dir == "" {
		dir = filepath.Join(home, ".claude")
	}
	credential = filepath.Join(dir, ".credentials.json")
	identity = filepath.Join(home, ".claude.json")
	if ccd != "" {
		identity = filepath.Join(ccd, ".claude.json")
	}
	contextName := ccd
	if hasSecure {
		contextName = secure
	}
	service = "Claude Code-credentials"
	if contextName != "" {
		sum := sha256.Sum256([]byte(norm.NFC.String(contextName)))
		service += "-" + hex.EncodeToString(sum[:4])
	}
	shareable = !hasSecure || secure == ccd
	return
}

func chooseOAuth(file, keychain map[string]any, now time.Time) map[string]any {
	if str(file["accessToken"]) == "" {
		return keychain
	}
	if str(keychain["accessToken"]) == "" {
		return file
	}
	f, fok := finiteNumber(file["expiresAt"])
	k, kok := finiteNumber(keychain["expiresAt"])
	expiredF := fok && f/1000 <= float64(now.Unix())
	expiredK := kok && k/1000 <= float64(now.Unix())
	if expiredF != expiredK {
		if expiredF {
			return keychain
		}
		return file
	}
	if fok && kok && k > f {
		return keychain
	}
	return file
}

func claudeAccountFingerprint(provider Provider, identityPath string, shareable bool) Provider {
	return ProviderFunc(func(ctx context.Context) Observation {
		o := provider.Fetch(ctx)
		if fp := claudeAccountFP(identityPath); fp != "" && shareable {
			o.AccountFP, o.AccountFPKind = fp, FPKindAccount
		}
		return o
	})
}
