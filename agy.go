package pacelever

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// agyProvider measures Antigravity quota over HTTP. A refused token is re-armed
// once by running the CLI's model listing, which refreshes the OAuth file as a
// side effect; a shared lock keeps the refresh single-flight.
type agyProvider struct {
	client  HTTPDoer
	now     func() time.Time
	file    func() string
	secrets agySecrets
	wake    func(context.Context) error
	// wakeLock serializes the CLI refresh across collectors whose cache
	// directories differ (default runs and herdr events). Empty relies on
	// the caller's own serialization.
	wakeLock string
	// The app's local server source (agy_local.go): running processes,
	// their listening ports and the loopback client. localProcs nil means
	// no local source; localPorts nil skips the TLS-port fallback.
	localProcs func(context.Context) []agyLocalProc
	localPorts func(context.Context, int) []int
	localHTTP  HTTPDoer
}

// agySecrets is an optional credential fallback store. On macOS it is the
// login keychain entry the Antigravity CLI writes through go-keyring.
type agySecrets interface {
	agyToken(context.Context) string
}

func newAgyProvider(home string, cfg Config, client HTTPDoer) agyProvider {
	p := agyProvider{
		client:     client,
		file:       func() string { return agyToken(home) },
		localProcs: agyLocalProcesses,
		localPorts: agyListenPorts,
	}
	if runtime.GOOS == "darwin" {
		p.secrets = agyKeychain{}
	}
	if binary, e := exec.LookPath(ResolveBinary("agy", cfg)); e == nil {
		p.wake = agyWake(binary)
		p.wakeLock = filepath.Join(CacheDir(), "agy-wake.lock")
	}
	return p
}

func (p agyProvider) fileToken() string {
	if p.file == nil {
		return ""
	}
	return p.file()
}

func (p agyProvider) secretToken(ctx context.Context) string {
	if p.secrets == nil {
		return ""
	}
	return p.secrets.agyToken(ctx)
}

func (p agyProvider) Fetch(ctx context.Context) Observation {
	now := clockFunc(p.now)
	// The running app's own server answers without a stored credential, so
	// it is strictly safer than the token path and goes first; any local
	// failure falls back to today's cloud source unchanged.
	if o, ok := p.local(ctx); ok {
		return o
	}
	token := p.fileToken()
	usedSecret := false
	if token == "" {
		if token = p.secretToken(ctx); token != "" {
			usedSecret = true
		}
	}
	if token == "" {
		return failed("agy", ErrKindCredentials, msgNoCredentials)
	}
	o := fetchAgy(ctx, token, p.client, now)
	if o.ErrorKind != ErrKindAuthFailed {
		return o
	}
	secretRefused := usedSecret
	switch {
	case p.wake == nil:
		o.Error = "antigravity authentication failed; the agy command is unavailable to refresh the login"
	default:
		fresh, freshSecret, e := p.refresh(ctx, token, usedSecret)
		if e != nil {
			o.Error = "antigravity authentication failed; `agy models` could not refresh the login"
			break
		}
		token, usedSecret = fresh, freshSecret
		if o = fetchAgy(ctx, token, p.client, now); o.ErrorKind != ErrKindAuthFailed {
			return o
		}
		if usedSecret {
			secretRefused = true
		}
	}
	// A file credential that is missing or still refused after the refresh
	// falls back to the keychain copy the CLI keeps. A refused fallback keeps
	// the refresh hint rather than masking it behind the same auth failure.
	if !secretRefused {
		if secret := p.secretToken(ctx); secret != "" && secret != token {
			if alt := fetchAgy(ctx, secret, p.client, now); alt.Error == "" {
				o = alt
			}
		}
	}
	return o
}

// refresh re-arms a refused token under a lock shared by collectors whose
// cache directories differ. A token another holder just refreshed is reused
// instead of waking the CLI again. It returns the token to retry with.
func (p agyProvider) refresh(ctx context.Context, token string, usedSecret bool) (string, bool, error) {
	// Re-read whichever store supplied the token: the wake or a concurrent
	// refresh may have replaced it in either one.
	latest := func() (string, bool) {
		if fresh := p.fileToken(); fresh != "" {
			return fresh, false
		}
		if usedSecret {
			if fresh := p.secretToken(ctx); fresh != "" {
				return fresh, true
			}
		}
		return token, usedSecret
	}
	if p.wakeLock == "" {
		if e := p.wake(ctx); e != nil {
			return token, usedSecret, e
		}
		next, secret := latest()
		return next, secret, nil
	}
	next, nextSecret := token, usedSecret
	e := WithFileLock(p.wakeLock, false, func() error {
		if fresh, secret := latest(); fresh != token {
			next, nextSecret = fresh, secret
			return nil
		}
		if e := p.wake(ctx); e != nil {
			return e
		}
		next, nextSecret = latest()
		return nil
	})
	return next, nextSecret, e
}

// agyWake re-arms the OAuth token with one model listing. The CLI output may
// carry account details, so it is discarded and never logged.
func agyWake(binary string) func(context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "models")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		return cmd.Run()
	}
}

type agyKeychain struct{}

func (agyKeychain) agyToken(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, "security", "find-generic-password", "-s", "gemini", "-a", "antigravity", "-w").Output()
	if e != nil {
		return ""
	}
	return agyEnvelopeToken(strings.TrimSpace(string(out)))
}

// agyEnvelopeToken unwraps the go-keyring envelope and reads
// $.token.access_token from the enclosed JSON.
func agyEnvelopeToken(raw string) string {
	if s := strings.TrimPrefix(raw, "go-keyring-base64:"); s != raw {
		decoded, e := base64.StdEncoding.DecodeString(s)
		if e != nil {
			return ""
		}
		raw = string(decoded)
	} else if s := strings.TrimPrefix(raw, "go-keyring-encoded:"); s != raw {
		decoded, e := hex.DecodeString(s)
		if e != nil {
			return ""
		}
		raw = string(decoded)
	}
	var data map[string]any
	if json.Unmarshal([]byte(raw), &data) != nil {
		return ""
	}
	return strings.TrimSpace(str(object(data["token"])["access_token"]))
}

func agyToken(home string) string {
	for _, path := range []string{
		filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"),
		filepath.Join(configBase(), "antigravity-cli", "antigravity-oauth-token"),
	} {
		token := strings.TrimSpace(str(object(readObject(path)["token"])["access_token"]))
		if token != "" {
			return token
		}
	}
	return ""
}

func fetchAgy(ctx context.Context, token string, client HTTPDoer, now func() time.Time) Observation {
	if token == "" {
		return failed("agy", ErrKindCredentials, msgNoCredentials)
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "User-Agent": "antigravity", "Content-Type": "application/json"}
	var last httpJSON
	gotStatus := false
	for _, base := range []string{"https://daily-cloudcode-pa.googleapis.com", "https://cloudcode-pa.googleapis.com"} {
		resp, e := doJSON(ctx, client, "POST", base+"/v1internal:retrieveUserQuotaSummary", headers, []byte("{}"))
		if e != nil {
			continue
		}
		last, gotStatus = resp, true
		if resp.Status == 401 || resp.Status == 403 {
			return statusObservation("agy", resp.Status, resp.Header, now(), ErrKindAuthFailed, "antigravity authentication failed; run agy to refresh the login", 0)
		}
		if resp.Status == 200 && !resp.ParseError {
			return parseAgy(resp.Body, fetchAgyModels(ctx, headers, client, base))
		}
	}
	if gotStatus && last.Status == 200 {
		return failed("agy", ErrKindParse, msgInvalidJSON)
	}
	if gotStatus {
		o := statusObservation("agy", last.Status, last.Header, now(), ErrKindAuthFailed, "antigravity authentication failed; run agy to refresh the login", 0)
		if last.Status >= 500 {
			// A server error is retryable, not a sign the login expired.
			o.ErrorKind = ErrKindNetwork
			o.Error = "antigravity temporarily unavailable"
		}
		return o
	}
	return failed("agy", ErrKindNetwork, msgRequestFailed)
}

// fetchAgyModels reads the per-model quota list. The summary groups omit a
// metering group when it is not reported for the account; each model's
// quotaInfo still carries that group's session (5h) quota. Best effort: a
// failure keeps the summary-only observation. The answered host goes first.
func fetchAgyModels(ctx context.Context, headers map[string]string, client HTTPDoer, answered string) map[string]any {
	hosts := []string{answered}
	for _, base := range []string{"https://daily-cloudcode-pa.googleapis.com", "https://cloudcode-pa.googleapis.com"} {
		if base != answered {
			hosts = append(hosts, base)
		}
	}
	for _, base := range hosts {
		resp, e := doJSON(ctx, client, "POST", base+"/v1internal:fetchAvailableModels", headers, []byte("{}"))
		if e != nil || resp.Status != 200 || resp.ParseError {
			continue
		}
		root := resp.Body
		if response := object(root["response"]); len(response) > 0 {
			root = response
		}
		return object(root["models"])
	}
	return nil
}

func parseAgy(raw map[string]any, extraModels map[string]any) Observation {
	o := Observation{ID: "agy", Source: SourceUsageAPI}
	root := raw
	if response := object(raw["response"]); len(response) > 0 {
		root = response
	}
	worst := map[string]Bucket{}
	add := func(group, window string, fraction float64, reset string) {
		// An unmappable identity is emitted as scope group/unknown: visible in
		// output but never matched by the gate, so it fails closed instead of
		// passing as an account-wide bucket.
		if group == "" {
			group = "unknown"
		}
		if window == "" || math.IsNaN(fraction) || fraction < 0 || fraction > 1 {
			return
		}
		h := horizon(window)
		if window == "unknown" {
			h = ""
		}
		bucket := Bucket{Label: window, Window: window, UsedPct: Pct(100 * (1 - fraction)), ResetsAt: reset, Scope: Scope{Kind: ScopeGroup, Name: group}, Horizon: h}
		key := group + "\x00" + window
		if previous, ok := worst[key]; !ok || (previous.UsedPct != nil && bucket.UsedPct != nil && *bucket.UsedPct > *previous.UsedPct) {
			worst[key] = bucket
		}
	}
	for _, groupValue := range list(root["groups"]) {
		group := object(groupValue)
		display := str(group["displayName"]) + " " + str(group["description"])
		for _, value := range list(group["buckets"]) {
			bucket := object(value)
			fraction, ok := finiteNumber(bucket["remainingFraction"])
			if !ok {
				continue
			}
			id := str(bucket["bucketId"])
			add(agyGroup(id, display), agyWindow(str(bucket["window"]), id), fraction, resetAtValue(bucket["resetTime"]))
		}
	}
	addModels := func(models map[string]any) {
		for key, value := range models {
			entry := object(value)
			if entry["isInternal"] == true {
				continue
			}
			quota := object(entry["quotaInfo"])
			fraction, ok := finiteNumber(quota["remainingFraction"])
			if !ok {
				continue
			}
			window := agyWindow(str(quota["window"]), key)
			switch {
			case window != "":
			case strings.TrimSpace(str(quota["window"])) == "":
				// Per-model quotaInfo without a window is the session quota.
				window = "5h"
			default:
				// An explicit window we do not recognize stays unmetered
				// instead of masquerading as the session quota.
				window = "unknown"
			}
			add(agyGroup(key, str(entry["displayName"])), window, fraction, resetAtValue(quota["resetTime"]))
		}
	}
	if len(worst) == 0 {
		addModels(object(root["models"]))
	}
	addModels(extraModels)
	for _, group := range []string{"gemini", "3p", "unknown"} {
		for _, window := range []string{"5h", "7d", "30d", "1d", "unknown"} {
			if bucket, ok := worst[group+"\x00"+window]; ok {
				o.Buckets = append(o.Buckets, bucket)
			}
		}
	}
	for _, key := range []string{"planDaily", "plan"} {
		plan := object(raw[key])
		name := str(object(plan["paidTier"])["name"])
		if name == "" {
			name = str(object(plan["currentTier"])["name"])
		}
		if name != "" {
			o.Plan = strings.ToUpper(name)
			break
		}
	}
	if len(o.Buckets) == 0 {
		o.fail(ErrKindParse, msgNoWindows)
	}
	return o
}

func agyWindow(window, bucketID string) string {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "weekly":
		return "7d"
	case "5h", "session":
		return "5h"
	case "daily":
		return "1d"
	}
	id := strings.ToLower(bucketID)
	switch {
	case strings.HasSuffix(id, "-weekly"):
		return "7d"
	case strings.HasSuffix(id, "-5h"):
		return "5h"
	}
	return ""
}

// agyGroup maps a bucket or model identity to the metered account group.
// A gemini model must never be gated on the Claude/GPT (3p) numbers.
func agyGroup(id, display string) string {
	identity := strings.ToLower(strings.TrimSpace(id))
	switch {
	case strings.HasPrefix(identity, "gemini"):
		return "gemini"
	case strings.HasPrefix(identity, "3p-"), strings.HasPrefix(identity, "3p_"):
		return "3p"
	}
	text := strings.ToLower(display + " " + identity)
	switch {
	case strings.Contains(text, "gemini"):
		return "gemini"
	case strings.Contains(text, "claude"), strings.Contains(text, "gpt"), strings.Contains(text, "oss"):
		return "3p"
	}
	return "unknown"
}
