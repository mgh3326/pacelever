package pacelever

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// parseClinePass reads the plan usage-limits endpoint.
func parseClinePass(o *Observation, raw map[string]json.RawMessage) {
	var data struct {
		Limits []struct {
			Type  string   `json:"type"`
			Used  *float64 `json:"percentUsed"`
			Reset string   `json:"resetsAt"`
		} `json:"limits"`
	}
	_ = json.Unmarshal(raw["data"], &data)
	for _, w := range data.Limits {
		window := map[string]string{"five_hour": "5h", "weekly": "7d", "monthly": "30d"}[w.Type]
		if window != "" {
			o.Buckets = append(o.Buckets, Bucket{Label: w.Type, Window: window, UsedPct: validated(w.Used), ResetsAt: w.Reset, Scope: Scope{Kind: ScopeAccount}, Horizon: horizon(window)})
		}
	}
}

func clineToken(home string) string {
	if token := firstEnv("CLINE_API_KEY", "CLINEPASS_API_KEY"); token != "" {
		return token
	}
	providers := object(readObject(filepath.Join(home, ".cline", "data", "settings", "providers.json"))["providers"])
	for _, name := range []string{"cline-pass", "cline"} {
		settings := object(providers[name])["settings"]
		token := strings.TrimSpace(str(object(object(settings)["auth"])["accessToken"]))
		if token != "" {
			return token
		}
	}
	if token := str(object(readObject(filepath.Join(dataBase(), "opencode", "auth.json"))["cline-pass"])["key"]); token != "" {
		return token
	}
	b, _ := os.ReadFile(filepath.Join(configBase(), "cline", "api-key"))
	return strings.TrimSpace(string(b))
}
