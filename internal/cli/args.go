package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

type args struct {
	pos    []string
	values map[string][]string
	flags  map[string]bool
}

var boolFlags = words("json raw brief explain hide-excluded no-color no-cache background _worker clear unparsed check emit-seed dry-run list-providers list-recommend-profiles help version allow-plaintext-http")
var valueFlags = words("profile effort role purpose operator-request requested-by pace-override gate-output cache-ttl recommend area only horizon watch exit-code-on used window measured-at reason ttl resets-in resets-at remaining total expires-on credit-balance credit-total expires until boost subscribed fusion-billing-fp inflight-reserve note reset observed rung source account-key pool model task tier grade rounds blockers-found completed input-tokens output-tokens notes limit out decided-by deviation-ref boundary-version min-passes demote-window demote-rate allow-degraded format file non-coding-task pa-reviews first-ci-green findings-by-severity reverted-within-7d time-to-merge-s experiment-id experiment-arm")

func words(s string) map[string]bool {
	m := map[string]bool{}
	for _, v := range strings.Fields(s) {
		m[v] = true
	}
	return m
}
func parse(argv []string) (args, error) {
	a := args{values: map[string][]string{}, flags: map[string]bool{}}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			a.pos = append(a.pos, argv[i+1:]...)
			break
		}
		if arg == "-h" {
			arg = "--help"
		}
		if arg == "-m" {
			arg = "--profile"
		}
		if !strings.HasPrefix(arg, "-") {
			a.pos = append(a.pos, arg)
			continue
		}
		key, value, has := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if boolFlags[key] {
			if has {
				return a, fmt.Errorf("--%s takes no value", key)
			}
			a.flags[key] = true
			continue
		}
		if !valueFlags[key] {
			return a, fmt.Errorf("unknown option --%s", key)
		}
		if !has {
			i++
			if i >= len(argv) {
				return a, fmt.Errorf("--%s requires a value", key)
			}
			value = argv[i]
		}
		a.values[key] = append(a.values[key], value)
	}
	return a, nil
}
func (a args) get(k string) string {
	v := a.values[k]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}
func (a args) at(i int) string {
	if i < len(a.pos) {
		return a.pos[i]
	}
	return ""
}
func (a args) float(k string, def float64) (float64, error) {
	if a.get(k) == "" {
		return def, nil
	}
	v, e := strconv.ParseFloat(a.get(k), 64)
	if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("--%s requires a finite number", k)
	}
	return v, nil
}
func (a args) integer(k string, def int) (int, error) {
	if a.get(k) == "" {
		return def, nil
	}
	v, e := strconv.Atoi(a.get(k))
	if e != nil {
		return 0, fmt.Errorf("--%s requires an integer", k)
	}
	return v, nil
}
func (a args) required(keys ...string) error {
	for _, k := range keys {
		if a.get(k) == "" {
			return fmt.Errorf("--%s required", k)
		}
	}
	return nil
}
