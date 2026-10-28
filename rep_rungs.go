package pacelever

import (
	_ "embed"
	"encoding/json"
)

//go:embed rep_aliases.json
var repAliasesJSON []byte

var repAliases = func() map[string][]string {
	var rows map[string][]string
	if e := json.Unmarshal(repAliasesJSON, &rows); e != nil {
		panic(e)
	}
	return rows
}()

// ResolveRepRung preserves an explicitly recorded effort. Only an effort-less
// record may inherit its spelling's pin or the catalog default.
func ResolveRepRung(c Catalog, r Rep) (profile, effort string, inferred bool) {
	profile = c.Canonical(r.Profile)
	pin := ""
	if alias := repAliases[r.Profile]; len(alias) == 2 {
		profile, pin = alias[0], alias[1]
	}
	if r.Effort != "" {
		return profile, r.Effort, false
	}
	if pin != "" {
		return profile, pin, false
	}
	rows := c.profileRows(profile)
	var placed []CatalogRow
	for _, row := range rows {
		if row.Placed || row.Gate == "consult_only" {
			placed = append(placed, row)
		}
	}
	catalogEffort, _ := defaultEffort(profile, placed, c.Defaults)
	return profile, catalogEffort, true
}
