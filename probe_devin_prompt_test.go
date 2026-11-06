package pacelever

import (
	"testing"
	"time"
)

// The devin CLI draws its input prompt with U+276D; the probe must still
// recognise it, type /usage and parse the panel instead of timing out.
func TestDevinProbeAcceptsItsPromptGlyph(t *testing.T) {
	isolate(t)
	const timeout = 5 * time.Second
	p := PTYProvider{ID: "devin", Binary: helperBinary(t, "devin-tui"), Timeout: timeout}
	started := time.Now() // wall-clock: pacing
	o := p.Fetch(t.Context())
	elapsed := time.Since(started) // wall-clock: pacing
	if o.Error != "" {
		t.Fatalf("probe failed after %s: %s", elapsed, o.Error)
	}
	var weekly *Bucket
	for i := range o.Buckets {
		if o.Buckets[i].Window == "7d" {
			weekly = &o.Buckets[i]
		}
	}
	if weekly == nil || weekly.UsedPct == nil || *weekly.UsedPct != 62 {
		t.Fatalf("expected a 7d bucket at 62%%, got %+v", o.Buckets)
	}
	if elapsed >= timeout-time.Second {
		t.Fatalf("probe took %s, expected well inside %s", elapsed, timeout)
	}
}
