package pacelever

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain applies PACELEVER_TEST_CLOCK_OFFSET (for example "400d") so the
// suite can run as if it were a later date. Tests read "now" with
// ClockNow(nil), never time.Now, so they move with the code under test.
func TestMain(m *testing.M) {
	if v := os.Getenv("PACELEVER_TEST_CLOCK_OFFSET"); v != "" {
		d, err := Duration(v)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid PACELEVER_TEST_CLOCK_OFFSET")
			os.Exit(2)
		}
		SetWallClockOffset(d)
	}
	os.Exit(m.Run())
}

// TestWallClockReadsAreMarked keeps every direct wall-clock read deliberate:
// code and tests read "now" through ClockNow, and the few elapsed-time pacing
// loops (probe deadlines, lease waits) carry a "// wall-clock:" note.
func TestWallClockReadsAreMarked(t *testing.T) {
	var files []string
	for _, pattern := range []string{"*.go", "internal/cli/*.go", "cmd/pacelever/*.go"} {
		m, _ := filepath.Glob(pattern)
		files = append(files, m...)
	}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if (strings.Contains(code, "time.Now()") || strings.Contains(code, "time.Since(") || strings.Contains(code, "time.Until(")) && !strings.Contains(line, "// wall-clock:") {
				t.Errorf("%s:%d reads the wall clock; use ClockNow or mark pacing with // wall-clock:", name, i+1)
			}
		}
	}
}
