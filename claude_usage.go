package pacelever

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ClaudeProvider prefers the OAuth usage API. Claude Code can still answer a
// print-mode /usage request while that endpoint is rate limited, so an API
// failure gets one bounded CLI attempt; a CLI failure returns the API result.
type ClaudeProvider struct {
	API HTTPProvider
	CLI ClaudeCLIProvider
}

func (p ClaudeProvider) Fetch(ctx context.Context) Observation {
	api := p.API.Fetch(ctx)
	if api.Error == "" {
		return api
	}
	cli := p.CLI.Fetch(ctx)
	if cli.Error == "" {
		return cli
	}
	return api
}

// ClaudeCLIProvider reads Claude Code's authenticated non-interactive usage
// output in an empty directory. The command is never run in the caller's repo.
type ClaudeCLIProvider struct {
	Env     map[string]string
	Binary  string
	Timeout time.Duration
	Now     func() time.Time
	Run     func(context.Context, string, string, map[string]string) (string, error)
}

func (p ClaudeCLIProvider) Fetch(ctx context.Context) Observation {
	result := Observation{ID: "claude", Source: SourceCLI}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := filepath.Join(CacheDir(), "probes", "claude-cli")
	if e := os.MkdirAll(base, 0700); e != nil {
		result.Error = "usage CLI directory unavailable"
		result.ErrorKind = ErrKindProbe
		return result
	}
	dir, e := os.MkdirTemp(base, "run-")
	if e != nil {
		result.Error = "usage CLI directory unavailable"
		result.ErrorKind = ErrKindProbe
		return result
	}
	defer os.RemoveAll(dir)
	run := p.Run
	if run == nil {
		run = runClaudeUsageCommand
	}
	text, e := run(probeCtx, p.Binary, dir, p.Env)
	if e != nil {
		result.Error = "usage CLI unavailable"
		result.ErrorKind = ErrKindProbe
		return result
	}
	return ParseClaudeUsageScreen(text, ClockNow(p.Now))
}

func runClaudeUsageCommand(ctx context.Context, binary, dir string, env map[string]string) (string, error) {
	if binary == "" {
		binary = "claude"
	}
	path, e := exec.LookPath(binary)
	if e != nil {
		return "", fmt.Errorf("claude executable unavailable")
	}
	cmd := exec.CommandContext(ctx, path, "-p", "/usage", "--allowed-tools", "")
	cmd.Dir = dir
	cmd.Env = os.Environ()
	if value := env["CLAUDE_CONFIG_DIR"]; value != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+value)
	}
	out := &boundedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	if e = cmd.Run(); e != nil {
		if ctx.Err() != nil && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return "", fmt.Errorf("claude usage command failed")
	}
	return out.String(), nil
}

var claudeUsagePercent = regexp.MustCompile(`(?i)([0-9]{1,3}(?:\.[0-9]+)?)\s*%\s*(used|left)`)
var claudeUsageOnly = regexp.MustCompile(`(?i)\s+only$`)
var claudeUsageReset = regexp.MustCompile(`(?i)\bresets?\s+([A-Za-z]{3,9})\.?\s+([0-9]{1,2})\s+at\s+([0-9]{1,2})(?::([0-9]{2}))?\s*([ap])\.?m\.?\s*\(([A-Za-z0-9_+\-/]+)\)`)

// ParseClaudeUsageScreen parses Claude Code's print-mode /usage output. The
// label-proximity and "% left" rules are ported from ClaudeBar's
// claude-usage-screen.js (MIT), adjusted for non-rendered CLI output.
func ParseClaudeUsageScreen(text string, now time.Time) Observation {
	o := Observation{ID: "claude", Source: SourceCLI}
	fail := func() Observation {
		o.Error = "usage panel incomplete or unrecognized"
		o.ErrorKind = ErrKindParse
		return o
	}
	lines := strings.Split(CleanScreen(text), "\n")
	session, sessionReset, ok := claudeUsageValue(lines, "current session", now)
	if !ok {
		return fail()
	}
	weekly, weeklyReset, ok := claudeUsageValue(lines, "current week (all models)", now)
	if !ok {
		return fail()
	}
	o.Buckets = []Bucket{accountBucket("5h", Pct(session), sessionReset), accountBucket("7d", Pct(weekly), weeklyReset)}
	seen := map[string]bool{}
	for i, line := range lines {
		lower := strings.ToLower(line)
		start := strings.Index(lower, "current week (")
		if start < 0 {
			continue
		}
		name := line[start+len("current week ("):]
		end := strings.Index(name, ")")
		if end < 0 {
			continue
		}
		name = strings.TrimSpace(name[:end])
		if name == "" || strings.EqualFold(name, "all models") {
			continue
		}
		// Claude reports "Sonnet only" as the Sonnet model limit, not a new model.
		name = claudeUsageOnly.ReplaceAllString(name, "")
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		used, ok := claudePercentNear(lines, i)
		if !ok {
			continue
		}
		reset, ok := claudeResetNear(lines, i, now)
		if !ok {
			reset = weeklyReset
		}
		seen[key] = true
		o.Buckets = append(o.Buckets, modelBucket(name, "7d", Pct(used), reset))
	}
	return o
}

func claudeUsageValue(lines []string, label string, now time.Time) (float64, string, bool) {
	label = strings.ToLower(label)
	for i, line := range lines {
		if !strings.Contains(strings.ToLower(line), label) {
			continue
		}
		used, ok := claudePercentNear(lines, i)
		if !ok {
			return 0, "", false
		}
		reset, ok := claudeResetNear(lines, i, now)
		if !ok {
			return 0, "", false
		}
		return used, reset, true
	}
	return 0, "", false
}

func claudePercentNear(lines []string, start int) (float64, bool) {
	for i := start; i < len(lines) && i < start+12; i++ {
		m := claudeUsagePercent.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		v, e := strconv.ParseFloat(m[1], 64)
		if e != nil || v < 0 || v > 100 {
			return 0, false
		}
		if strings.EqualFold(m[2], "left") {
			v = 100 - v
		}
		return v, true
	}
	return 0, false
}

func claudeResetNear(lines []string, start int, now time.Time) (string, bool) {
	for i := start; i < len(lines) && i < start+14; i++ {
		m := claudeUsageReset.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		return parseClaudeReset(m, now)
	}
	return "", false
}

func parseClaudeReset(m []string, now time.Time) (string, bool) {
	months := map[string]time.Month{"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April, "may": time.May, "jun": time.June, "jul": time.July, "aug": time.August, "sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December}
	month, ok := months[strings.ToLower(m[1])[:3]]
	if !ok {
		return "", false
	}
	day, e1 := strconv.Atoi(m[2])
	hour, e2 := strconv.Atoi(m[3])
	minute := 0
	if m[4] != "" {
		var e error
		minute, e = strconv.Atoi(m[4])
		if e != nil {
			return "", false
		}
	}
	if e1 != nil || e2 != nil || day < 1 || day > 31 || hour < 1 || hour > 12 || minute > 59 {
		return "", false
	}
	if strings.EqualFold(m[5], "p") && hour != 12 {
		hour += 12
	}
	if strings.EqualFold(m[5], "a") && hour == 12 {
		hour = 0
	}
	loc, e := time.LoadLocation(m[6])
	if e != nil {
		return "", false
	}
	localNow := now.In(loc)
	reset := time.Date(localNow.Year(), month, day, hour, minute, 0, 0, loc)
	if !reset.After(now) {
		reset = reset.AddDate(1, 0, 0)
	}
	return reset.UTC().Format(time.RFC3339Nano), true
}
