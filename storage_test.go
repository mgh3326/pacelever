package pacelever

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateIDsAndFraming(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if e := os.WriteFile(path, []byte(`{"origin_id":9007199254740993}`), 0600); e != nil {
		t.Fatal(e)
	}
	var raw map[string]any
	if e := ReadJSON(path, &raw); e != nil || mapInt(raw["origin_id"]) != 9007199254740993 {
		t.Fatal("state origin id lost precision")
	}
	if e := os.WriteFile(path, []byte(`{"origin_id":1}{"origin_id":2}`), 0600); e != nil {
		t.Fatal(e)
	}
	if ReadJSON(path, &raw) == nil {
		t.Fatal("multiple JSON state records accepted")
	}
}
