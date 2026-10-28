package pacelever

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestModelDrift(t *testing.T) {
	rows := []ServedModel{{Profile: "example", Slug: "example/alias", Recorded: "example/model-v1"}}
	for _, tt := range []struct {
		live   string
		err    error
		status string
		code   int
	}{{"example/model-v1", nil, "match", 0}, {"example/model-v2", nil, "drift", 1}, {"", fmt.Errorf("unavailable"), "unknown", 2}} {
		got, code := VerifyModels(context.Background(), rows, func(context.Context, string) (string, error) { return tt.live, tt.err })
		if code != tt.code || got[0].Status != tt.status {
			t.Fatalf("%+v code=%d", got, code)
		}
	}
}
func TestStaleWarningAdvisoryCache(t *testing.T) {
	isolate(t)
	calls := 0
	client := doFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(200, `{"ahead_by":2,"status":"ahead"}`), nil
	})
	warning := StaleBuildWarning(context.Background(), "abc123", client)
	if !strings.Contains(warning, "2 commits") {
		t.Fatal(warning)
	}
	_ = StaleBuildWarning(context.Background(), "abc123", client)
	if calls != 1 {
		t.Fatalf("warning polled %d times", calls)
	}
}
