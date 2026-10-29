# pacelever

**Measure AI quota, choose model profiles, and admit work at a sustainable pace.**

pacelever is a Go CLI and library for quota-aware AI work. It replaces scopefuel's
core while keeping the command contract that spawners and quota collectors use.
It supports Claude, Codex, Devin, Kimi, Grok, Kiro, ClinePass, Command Code and
Antigravity, plus manual observations for credit balances and unavailable probes.

```sh
CGO_ENABLED=0 go build -o dist/pacelever ./cmd/pacelever
./dist/pacelever --brief
./dist/pacelever policy launch sonnet --effort high --role worker --json
./dist/pacelever gate -m sonnet --effort high --role worker
./dist/pacelever --recommend A --role builder --json
```

| Gate exit | Meaning |
| --- | --- |
| 0 | Admitted |
| 2 | Invalid input or an I/O failure |
| 3 | Quota or policy denial |
| 4 | Quota cannot be measured |
| 5 | Role denial; spawners may map this to 77 |
| 6 | Subscription disabled |

An admitted gate writes a `profile=… pool=… used_pct=… class=…` first line to
stdout. Refusals go to stderr. `--gate-output FILE` writes the compatible
`scopefuel.gate.v1` audit record. `policy launch` emits the compatible JSON
shape and returns 3 when resolution is refused.

Pace affects ordering and warnings, never admission eligibility. At a configurable
weekly pace multiple of 1.2, alternatives from other pools move ahead of the
triggering paid pool, within the requested grade's placements. An available SWE-2
Free rung comes first. The SWE-2 free bucket cannot be measured, so it is
`available (free)` unless a free-limit exceed is on record. Account credit
exhaustion does not block it.

Credit profiles require an explicit effort and seat. A placed, default-gate
`pi-*` or `cmd-dsflash41` rung permits builder, worker and tester seats. Other
credit rungs permit worker/tester seats under their existing gate restrictions.
An omitted seat is refused.

Optional per-pool exploration offers a deterministic one-step downshift only
under headroom and a sufficient placed grade. It defaults off. Every launch
includes a `label` with profile, effort, `baseline|downshift`, and a nullable
experiment ID. [Exploration and PR outcomes](docs/experiments.md) describes the
policy, labels and outcome evidence.

Existing `~/.config/scopefuel/config.toml`, JSON observations and read-only
SQLite bench data are recognized automatically. New writes belong to pacelever.
Unknown pool sections produce a warning. See [configuration](docs/config.md),
[design and migration](DESIGN.md), and [the CLI reference](docs/cli.md).

The root package is importable without subprocesses:

```go
catalog := pacelever.BundledCatalog()
launch, err := pacelever.ResolveLaunch(catalog, config, pacelever.Request{
    Profile: "sonnet", Effort: "high", Role: "worker",
})
verdict := pacelever.Gate(pacelever.Inputs{
    Catalog: catalog, Config: config, Observations: observations, Now: now,
}, pacelever.Request{Profile: "sonnet", Effort: "high", Role: "worker"})
rows := pacelever.Recommend(inputs, "A", "builder", "")
```

`Gate`, `ResolveLaunch` and `Recommend` do no I/O. `Collector`, provider interfaces,
`BenchClient` and the observation stores supply the runtime boundary.
Use `ResolveLaunchWithInputs(inputs, request)` and `RecommendFor` when enabling
exploration; the baseline resolver has no observations to prove headroom. Callers own
the observation freshness and cancellation context.

Release tags publish standalone binaries for Linux and macOS, each on amd64 and
arm64, plus `SHA256SUMS`. CI runs gofmt, vet, staticcheck, race tests, four cross-builds and macOS
probe tests. Development uses focused tests; `scripts/check.sh` is the final
verification command. No external database server is needed.

The compatibility suite records 59 installed-scopefuel gate cases and 56 launch
cases, including 39 successful reference launch JSONs. Their existing fields remain
identical; the new mandatory label is checked separately. Three credit builder-seat cases
intentionally implement the newer seat rule. Regenerate only with the read-only
fixture procedure in [DESIGN.md](DESIGN.md).

`.mergewake/policy.yaml` uses shadow mode (`enabled: false`). Landing, recovery,
installation and service management are separate operator actions.
