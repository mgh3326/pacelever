package pacelever

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type ManualObservation struct {
	Extra              map[string]json.RawMessage `json:"-"`
	ID                 string                     `json:"manual_observation_id"`
	Event              string                     `json:"event"`
	Pool               string                     `json:"pool"`
	Window             string                     `json:"window"`
	UsedPct            *float64                   `json:"used_pct"`
	ResetsAt           string                     `json:"resets_at,omitempty"`
	MeasuredAt         string                     `json:"measured_at"`
	EnteredAt          string                     `json:"entered_at"`
	ExpiresAt          string                     `json:"expires_at"`
	Reason             string                     `json:"reason"`
	Source             string                     `json:"source"`
	SourceVerification string                     `json:"source_verification"`
	SourceLabel        string                     `json:"source_label"`
	SupersedesRef      *string                    `json:"supersedes_ref"`
	Credit             *Credit                    `json:"credit,omitempty"`
}
type Credit struct {
	RemainingUSD float64 `json:"remaining_usd"`
	TotalUSD     float64 `json:"total_usd"`
	ExpiresOn    string  `json:"expires_on"`
}
type ManualStore struct {
	Schema  string              `json:"schema"`
	History []ManualObservation `json:"history"`
	Latest  map[string]string   `json:"latest"`
}

func LoadManual() (ManualStore, error) {
	var s ManualStore
	p := filepath.Join(CacheDir(), "manual.json")
	e := ReadJSON(p, &s)
	if os.IsNotExist(e) {
		e = ReadJSON(filepath.Join(filepath.Dir(legacyCachePath()), "manual.json"), &s)
	}
	if os.IsNotExist(e) {
		return ManualStore{Schema: "scopefuel.manual.v1", History: []ManualObservation{}, Latest: map[string]string{}}, nil
	}
	if e != nil {
		return s, e
	}
	if (s.Schema != "scopefuel.manual.v1" && s.Schema != "pacelever.manual.v1") || s.Latest == nil || s.History == nil {
		return s, fmt.Errorf("invalid manual observation store")
	}
	return s, nil
}
func (s ManualStore) Valid(pool string, now time.Time) []ManualObservation {
	var out []ManualObservation
	for _, m := range s.History {
		if m.Pool != pool || m.Event != "set" || s.Latest[pool+":"+m.Window] != m.ID || !validPct(m.UsedPct) {
			continue
		}
		measured, e := ParseTime(m.MeasuredAt)
		if e != nil || measured.After(now.Add(60*time.Second)) {
			continue
		}
		expiry, e := ParseTime(m.ExpiresAt)
		if e != nil || !expiry.After(now) {
			continue
		}
		if m.Credit != nil {
			expiry, e := time.Parse("2006-01-02", m.Credit.ExpiresOn)
			if e != nil || !expiry.After(now) {
				continue
			}
		}
		if reset, e := ParseTime(m.ResetsAt); e == nil && !reset.After(now) {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Window < out[j].Window })
	return out
}
func prepareManual(m ManualObservation, ttl time.Duration, now time.Time) (ManualObservation, error) {
	if !knownPool(m.Pool) || m.Reason == "" {
		return m, fmt.Errorf("known pool and --reason required")
	}
	if m.Window == "" {
		switch m.Pool {
		case "grok", "devin":
			m.Window = "7d"
		case "kiro":
			m.Window = "30d"
		case "anthropic-api", "commandcode":
			m.Window = "credit"
		default:
			return m, fmt.Errorf("--window required")
		}
	}
	measured, e := ParseTime(m.MeasuredAt)
	if e != nil || measured.After(now.Add(time.Minute)) {
		return m, fmt.Errorf("invalid or future --measured-at")
	}
	if ttl == 0 {
		ttl = 30 * time.Minute
	}
	if ttl < 0 || (m.Window != "credit" && ttl > 2*time.Hour) {
		return m, fmt.Errorf("manual TTL must be positive and at most 2h")
	}
	expires := measured.Add(ttl)
	if m.Window == "credit" {
		if m.Credit == nil || m.Credit.TotalUSD <= 0 || m.Credit.RemainingUSD < 0 || m.Credit.RemainingUSD > m.Credit.TotalUSD {
			return m, fmt.Errorf("credit requires valid --remaining, --total and --expires-on")
		}
		expiry, e := time.Parse("2006-01-02", m.Credit.ExpiresOn)
		if e != nil || !expiry.After(now) {
			return m, fmt.Errorf("credit already expired or invalid expiry")
		}
		m.UsedPct = Pct(100 * (1 - m.Credit.RemainingUSD/m.Credit.TotalUSD))
		expires = expiry
	} else if !validPct(m.UsedPct) {
		return m, fmt.Errorf("--used must be 0..100")
	}
	if m.ResetsAt != "" {
		reset, e := ParseTime(m.ResetsAt)
		if e != nil {
			return m, fmt.Errorf("invalid reset timestamp")
		}
		if reset.Before(expires) {
			expires = reset
		}
	}
	if !expires.After(now) {
		return m, fmt.Errorf("observation already expired")
	}
	m.EnteredAt = now.UTC().Format(time.RFC3339Nano)
	m.ExpiresAt = expires.UTC().Format(time.RFC3339Nano)
	m.Event = "set"
	m.Source = "operator"
	m.SourceVerification = "unverified"
	m.SourceLabel = "self-reported manual observation"
	b := make([]byte, 12)
	if _, e = rand.Read(b); e != nil {
		return m, e
	}
	m.ID = "manual-" + hex.EncodeToString(b)
	return m, nil
}
func AddManual(m ManualObservation, ttl time.Duration, now time.Time) (ManualObservation, error) {
	m, e := prepareManual(m, ttl, now)
	if e != nil {
		return m, e
	}
	e = WithFileLock(filepath.Join(CacheDir(), "manual.lock"), false, func() error {
		s, e := LoadManual()
		if e != nil {
			return e
		}
		if prior := s.Latest[m.Pool+":"+m.Window]; prior != "" {
			m.SupersedesRef = &prior
		}
		s.Latest[m.Pool+":"+m.Window] = m.ID
		s.History = append(s.History, m)
		return WriteJSON(filepath.Join(CacheDir(), "manual.json"), s)
	})
	return m, e
}
func ClearManual(pool string) error {
	return WithFileLock(filepath.Join(CacheDir(), "manual.lock"), false, func() error {
		s, e := LoadManual()
		if e != nil {
			return e
		}
		for k := range s.Latest {
			for _, m := range s.History {
				if m.Pool == pool && k == pool+":"+m.Window {
					delete(s.Latest, k)
				}
			}
		}
		return WriteJSON(filepath.Join(CacheDir(), "manual.json"), s)
	})
}
func RequiredWindows(pool, plan string, manual bool) []string {
	if manual && (pool == "anthropic-api" || pool == "commandcode") {
		return []string{"credit"}
	}
	if pool == "codex" {
		switch normalize(plan) {
		case "prolite":
			return []string{"7d"}
		case "free":
			return []string{"30d"}
		}
	}
	switch pool {
	case "claude", "codex", "kimi":
		return []string{"5h", "7d"}
	case "devin", "grok":
		return []string{"7d"}
	case "kiro":
		return []string{"30d"}
	case "clinepass", "commandcode":
		return []string{"5h", "7d", "30d"}
	case "anthropic-api":
		return []string{"credit"}
	}
	return nil
}

// Preserve legacy audit fields when copying history into the native store.
func (m *ManualObservation) UnmarshalJSON(data []byte) error {
	type plain ManualObservation
	var decoded plain
	if e := json.Unmarshal(data, &decoded); e != nil {
		return e
	}
	*m = ManualObservation(decoded)
	return json.Unmarshal(data, &m.Extra)
}
func (m ManualObservation) MarshalJSON() ([]byte, error) {
	type plain ManualObservation
	b, e := json.Marshal(plain(m))
	if e != nil {
		return nil, e
	}
	var known map[string]json.RawMessage
	if e = json.Unmarshal(b, &known); e != nil {
		return nil, e
	}
	out := map[string]json.RawMessage{}
	for k, v := range m.Extra {
		out[k] = v
	}
	for k, v := range known {
		out[k] = v
	}
	return json.Marshal(out)
}
