package pacelever

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

//go:embed served_models.json
var servedModels []byte

type ServedModel struct {
	Profile  string `json:"profile"`
	Slug     string `json:"slug"`
	Recorded string `json:"recorded"`
	AsOf     string `json:"as_of"`
}
type ModelVerdict struct {
	ServedModel
	Live   *string `json:"live"`
	Status string  `json:"status"`
}

func ServedModels() []ServedModel {
	var rows []ServedModel
	_ = json.Unmarshal(servedModels, &rows)
	return rows
}
func VerifyModels(ctx context.Context, rows []ServedModel, probe func(context.Context, string) (string, error)) ([]ModelVerdict, int) {
	out := make([]ModelVerdict, 0, len(rows))
	code := 0
	for _, r := range rows {
		live, e := probe(ctx, r.Slug)
		v := ModelVerdict{ServedModel: r, Status: "unknown"}
		if e == nil && live != "" {
			v.Live = &live
			if r.Recorded != "" {
				v.Status = "match"
				if live != r.Recorded {
					v.Status = "drift"
					code = 1
				}
			}
		}
		if v.Status == "unknown" && code == 0 {
			code = 2
		}
		out = append(out, v)
	}
	return out, code
}
func ProbeServedModel(ctx context.Context, slug string) (string, error) {
	home, _ := os.UserHomeDir()
	token := os.Getenv("CLINE_API_KEY")
	if token == "" {
		path := firstEnv("PACELEVER_OPENCODE_AUTH", "SCOPEFUEL_OPENCODE_AUTH")
		if path == "" {
			path = filepath.Join(home, ".local/share/opencode/auth.json")
		}
		token = str(object(readObject(path)["cline-pass"])["key"])
	}
	if token == "" {
		return "", fmt.Errorf("credentials unavailable")
	}
	body, _ := json.Marshal(map[string]any{"model": slug, "messages": []map[string]string{{"role": "user", "content": "p"}}, "max_tokens": 1, "stream": false})
	req, e := http.NewRequestWithContext(ctx, "POST", "https://api.cline.bot/api/v1/chat/completions", bytes.NewReader(body))
	if e != nil {
		return "", fmt.Errorf("model probe unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, e := client.Do(req)
	if e != nil {
		return "", fmt.Errorf("model probe failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("model probe HTTP %d", resp.StatusCode)
	}
	var data struct {
		Model string `json:"model"`
		Data  struct {
			Model string `json:"model"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data) != nil {
		return "", fmt.Errorf("model probe JSON invalid")
	}
	if data.Data.Model != "" {
		return data.Data.Model, nil
	}
	return data.Model, nil
}

type staleState struct {
	CheckedAt string `json:"checked_at"`
	Commit    string `json:"commit"`
	Warning   string `json:"warning"`
}

// StaleBuildWarning is advisory and bounded by two seconds. Unknown/offline
// results are silent. It never installs or updates anything.
func StaleBuildWarning(ctx context.Context, commit string, client HTTPDoer) string {
	if commit == "" || commit == "unknown" || commit == "dev" {
		return ""
	}
	switch normalize(firstEnv("PACELEVER_STALE_WARN", "SCOPEFUEL_STALE_WARN")) {
	case "0", "off", "false", "no":
		return ""
	}
	path := filepath.Join(CacheDir(), "stale-build.json")
	var cache staleState
	if ReadJSON(path, &cache) == nil && cache.Commit == commit {
		if at, e := ParseTime(cache.CheckedAt); e == nil && time.Since(at) >= 0 && time.Since(at) < time.Hour {
			return cache.Warning
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	req, e := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/mgh3326/pacelever/compare/"+urlCommit(commit)+"...main", nil)
	if e != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, e := client.Do(req)
	warning := ""
	if e == nil {
		defer resp.Body.Close()
		var v struct {
			AheadBy int    `json:"ahead_by"`
			Status  string `json:"status"`
		}
		if resp.StatusCode == 200 && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v) == nil && v.AheadBy > 0 {
			warning = fmt.Sprintf("pacelever build is %d commits behind main; review a newer release", v.AheadBy)
		}
	}
	_ = WriteJSON(path, staleState{CheckedAt: ClockNow(nil).Format(time.RFC3339Nano), Commit: commit, Warning: warning})
	return warning
}
func urlCommit(s string) string {
	for _, c := range s {
		if !((c >= 'a' && c <= 'f') || (c >= '0' && c <= '9')) {
			return "unknown"
		}
	}
	return s
}
