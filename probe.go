package pacelever

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type PTYProvider struct {
	ID      string
	Binary  string
	Timeout time.Duration
	// Guard supplies a parent-death watchdog. The CLI provides GuardProcess.
	Guard func(int) (func(), error)
}

func ResolveBinary(pool string, cfg Config) string {
	name := map[string]string{"commandcode": "cmd", "kiro": "kiro-cli"}[pool]
	if name == "" {
		name = pool
	}
	explicit := firstEnv("PACELEVER_"+strings.ToUpper(pool)+"_BIN", "SCOPEFUEL_"+strings.ToUpper(pool)+"_BIN")
	if explicit == "" {
		explicit = cfg.Pools[pool].Bin
	}
	if explicit != "" {
		return expandHome(explicit)
	}
	if path, e := exec.LookPath(name); e == nil {
		return path
	}
	if pool == "commandcode" {
		for _, dir := range []string{"~/.hermes/node/bin", "~/.local/bin", "~/.npm-global/bin", "/usr/local/bin", "/opt/homebrew/bin"} {
			p := filepath.Join(expandHome(dir), name)
			if info, e := os.Stat(p); e == nil && info.Mode()&0111 != 0 {
				return p
			}
		}
	}
	return name
}
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}
func (p PTYProvider) Fetch(ctx context.Context) Observation {
	result := Observation{ID: p.ID, Source: "cli:/usage"}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 35 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	e := WithFileLock(filepath.Join(CacheDir(), "probe-"+p.ID+".lock"), true, func() error {
		text, e := p.screen(ctx)
		if e != nil {
			result.Error = e.Error()
			result.ErrorKind = "probe_error"
		} else {
			result = ParseScreen(p.ID, text, time.Now().UTC())
		}
		if p.ID == "devin" {
			models := exec.CommandContext(ctx, p.Binary, "models", "list")
			models.WaitDelay = time.Second
			var output boundedBuffer
			models.Stdout = &output
			models.Stderr = io.Discard
			if models.Run() == nil {
				free := ParseDevinModels(output.String())
				result.Buckets = append(result.Buckets, free...)
				if len(free) > 0 {
					result.Error = ""
					result.ErrorKind = ""
				}
			}
		}
		return nil
	})
	if e == ErrBusy {
		return Observation{ID: p.ID, Error: "probe already in progress", ErrorKind: "probe_in_progress"}
	}
	if e != nil {
		result.Error = "probe lock unavailable"
		result.ErrorKind = "probe_error"
	}
	return result
}

type boundedBuffer struct{ b []byte }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.b) < 1<<20 {
		left := (1 << 20) - len(b.b)
		if len(p) > left {
			p = p[:left]
		}
		b.b = append(b.b, p...)
	}
	return n, nil
}
func (b *boundedBuffer) String() string { return string(b.b) }
func (p PTYProvider) screen(ctx context.Context) (string, error) {
	binary, e := exec.LookPath(p.Binary)
	if e != nil {
		return "", fmt.Errorf("probe executable unavailable")
	}
	base := filepath.Join(CacheDir(), "probes", p.ID)
	if e = os.MkdirAll(base, 0700); e != nil {
		return "", fmt.Errorf("probe directory unavailable")
	}
	dir, e := os.MkdirTemp(base, "run-")
	if e != nil {
		return "", fmt.Errorf("probe directory unavailable")
	}
	defer os.RemoveAll(dir)
	// Isolated empty repository prevents probes from reading a caller's project.
	if e = os.Mkdir(filepath.Join(dir, ".git"), 0700); e != nil {
		return "", fmt.Errorf("probe repository unavailable")
	}
	args := []string{}
	if p.ID == "commandcode" {
		args = append(args, "--no-session")
	}
	if p.ID == "devin" {
		args = append(args, "--respect-workspace-trust", "false")
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=1")
	terminal, e := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 200})
	if e != nil {
		return "", fmt.Errorf("cannot start PTY probe")
	}
	var stopGuard func()
	if p.Guard != nil {
		stopGuard, e = p.Guard(cmd.Process.Pid)
		if e != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = terminal.Close()
			_ = cmd.Wait()
			return "", fmt.Errorf("cannot guard probe lifetime")
		}
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = terminal.Close()
		_ = cmd.Wait()
		if stopGuard != nil {
			stopGuard()
		}
	}()
	chunks := make(chan []byte, 8)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(chunks)
		buf := make([]byte, 8192)
		for {
			n, e := terminal.Read(buf)
			if n > 0 {
				piece := append([]byte(nil), buf[:n]...)
				select {
				case chunks <- piece:
				case <-done:
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	started := time.Now()
	typed := false
	submitted := false
	typedAt := time.Time{}
	var out boundedBuffer
	lastChange := started
	stage := "no-output"
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("probe timeout stage=%s bytes=%d", stage, len(out.b))
		case chunk, ok := <-chunks:
			if !ok {
				result := ParseScreen(p.ID, out.String(), time.Now().UTC())
				if result.Error == "" {
					return out.String(), nil
				}
				return "", fmt.Errorf("probe exited before complete usage panel stage=%s", stage)
			}
			_, _ = out.Write(chunk)
			lastChange = time.Now()
			stage = "no-prompt"
			if submitted {
				stage = "unparsed"
				parsed := ParseScreen(p.ID, out.String(), time.Now().UTC())
				if parsed.Error == "" {
					return out.String(), nil
				}
			}
		case <-ticker.C:
			clean := CleanScreen(out.String())
			lower := strings.ToLower(clean)
			if strings.Contains(lower, "trust this folder") || strings.Contains(lower, "do you trust the contents") {
				return "", fmt.Errorf("probe requires workspace trust stage=trust-prompt")
			}
			ready := strings.Contains(clean, "❯") || strings.Contains(clean, "›") || strings.Contains(clean, "> ") || strings.Contains(clean, "Shift+Tab") || p.ID == "kiro"
			if !typed && ready && time.Since(started) > 400*time.Millisecond {
				if _, e = terminal.Write([]byte("/usage")); e != nil {
					return "", fmt.Errorf("probe input failed")
				}
				typed = true
				typedAt = time.Now()
				stage = "no-panel"
			}
			// Input text and Enter must never share a write. Wait for the terminal
			// echo or a bounded input-settle delay; banner mentions do not count.
			if typed && !submitted && time.Since(typedAt) >= 500*time.Millisecond {
				if _, e = terminal.Write([]byte("\r")); e != nil {
					return "", fmt.Errorf("probe submit failed")
				}
				submitted = true
			}
			if submitted && time.Since(lastChange) > 250*time.Millisecond {
				if parsed := ParseScreen(p.ID, out.String(), time.Now().UTC()); parsed.Error == "" {
					return out.String(), nil
				}
			}
		}
	}
}

// GuardProcess starts a tiny watchdog with a parent-owned pipe. SIGKILL closes
// that pipe in the kernel, so descendants are reaped without a user-space defer.
func GuardProcess(pgid int) (func(), error) {
	exe, e := os.Executable()
	if e != nil {
		return nil, e
	}
	r, w, e := os.Pipe()
	if e != nil {
		return nil, e
	}
	cmd := exec.Command(exe, "_reap", strconv.Itoa(pgid))
	cmd.ExtraFiles = []*os.File{r}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if e = cmd.Start(); e != nil {
		r.Close()
		w.Close()
		return nil, e
	}
	r.Close()
	return func() { w.Close(); _ = cmd.Wait() }, nil
}
func RunReaper(pgid int) error {
	if pgid <= 1 {
		return fmt.Errorf("invalid process group")
	}
	f := os.NewFile(3, "parent-lifetime")
	if f == nil {
		return fmt.Errorf("missing lifetime pipe")
	}
	defer f.Close()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, f); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	return nil
}

// StartRefresh detaches only a pacelever worker, never a service. The worker
// owns its deadline and kernel lock independently of the initiating caller.
func StartRefresh(pool string) error {
	if !knownPool(pool) || pool == "anthropic-api" {
		return fmt.Errorf("unknown automatic pool")
	}
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	cmd := exec.Command(exe, "refresh", pool, "--_worker")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if e = cmd.Start(); e != nil {
		return e
	}
	return cmd.Process.Release()
}
