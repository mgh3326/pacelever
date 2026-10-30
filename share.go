package pacelever

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func safeHost(host string) string {
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(host) {
		return "unknown"
	}
	if len(host) > 64 {
		return host[:55] + "-" + fingerprint("host", host)[:8]
	}
	return host
}
func quotaEnabled() bool {
	v := normalize(FirstEnv("PACELEVER_QUOTA_SHARE"))
	return v != "0" && v != "off" && v != "false"
}
func identity(o Observation) (string, string) {
	if o.PublishFP != "" {
		return o.PublishFP, o.PublishFPKind
	}
	return o.AccountFP, o.AccountFPKind
}

type InflightJob struct {
	JobID     string `json:"job_id"`
	StartedAt string `json:"started_at"`
}
type InflightSnapshot struct {
	Schema      string        `json:"schema"`
	Pool        string        `json:"pool"`
	AccountFP   string        `json:"account_fp"`
	Host        string        `json:"host"`
	PublishedAt string        `json:"published_at"`
	Jobs        []InflightJob `json:"jobs"`
}
type InflightResult struct {
	N           *int          `json:"n"`
	LocalKnown  bool          `json:"local_known"`
	RemoteKnown bool          `json:"remote_known"`
	Jobs        []InflightJob `json:"jobs"`
}

func validJob(j InflightJob, now time.Time) bool {
	at, e := ParseTime(j.StartedAt)
	if e != nil {
		if n, e2 := strconv.ParseFloat(j.StartedAt, 64); e2 == nil {
			at = time.UnixMilli(int64(n * 1000))
			e = nil
		}
	}
	age := now.Sub(at)
	return e == nil && j.JobID != "" && age >= -time.Minute && age <= 8*time.Hour
}
func LocalInflight(ctx context.Context, pool string, now time.Time) ([]InflightJob, error) {
	binary := FirstEnv("ARBITER_BIN")
	if binary == "" {
		binary = "arbiter"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "status", "--json")
	var out boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	cmd.WaitDelay = time.Second
	if cmd.Run() != nil {
		return nil, fmt.Errorf("local inflight unavailable")
	}
	var payload struct {
		Records []struct {
			Pool    string          `json:"pool"`
			JobID   string          `json:"job_id"`
			Started json.RawMessage `json:"started_at"`
		} `json:"quota_pool_records"`
	}
	if json.Unmarshal(out.b, &payload) != nil || payload.Records == nil {
		return nil, fmt.Errorf("invalid local inflight")
	}
	var jobs []InflightJob
	for _, r := range payload.Records {
		if r.Pool != pool {
			continue
		}
		started := strings.Trim(string(r.Started), "\"")
		j := InflightJob{r.JobID, started}
		if validJob(j, now) {
			jobs = append(jobs, j)
		}
	}
	return jobs, nil
}
func (b BenchClient) Inflight(ctx context.Context, pool, fp string, local []InflightJob, localKnown bool) InflightResult {
	result := InflightResult{LocalKnown: localKnown, Jobs: []InflightJob{}}
	seen := map[string]bool{}
	for _, j := range local {
		if validJob(j, b.now()) && !seen[j.JobID] {
			seen[j.JobID] = true
			result.Jobs = append(result.Jobs, j)
		}
	}
	if !quotaEnabled() || fp == "" || !b.remote("quota_share") {
		return result
	}
	host, _ := os.Hostname()
	host = safeHost(host)
	prefix := "quota/" + pool + "/" + fp + "/inflight/"
	var payload struct {
		Documents []struct {
			Key  string `json:"key"`
			Body string `json:"body"`
		} `json:"documents"`
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if b.Send(ctx, "GET", "/v1/documents?prefix="+url.QueryEscape(prefix)+"&limit=100", "quota_share", nil, &payload) != nil || payload.Documents == nil || len(payload.Documents) >= 100 {
		return result
	}
	result.RemoteKnown = true
	for _, doc := range payload.Documents {
		var v InflightSnapshot
		if !strings.HasPrefix(doc.Key, prefix) || json.Unmarshal([]byte(doc.Body), &v) != nil || v.Schema != "scopefuel.quota-inflight.v1" || v.Pool != pool || v.AccountFP != fp || v.Host == host {
			continue
		}
		at, e := ParseTime(v.PublishedAt)
		if e != nil || at.After(b.now().Add(time.Minute)) {
			continue
		}
		for _, j := range v.Jobs {
			if validJob(j, b.now()) && !seen[j.JobID] {
				seen[j.JobID] = true
				result.Jobs = append(result.Jobs, j)
			}
		}
	}
	if result.LocalKnown && result.RemoteKnown {
		result.N = ptr(len(result.Jobs))
	}
	return result
}
func (b BenchClient) PublishInflight(ctx context.Context, pool, fp string, jobs []InflightJob) error {
	if !quotaEnabled() || fp == "" || !b.remote("quota_share") {
		return nil
	}
	host, _ := os.Hostname()
	host = safeHost(host)
	v := InflightSnapshot{Schema: "scopefuel.quota-inflight.v1", Pool: pool, AccountFP: fp, Host: host, PublishedAt: b.now().Format(time.RFC3339Nano), Jobs: jobs}
	if v.Jobs == nil {
		v.Jobs = []InflightJob{}
	}
	body, _ := json.Marshal(v)
	key := "quota/" + pool + "/" + fp + "/inflight/" + host
	return b.Send(ctx, "PUT", "/v1/documents/"+url.PathEscape(key), "quota_share", map[string]any{"key": key, "kind": "note", "session": "scopefuel-quota-share", "job": "", "body": string(body)}, nil)
}

// UnknownResetHold is how long a limit observation with no readable reset time
// stays active.
const UnknownResetHold = 60 * time.Minute

type LimitRecord struct {
	ResetAt    string `json:"reset_at"`
	ObservedAt string `json:"observed_at"`
	Rung       string `json:"rung"`
	Source     string `json:"source"`
	Unparsed   bool   `json:"unparsed"`
}

// EffectiveReset is when the record stops holding. An unreadable reset falls
// back to UnknownResetHold after the observation, so a damaged record holds
// instead of admitting. It errors only when neither time is usable.
func (r LimitRecord) EffectiveReset() (time.Time, error) {
	if reset, e := ParseTime(r.ResetAt); e == nil {
		return reset, nil
	}
	observed, e := ParseTime(r.ObservedAt)
	if e != nil {
		return time.Time{}, fmt.Errorf("reset_at and observed_at are both unusable")
	}
	return observed.Add(UnknownResetHold), nil
}

type limitStore struct {
	Schema  string                 `json:"schema"`
	Records map[string]LimitRecord `json:"records"`
}

func limitIdentity(pool string) string {
	entries, _ := LoadSnapshots()
	o := entries[pool].Result
	if o.AccountFP != "" && o.AccountFPKind == FPKindAccount {
		return "account-" + longFingerprint("account\x00"+o.AccountFP)
	}
	host, _ := os.Hostname()
	return "host-user-" + longFingerprint(host+"\x00"+strconv.Itoa(os.Getuid()))
}
func longFingerprint(s string) string {
	b := sha256.Sum256([]byte(s))
	return hex.EncodeToString(b[:16])
}

func limitPath(pool string) string {
	name := "devin-free-limit.json"
	if pool == "commandcode" {
		name = "commandcode-limit.json"
	}
	return filepath.Join(CacheDir(), name)
}
func readLimits(pool string) limitStore {
	var s limitStore
	if e := ReadJSON(limitPath(pool), &s); os.IsNotExist(e) {
		_ = ReadJSON(filepath.Join(filepath.Dir(legacyCachePath()), filepath.Base(limitPath(pool))), &s)
	}
	if s.Records == nil {
		s.Records = map[string]LimitRecord{}
	}
	return s
}
func ObserveLimit(pool string, r LimitRecord, clear bool, accountKey string, now time.Time) error {
	if pool != "devin" && pool != "commandcode" {
		return fmt.Errorf("unsupported limit observation")
	}
	if accountKey == "" {
		accountKey = limitIdentity(pool)
	}
	if !clear {
		observed, e := ParseTime(r.ObservedAt)
		if e != nil || observed.After(now.Add(time.Minute)) {
			return fmt.Errorf("invalid observation time")
		}
		reset, e := ParseTime(r.ResetAt)
		if e != nil || !reset.After(observed) || reset.Sub(observed) > 24*time.Hour {
			return fmt.Errorf("invalid short-window reset")
		}
		if !strings.HasPrefix(r.Source, "job:") || strings.ContainsAny(r.Source, "\r\n\t ") {
			return fmt.Errorf("--source job:<id> required")
		}
	}
	return WithFileLock(limitPath(pool)+".lock", false, func() error {
		s := readLimits(pool)
		s.Schema = "scopefuel.devin-free-limit.v1"
		if pool == "commandcode" {
			s.Schema = "scopefuel.commandcode-limit.v1"
		}
		if clear {
			delete(s.Records, accountKey)
		} else {
			prior, ok := s.Records[accountKey]
			if !ok || prior.ObservedAt <= r.ObservedAt {
				s.Records[accountKey] = r
			}
		}
		return WriteJSON(limitPath(pool), s)
	})
}
func ActiveLimit(pool string, now time.Time) *LimitRecord {
	s := readLimits(pool)
	r, ok := s.Records[limitIdentity(pool)]
	if !ok {
		return nil
	}
	reset, e := ParseTime(r.ResetAt)
	observed, e2 := ParseTime(r.ObservedAt)
	if e != nil || e2 != nil || !reset.After(now) || observed.After(now) {
		return nil
	}
	return &r
}

// ActiveFreeLimit returns the Devin free-limit exceed that still holds at now.
// Unlike ActiveLimit it fails closed: a record whose times cannot be read is
// returned so the gate refuses it, rather than being dropped.
func ActiveFreeLimit(now time.Time) *LimitRecord {
	s := readLimits("devin")
	r, ok := s.Records[limitIdentity("devin")]
	if !ok {
		return nil
	}
	if reset, e := r.EffectiveReset(); e == nil && !reset.After(now) {
		return nil
	}
	return &r
}
