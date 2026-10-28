package pacelever

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ansi = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\\([0-2])")

func CleanScreen(s string) string { return strings.ReplaceAll(ansi.ReplaceAllString(s, ""), "\r", "") }

var percent = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*%\s*(left|used|remaining)?`)
var resetsIn = regexp.MustCompile(`(?i)resets?\s+in\s+((?:[0-9]+(?:\.[0-9]+)?\s*(?:days?|hours?|minutes?|seconds?|weeks?|[dhmsw])\s*)+)`)

func number(s string) float64 {
	v, _ := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	return v
}
func humanDuration(s string) (time.Duration, error) {
	s = strings.ToLower(s)
	for _, p := range []struct{ a, b string }{{"weeks", "w"}, {"week", "w"}, {"days", "d"}, {"day", "d"}, {"hours", "h"}, {"hour", "h"}, {"minutes", "m"}, {"minute", "m"}, {"seconds", "s"}, {"second", "s"}} {
		s = strings.ReplaceAll(s, p.a, p.b)
	}
	return Duration(strings.Join(strings.Fields(s), ""))
}
func resetIn(s string, now time.Time) string {
	m := resetsIn.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	d, e := humanDuration(m[1])
	if e != nil || d <= 0 {
		return ""
	}
	return now.Add(d).UTC().Format(time.RFC3339Nano)
}
func screenBucket(window string, used float64, reset string) Bucket {
	return Bucket{Label: window, Window: window, UsedPct: Pct(used), ResetsAt: reset, Scope: Scope{Kind: "account"}, Horizon: horizon(window)}
}
func ParseScreen(pool, text string, now time.Time) Observation {
	o := Observation{ID: pool, Source: "cli:/usage"}
	clean := CleanScreen(text)
	fail := func() Observation {
		o.Error = "usage panel incomplete or unrecognized"
		o.ErrorKind = "parse_error"
		o.Buckets = nil
		return o
	}
	if strings.Contains(strings.ToLower(clean), "trust this folder") {
		return fail()
	}
	switch pool {
	case "commandcode":
		return parseCommandcode(clean, now)
	case "kiro":
		re := regexp.MustCompile(`(?i)Credits\s*\(\s*([\d.,]+)\s+of\s+([\d.,]+)`)
		m := re.FindStringSubmatch(clean)
		if m == nil {
			return fail()
		}
		used, total := number(m[1]), number(m[2])
		bonus := regexp.MustCompile(`(?i)Bonus credits:\s*([\d.,]+)\s*/\s*([\d.,]+)`).FindStringSubmatch(clean)
		if bonus != nil {
			used += number(bonus[1])
			total += number(bonus[2])
		}
		if total <= 0 || used > total {
			return fail()
		}
		reset := ""
		r := regexp.MustCompile(`(?i)resets on (\d{4}-\d{2}-\d{2})`).FindStringSubmatch(clean)
		if r != nil {
			t, e := time.Parse("2006-01-02", r[1])
			if e == nil {
				reset = t.UTC().Format(time.RFC3339)
			}
		}
		o.Buckets = []Bucket{screenBucket("30d", 100*used/total, reset)}
	case "grok":
		m := regexp.MustCompile(`(?i)Weekly limit[^\n%]*?([0-9]+(?:\.[0-9]+)?)\s*%`).FindStringSubmatch(clean)
		if m == nil {
			return fail()
		}
		reset := resetIn(clean, now)
		if reset == "" {
			m := regexp.MustCompile(`(?i)Next reset[^\n]*?([A-Za-z]+)\s+(\d{1,2}),?\s+(\d{1,2}:\d{2})`).FindStringSubmatch(clean)
			if m == nil {
				return fail()
			}
			t, e := time.ParseInLocation("2006 January 2 15:04", fmt.Sprintf("%d %s %s %s", now.In(time.Local).Year(), m[1], m[2], m[3]), time.Local)
			if e != nil {
				return fail()
			}
			if !t.After(now) {
				t = t.AddDate(1, 0, 0)
			}
			reset = t.UTC().Format(time.RFC3339)
		}
		o.Buckets = []Bucket{screenBucket("7d", number(m[1]), reset)}
	case "kimi", "devin":
		for _, line := range strings.Split(clean, "\n") {
			lower := strings.ToLower(line)
			window := ""
			switch {
			case strings.Contains(lower, "weekly") || strings.Contains(lower, "7-day"):
				window = "7d"
			case strings.Contains(lower, "5h") || strings.Contains(lower, "5-hour"):
				window = "5h"
			case strings.Contains(lower, "daily"):
				window = "1d"
			case strings.Contains(lower, "monthly"):
				window = "30d"
			}
			if window == "" {
				continue
			}
			m := percent.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if pool == "kimi" && m[2] == "" {
				return fail()
			}
			used := number(m[1])
			if strings.EqualFold(m[2], "left") || strings.EqualFold(m[2], "remaining") {
				used = 100 - used
			}
			o.Buckets = append(o.Buckets, screenBucket(window, used, resetIn(line, now)))
		}
		if pool == "devin" && len(o.Buckets) == 0 {
			m := regexp.MustCompile(`(?i)(?:Pro|Ultra|Free)[^\n]*?([0-9]+(?:\.[0-9]+)?)%\s+remaining`).FindStringSubmatch(clean)
			if m != nil {
				o.Buckets = []Bucket{screenBucket("7d", 100-number(m[1]), resetIn(clean, now))}
			}
		}
	}
	if len(o.Buckets) == 0 {
		return fail()
	}
	for _, b := range o.Buckets {
		if !validPct(b.UsedPct) {
			return fail()
		}
	}
	return o
}
func parseCommandcode(clean string, now time.Time) Observation {
	o := Observation{ID: "commandcode", Source: "cli:/usage", PoolClass: "spend"}
	fail := func() Observation {
		return Observation{ID: "commandcode", Source: "cli:/usage", Error: "cmd usage panel missing a complete monthly, 5-hour or weekly row", ErrorKind: "parse_error"}
	}
	five := regexp.MustCompile(`(?i)\b5[- ]?hour\b`).FindStringIndex(clean)
	week := regexp.MustCompile(`(?i)\bweekly\b`).FindStringIndex(clean)
	renew := regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*days?\s+to\s+renew`).FindStringSubmatchIndex(clean)
	if five == nil || week == nil || renew == nil {
		return fail()
	}
	// Each percentage belongs to its own row; a missing value must not borrow
	// the next row's number or the previous row's reset duration.
	monthly := clean[:renew[0]]
	for _, m := range resetsIn.FindAllStringIndex(monthly, -1) {
		monthly = monthly[m[1]:]
		break
	}
	vals := percent.FindAllStringSubmatch(monthly, -1)
	if len(vals) == 0 {
		return fail()
	}
	monthlyUsed := number(vals[len(vals)-1][1])
	days := number(clean[renew[2]:renew[3]])
	if days <= 0 {
		return fail()
	}
	o.Buckets = append(o.Buckets, screenBucket("30d", monthlyUsed, now.Add(time.Duration(days*24)*time.Hour).UTC().Format(time.RFC3339Nano)))
	for _, row := range []struct {
		pos    []int
		other  []int
		window string
	}{{five, week, "5h"}, {week, five, "7d"}} {
		end := len(clean)
		if row.other[0] > row.pos[0] {
			end = row.other[0]
		}
		if renew[0] > row.pos[0] && renew[0] < end {
			end = renew[0]
		}
		segment := clean[row.pos[1]:end]
		m := percent.FindStringSubmatch(segment)
		reset := resetIn(segment, now)
		if m == nil || reset == "" {
			return fail()
		}
		used := number(m[1])
		if strings.EqualFold(m[2], "left") || strings.EqualFold(m[2], "remaining") {
			used = 100 - used
		}
		b := screenBucket(row.window, used, reset)
		if regexp.MustCompile(`(?i)(?:usage|rate|request) limit (?:reached|exceeded)`).MatchString(segment) {
			b.UsedPct = Pct(100)
			b.Locked = true
		}
		o.Buckets = append(o.Buckets, b)
	}
	for _, b := range o.Buckets {
		if !validPct(b.UsedPct) {
			return fail()
		}
	}
	return o
}
func ParseDevinModels(text string) []Bucket {
	var out []Bucket
	rowID := regexp.MustCompile(`^\s*(swe-2-high|swe-2-medium|swe-2-max)\s+`)
	for _, line := range strings.Split(CleanScreen(text), "\n") {
		m := rowID.FindStringSubmatch(line)
		if m == nil || !regexp.MustCompile(`(?i)\bFree\b`).MatchString(line) {
			continue
		}
		out = append(out, Bucket{Label: m[1], Window: "", UsedPct: Pct(0), Scope: Scope{Kind: "model", Name: m[1]}, Horizon: "now", Note: "Free tag observed"})
	}
	return out
}
