package pacelever

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type exhaustEpisode struct {
	EventID     string `json:"event_id"`
	AttemptedAt string `json:"attempted_at"`
	NotifiedAt  string `json:"notified_at,omitempty"`
}

// ExhaustionNotice never changes admission. An explicit operator-switch policy
// enables a deduplicated notice; other modes perform no external action.
func ExhaustionNotice(ctx context.Context, cfg Config, o Observation, scope string, now time.Time, sink func(context.Context, string, string) bool) string {
	switch normalize(firstEnv("PACELEVER_NOTIFY")) {
	case "0", "off", "false":
		return ""
	}
	if o.Error != "" || o.Stale || o.Warning != "" {
		return ""
	}
	policy := cfg.Pool(o.ID, now)
	path := filepath.Join(CacheDir(), "exhaust-notify.json")
	key := o.ID
	if scope != "" {
		key += "@" + scope
	}
	cutoff := policy.Cutoff
	if cfg.Pools[o.ID].Cutoff == 0 && policy.Class == "preserve" {
		cutoff = 90
	}
	var over *Bucket
	for i, b := range o.Buckets {
		if b.Scope.Kind != "account" || !validPct(b.UsedPct) {
			continue
		}
		if reset, e := ParseTime(b.ResetsAt); e == nil && !reset.After(now) {
			continue
		}
		if *b.UsedPct >= cutoff {
			over = &o.Buckets[i]
			break
		}
	}
	if policy.OnExhaust != "operator-switch" && over != nil {
		return ""
	}
	if over == nil {
		var state map[string]exhaustEpisode
		if ReadJSON(path, &state) != nil || state[key].EventID == "" {
			return ""
		}
		_ = WithFileLock(path+".lock", false, func() error { _ = ReadJSON(path, &state); delete(state, key); return WriteJSON(path, state) })
		return ""
	}
	notice := "exhaustion notice unavailable"
	_ = WithFileLock(path+".lock", false, func() error {
		state := map[string]exhaustEpisode{}
		_ = ReadJSON(path, &state)
		episode := state[key]
		if episode.NotifiedAt != "" {
			notice = "operator-switch already notified"
			return nil
		}
		if at, e := ParseTime(episode.AttemptedAt); e == nil && now.Sub(at) < time.Minute {
			notice = "operator-switch notification retry pending"
			return nil
		}
		if episode.EventID == "" {
			v := make([]byte, 8)
			if _, e := rand.Read(v); e != nil {
				return e
			}
			episode.EventID = "pacelever-exhaust-" + o.ID + "-" + hex.EncodeToString(v)
		}
		episode.AttemptedAt = now.UTC().Format(time.RFC3339Nano)
		state[key] = episode
		if e := WriteJSON(path, state); e != nil {
			return e
		}
		if sink == nil {
			sink = EmitExhaustion
		}
		text := fmt.Sprintf("[pacelever] pool=%s account switch required; window=%s used=%g%% cutoff=%g%% reset=%s", o.ID, over.Window, *over.UsedPct, cutoff, over.ResetsAt)
		if sink(ctx, text, episode.EventID) {
			episode.NotifiedAt = episode.AttemptedAt
			notice = "operator-switch notified"
		} else {
			notice = "operator-switch notification failed"
		}
		state[key] = episode
		return WriteJSON(path, state)
	})
	return notice
}
func EmitExhaustion(ctx context.Context, text, eventID string) bool {
	binary := firstEnv("PANEWIRE_BIN")
	if binary == "" {
		binary = "panewire"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "emit", "--kind", "lane.event", "--lane", "operator-desk", "--event-id", eventID, "--text", text)
	var errors boundedBuffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &errors
	cmd.WaitDelay = time.Second
	e := cmd.Run()
	return e == nil || strings.Contains(strings.ToLower(errors.String()), "duplicate event_id")
}
