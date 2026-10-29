package pacelever

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Value helpers shared by every provider. Each loose JSON value goes through
// one of these so a field means the same thing whichever source reports it.

// ParseFinite parses a decimal number and refuses NaN and ±Inf. It does not
// trim; the caller decides whether surrounding space is allowed.
func ParseFinite(s string) (float64, bool) {
	n, e := strconv.ParseFloat(s, 64)
	if e != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
}

// finiteNumber accepts a decoded JSON number only.
func finiteNumber(value any) (float64, bool) {
	var n float64
	switch v := value.(type) {
	case float64:
		n = v
	case json.Number:
		var e error
		n, e = v.Float64()
		if e != nil {
			return 0, false
		}
	case int64:
		n = float64(v)
	case int:
		n = float64(v)
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}

// looseNumber accepts a JSON number or a numeric string, as several
// providers send money and percentage fields either way.
func looseNumber(v any) (float64, bool) {
	if n, ok := finiteNumber(v); ok {
		return n, true
	}
	if text, ok := v.(string); ok {
		return ParseFinite(strings.TrimSpace(text))
	}
	return 0, false
}

func numericValue(v any) float64 {
	n, _ := finiteNumber(v)
	return n
}

func creditAmount(v any) float64 {
	n, ok := finiteNumber(v)
	if !ok || n < 0 {
		return 0
	}
	return n
}

// headerFloat parses a numeric response header; absent or unparsable is nil,
// never a guessed value.
func headerFloat(header http.Header, name string) *float64 {
	if header == nil {
		return nil
	}
	if n, ok := ParseFinite(strings.TrimSpace(header.Get(name))); ok {
		return &n
	}
	return nil
}

// anyFloat is looseNumber as a pointer: nil when the value is not a finite
// number (the codex credits balance has been observed as "0").
func anyFloat(value any) *float64 {
	if n, ok := looseNumber(value); ok {
		return &n
	}
	return nil
}

// resetAtValue reads a reset time sent as epoch seconds, milliseconds or
// microseconds (number or string), RFC 3339 or an HTTP date.
func resetAtValue(v any) string {
	if n, ok := finiteNumber(v); ok {
		return epochTime(n)
	}
	switch x := v.(type) {
	case string:
		text := strings.TrimSpace(x)
		if text == "" {
			return ""
		}
		if at, e := time.Parse(time.RFC3339Nano, text); e == nil {
			return at.UTC().Format(time.RFC3339Nano)
		}
		if n, ok := ParseFinite(text); ok {
			return epochTime(n)
		}
		if at, e := http.ParseTime(text); e == nil {
			return at.UTC().Format(time.RFC3339Nano)
		}
	}
	return ""
}

func epochTime(n float64) string {
	if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 || n > 9.2e18 {
		return ""
	}
	var at time.Time
	switch {
	case n >= 1e14:
		at = time.UnixMicro(int64(n))
	case n >= 1e11:
		at = time.UnixMilli(int64(n))
	default:
		at = time.Unix(int64(n), 0)
	}
	at = at.UTC()
	// A quota reset outside this range is a malformed unit, not a real time.
	if at.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || at.After(time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)) {
		return ""
	}
	return at.Format(time.RFC3339Nano)
}

func validated(p *float64) *float64 {
	if !validPct(p) {
		return nil
	}
	return p
}

func object(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func list(v any) []any {
	if items, ok := v.([]any); ok {
		return items
	}
	return nil
}

func readObject(path string) map[string]any {
	var data map[string]any
	_ = ReadJSON(path, &data)
	return data
}
