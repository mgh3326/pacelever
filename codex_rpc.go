package pacelever

// Codex JSON-RPC source ported from ClaudeBar (MIT, github.com/tddworks/ClaudeBar
// Modules/Providers/Resources/Providers/codex.json "rpc" data source): spawn
// `codex -s read-only -a never app-server`, handshake initialize/initialized,
// then call account/rateLimits/read and account/read. The probe never writes
// credentials and runs only when a file login is already present, so it cannot
// surface a browser sign-in; on any failure the caller falls back to the wham
// usage API.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// codexRPCTransport is the stdin/stdout line channel of one app-server process.
// Tests inject a fake so no test spawns a real CLI.
type codexRPCTransport interface {
	Send([]byte) error
	Recv() ([]byte, error)
	Close()
}

// codexRPCStart is the spawn seam; tests replace it with a fixture transport.
var codexRPCStart = spawnCodexAppServer

// codexRPCTimeout bounds the whole handshake + calls exchange.
const codexRPCTimeout = 15 * time.Second

type codexProcTransport struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	rd   *bufio.Reader
	wait chan error
	once sync.Once
}

func spawnCodexAppServer(ctx context.Context, bin, workdir, codexHome string) (codexRPCTransport, error) {
	cmd := exec.Command(bin, "-s", "read-only", "-a", "never", "app-server")
	cmd.Dir = workdir
	cmd.Stderr = io.Discard
	if codexHome != "" {
		cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	}
	in, e := cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	if e = cmd.Start(); e != nil {
		return nil, e
	}
	t := &codexProcTransport{cmd: cmd, in: in, rd: bufio.NewReader(out), wait: make(chan error, 1)}
	go func() { t.wait <- cmd.Wait() }()
	go func() {
		<-ctx.Done()
		t.Close()
	}()
	return t, nil
}

func (t *codexProcTransport) Send(b []byte) error {
	_, e := t.in.Write(append(b, '\n'))
	return e
}

func (t *codexProcTransport) Recv() ([]byte, error) { return t.rd.ReadBytes('\n') }

// Close kills the app-server and reaps it; every spawn must be closed so probe
// runs cannot leak `codex app-server` processes.
func (t *codexProcTransport) Close() {
	t.once.Do(func() {
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		_ = t.in.Close()
		select {
		case <-t.wait:
		case <-time.After(2 * time.Second):
		}
	})
}

// codexBinary resolves the codex CLI: explicit override, PATH, then the desktop
// app's bundled copy (ClaudeBar's `cli` list).
func codexBinary(home string, cfg Config) string {
	if explicit := firstEnv("PACELEVER_CODEX_BIN", "SCOPEFUEL_CODEX_BIN"); explicit != "" {
		return expandHome(explicit)
	}
	if cfg.Pools["codex"].Bin != "" {
		return expandHome(cfg.Pools["codex"].Bin)
	}
	if path, e := exec.LookPath("codex"); e == nil {
		return path
	}
	if runtime.GOOS == "darwin" {
		for _, path := range []string{
			"/Applications/Codex.app/Contents/Resources/codex",
			filepath.Join(home, "Applications", "Codex.app", "Contents", "Resources", "codex"),
		} {
			if st, e := os.Stat(path); e == nil && !st.IsDir() {
				return path
			}
		}
	}
	return ""
}

type codexRPCMessage struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func codexRPCSend(t codexRPCTransport, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return t.Send(b)
}

// codexRPCCall sends one request and waits for its response, skipping
// server notifications (messages with no id), like ClaudeBar's exchange.
func codexRPCCall(t codexRPCTransport, id int, method string, params any) (json.RawMessage, error) {
	if e := codexRPCSend(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); e != nil {
		return nil, e
	}
	want := strconv.Itoa(id)
	for {
		line, e := t.Recv()
		if e != nil {
			return nil, e
		}
		var msg codexRPCMessage
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		if len(msg.ID) == 0 || string(msg.ID) != want {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("rpc: %s", msg.Error.Message)
		}
		return msg.Result, nil
	}
}

type codexRPCWindow struct {
	UsedPct  *float64 `json:"usedPercent"`
	ResetsAt float64  `json:"resetsAt"`
	Mins     float64  `json:"windowDurationMins"`
}

func (w codexRPCWindow) bucket(scope Scope) Bucket {
	label := codexWindowLabel(w.Mins * 60)
	reset := ""
	if w.ResetsAt > 0 {
		reset = time.Unix(int64(w.ResetsAt), 0).UTC().Format(time.RFC3339Nano)
	}
	return Bucket{Label: label, Window: label, UsedPct: validated(w.UsedPct), ResetsAt: reset, Scope: scope, Horizon: horizon(label)}
}

// codexLimitName trims the codex prefix from a per-limit id, matching
// ClaudeBar's dropPrefixes ("codex_" → capitalize, "codex " → drop).
func codexLimitName(s string) string {
	if len(s) > len("codex_") && strings.EqualFold(s[:len("codex_")], "codex_") {
		rest := s[len("codex_"):]
		return strings.ToUpper(rest[:1]) + rest[1:]
	}
	if len(s) > len("codex ") && strings.EqualFold(s[:len("codex ")], "codex ") {
		return s[len("codex "):]
	}
	return s
}

// fetchCodexRPC runs the whole exchange. It returns ok=false on any failure so
// the caller falls back to the wham usage API; it never invents a value.
func fetchCodexRPC(ctx context.Context, home string, cfg Config, codexDir string) (Observation, bool) {
	o := Observation{ID: "codex", Source: "usage-rpc"}
	bin := codexBinary(home, cfg)
	if bin == "" {
		return o, false
	}
	ctx, cancel := context.WithTimeout(ctx, codexRPCTimeout)
	defer cancel()
	t, e := codexRPCStart(ctx, bin, codexDir, codexDir)
	if e != nil {
		return o, false
	}
	defer t.Close()
	if _, e = codexRPCCall(t, 1, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "pacelever", "version": "1"},
	}); e != nil {
		return o, false
	}
	if e = codexRPCSend(t, map[string]any{"jsonrpc": "2.0", "method": "initialized"}); e != nil {
		return o, false
	}
	result, e := codexRPCCall(t, 2, "account/rateLimits/read", map[string]any{})
	if e != nil {
		return o, false
	}
	var rl struct {
		RateLimits struct {
			PlanType  string          `json:"planType"`
			Primary   *codexRPCWindow `json:"primary"`
			Secondary *codexRPCWindow `json:"secondary"`
		} `json:"rateLimits"`
		ByLimit map[string]struct {
			LimitName string          `json:"limitName"`
			LimitID   string          `json:"limitId"`
			Primary   *codexRPCWindow `json:"primary"`
			Secondary *codexRPCWindow `json:"secondary"`
		} `json:"rateLimitsByLimitId"`
	}
	if json.Unmarshal(result, &rl) != nil {
		return o, false
	}
	o.Plan = rl.RateLimits.PlanType
	for _, w := range []*codexRPCWindow{rl.RateLimits.Primary, rl.RateLimits.Secondary} {
		if w != nil {
			o.Buckets = append(o.Buckets, w.bucket(Scope{Kind: "account"}))
		}
	}
	var ids []string
	for id := range rl.ByLimit {
		if id != "codex" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		x := rl.ByLimit[id]
		name := x.LimitName
		if name == "" {
			name = x.LimitID
		}
		if name == "" {
			name = id
		}
		name = codexLimitName(name)
		for _, w := range []*codexRPCWindow{x.Primary, x.Secondary} {
			if w != nil {
				o.Buckets = append(o.Buckets, w.bucket(Scope{Kind: "model", Name: name}))
			}
		}
	}
	if len(o.Buckets) == 0 {
		if rl.RateLimits.PlanType != "free" {
			return o, false
		}
		o.Buckets = append(o.Buckets, Bucket{Label: "free", Window: "free", UsedPct: Pct(0), Scope: Scope{Kind: "account"}, Note: "Free plan"})
	}
	// account/read supplies identity for shared mode; its failure does not
	// discard the limits already read.
	if acct, e := codexRPCCall(t, 3, "account/read", map[string]any{"refreshToken": false}); e == nil {
		var a struct {
			Account *struct {
				Email string `json:"email"`
			} `json:"account"`
		}
		if json.Unmarshal(acct, &a) == nil && a.Account != nil && a.Account.Email != "" {
			if o.PublishFP == "" {
				o.PublishFP = fingerprint("codex-account", a.Account.Email)
				o.PublishFPKind = "account"
			}
		}
	}
	return o, true
}
