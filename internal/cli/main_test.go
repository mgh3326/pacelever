package cli

import (
	"fmt"
	"os"
	"testing"

	pl "github.com/mgh3326/pacelever"
)

// TestMain applies PACELEVER_TEST_CLOCK_OFFSET; see the root package's.
func TestMain(m *testing.M) {
	if v := os.Getenv("PACELEVER_TEST_CLOCK_OFFSET"); v != "" {
		d, err := pl.Duration(v)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid PACELEVER_TEST_CLOCK_OFFSET")
			os.Exit(2)
		}
		pl.SetWallClockOffset(d)
	}
	os.Exit(m.Run())
}
