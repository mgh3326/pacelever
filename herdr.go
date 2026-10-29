package pacelever

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

type HerdrEvent struct {
	PaneID     string
	Provider   string
	Selectors  map[string]string
	Credential string
}

func ParseHerdrEvent(raw string) (HerdrEvent, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &envelope) != nil {
		return HerdrEvent{}, fmt.Errorf("invalid herdr event JSON")
	}
	if data, ok := envelope["data"]; ok {
		if json.Unmarshal(data, &envelope) != nil {
			return HerdrEvent{}, fmt.Errorf("invalid herdr event data")
		}
	}
	e := HerdrEvent{Selectors: map[string]string{}, Credential: "default"}
	_ = json.Unmarshal(envelope["pane_id"], &e.PaneID)
	for _, key := range []string{"agent", "display_agent", "provider"} {
		var name string
		_ = json.Unmarshal(envelope[key], &name)
		name = strings.ReplaceAll(normalize(name), " ", "-")
		if name == "claude-code" {
			name = "claude"
		}
		for _, known := range ProviderIDs {
			if name == known {
				e.Provider = name
				break
			}
		}
		if e.Provider != "" {
			break
		}
	}
	var env map[string]string
	payload := envelope["env"]
	if payload == nil {
		payload = envelope["environment"]
	}
	_ = json.Unmarshal(payload, &env)
	identity := ""
	for _, key := range []string{"HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "KIMI_CODE_HOME"} {
		if value := env[key]; value != "" {
			identity += key + "=" + value + "|"
			if key != "HOME" {
				e.Selectors[key] = value
			}
		}
	}
	if home := env["HOME"]; home != "" {
		key, subdir := "", ""
		switch e.Provider {
		case "claude":
			key, subdir = "CLAUDE_CONFIG_DIR", ".claude"
		case "codex":
			key, subdir = "CODEX_HOME", ".codex"
		case "grok":
			key, subdir = "GROK_HOME", ".grok"
		case "kimi":
			key, subdir = "KIMI_CODE_HOME", ".kimi"
		}
		if key != "" && e.Selectors[key] == "" {
			e.Selectors[key] = filepath.Join(home, subdir)
		}
	}
	if identity != "" {
		sum := sha256.Sum256([]byte(identity))
		e.Credential = hex.EncodeToString(sum[:5])
	}
	return e, nil
}
func HerdrLabel(o Observation, credential string) string {
	if o.Error != "" {
		return o.ID + " unavailable · credential=" + credential
	}
	label := o.ID
	var now, week *float64
	for _, b := range o.Buckets {
		if b.Scope.Kind != ScopeAccount || !validPct(b.UsedPct) {
			continue
		}
		if b.Horizon == "now" && (now == nil || *b.UsedPct > *now) {
			now = b.UsedPct
		}
		if (b.Horizon == "week" || b.Horizon == "month") && (week == nil || *b.UsedPct > *week) {
			week = b.UsedPct
		}
	}
	if now != nil {
		label += fmt.Sprintf(" · now %g%%", *now)
	}
	if week != nil {
		label += fmt.Sprintf(" · wk %g%%", *week)
	}
	label += " · credential=" + credential
	if len([]rune(label)) > 80 {
		label = string([]rune(label)[:80])
	}
	return label
}
