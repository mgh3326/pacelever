package cli

import (
	"context"
	"fmt"

	pl "github.com/mgh3326/pacelever"
)

func (a *App) warnStale(ctx context.Context) {
	if warning := pl.StaleBuildWarning(ctx, a.Commit, nil); warning != "" {
		fmt.Fprintln(a.Err, "warning:", warning)
	}
}
func (a *App) models(ctx context.Context, v args) (int, error) {
	switch v.at(1) {
	case "list":
		return 0, a.json(pl.ServedModels())
	case "verify":
		rows, code := pl.VerifyModels(ctx, pl.ServedModels(), pl.ProbeServedModel)
		return code, a.json(rows)
	}
	return pl.Invalid, fmt.Errorf("models list or verify required")
}
