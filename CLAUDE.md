# pacelever conventions

Read README.md and DESIGN.md first. The gate exit codes, `scopefuel.v1` /
`scopefuel.gate.v1` JSON schema ids and the golden fixtures in `testdata/` are a
compatibility contract: a refactor must leave them unchanged.

## Layout

- `gate.go`, `exploration.go`, `catalog.go`, `grades.go`: pure decisions over
  explicit `Inputs`. No I/O, no wall clock.
- `providers.go`: shared HTTP plumbing (`HTTPProvider`, `statusObservation`,
  `doJSON`, `fingerprint`) and `DefaultProvidersAt`, which wires every provider.
- One file per provider: `claude.go`, `claude_usage.go`, `codex.go`,
  `codex_rpc.go`, `clinepass.go`, `commandcode.go`, `grok.go`, `devin_web.go`,
  `devin_account.go`, `agy.go`, `agy_local.go`; PTY probes in `probe.go` and
  `screens.go`. A new provider gets its own file and is registered in
  `DefaultProvidersAt`.
- `values.go`: decoding of loose JSON values. `kinds.go`: the `ErrorKind`,
  `Scope.Kind`, `Source`, fingerprint-kind and bench-source vocabulary.
  `account.go`: provider account identity.
- `internal/cli`: flags, config, collection, persistence and rendering.

## Rules

- **Time.** Never call `time.Now()` for a measurement or timestamp. Take a
  `func() time.Time` (a `Now` field or parameter) and read it with
  `ClockNow(now)`; pure functions take `Inputs.Now`. The CLI reads `a.now()`.
  Code with no clock to inject says so with `ClockNow(nil)`. `time.Now` and
  `time.Since` are fine only for elapsed-time pacing inside a probe.
- **Ordering.** Never let map iteration order reach output, an error message or
  a "first match" decision; iterate `slices.Sorted(maps.Keys(m))`.
- **Numbers.** Decode provider values with `finiteNumber` (JSON number),
  `looseNumber` (number or numeric string), `ParseFinite` (a string),
  `headerFloat`, `anyFloat` and `resetAtValue`. They all refuse NaN and ±Inf.
  Do not add a provider-specific copy.
- **Kinds.** Use the `ErrKind*`, `Scope*`, `Source*`, `FPKindAccount` and
  `Bench*` constants, never a literal. A new value is added to `kinds.go` with
  a one-line meaning.
- **Failures.** Build a failed observation with `failed(id, kind, message)` or
  `o.fail(kind, message)`; reuse the `msg*` constants for shared sentences.
- **Identity.** Name an account with `accountFP(pool, value)` and attribute an
  observation with `o.publishAccount(fp)`. Resolve logins through
  `account.go` (`codexHome`, `codexTokens`, `claudeAccountFP`), so the key a
  provider publishes is the key the shared store and gate expect.
- **Environment.** Read options with `FirstEnv(pacelever name, alias...)`; it trims and skips blank values.
- **Exit codes.** The CLI returns `pl.Admitted`/`0`, `pl.Invalid`,
  `pl.Blocked`, `pl.Unmeasurable`, `pl.RoleDenied`, `pl.Unsubscribed` or
  `exitAlert`, never a bare non-zero number.
- **Buckets.** Build with `accountBucket`, `modelBucket` or `windowBucket`, so
  the label, scope and horizon follow one rule. Use a literal only when the
  label differs from the window or the bucket has no horizon (credit buckets).
- **Safety.** Providers return allowlisted measurement fields only. Response
  bodies, URLs and credentials never enter an `Observation` or an error.

## Checks

Use focused `go test -run` while developing. Before pushing, run
`scripts/check.sh` (gofmt, vet, staticcheck, race tests, cross-builds). CI
runs the same gofmt, vet and staticcheck steps.
