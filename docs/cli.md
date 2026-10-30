# CLI reference

Flags may follow subcommands and positional names. JSON is selected with `--json`.
Rep add/list retain their text receipt format by default for existing publishers.
Run `pacelever --help` for a compact command list.

| Command | Purpose |
| --- | --- |
| `gate -m PROFILE [--effort RUNG] [--role SEAT]` | Quota/policy admission; stable exit vocabulary |
| `policy launch PROFILE [--effort RUNG] [--role SEAT]` | Resolve launcher JSON without probing quota |
| `policy list`, `set POOL [CLASS]`, `clear POOL` | Pool control plane |
| `policy profile NAME on\|off\|clear` | Profile subscription override |
| `policy setting altlane_pace_multiple VALUE\|none` | Pace threshold |
| `manual set POOL`, `list [POOL]`, `clear POOL` | Bounded fallback observations; `--credit-balance/--credit-total/--expires` records a credit pool |
| `bench catalog list\|status [--check]` | Canonical catalog and provenance |
| `bench push-catalog FILE [--dry-run]` | Explicit operator catalog PUT |
| `bench push-catalog --emit-seed` | Emit bundled catalog without a write |
| `bench grades list\|set` | Canonical grade projection |
| `bench list\|show\|coverage` | Read benchmark scores |
| `reps add`, `list`, `compare` | Record or inspect run evidence |
| `reps pending list\|retry\|drop` | Inspect, retry or discard a local pending intent |
| `grades propose [--out FILE]`, `apply FILE --out FILE` | Reviewable grade artifacts |
| `grades areas` | Per-area evidence, separated by grade band and builder/tester role |
| `refresh POOL [--background]` | Refresh one automatic pool |
| `observe devin-free-limit` | Passive free-limit observation; advisory |
| `observe commandcode-limit` | Passive usage-limit observation until reset |
| `inflight show --pool POOL` | Read local plus peer inflight observations |
| `herdr-event` | Display-only pane metadata bridge |
| `models list\|verify` | Serving-model baselines and drift checks |
| `--recommend GRADE [--role SEAT] [--area AREA]` | Rank only current grade placements |
| `--brief`, `--json`, `--only POOLS`, `--horizon now\|week\|month`, `--watch SECONDS` | Quota display |

Gate audit references use `--operator-request hk:task/NUMBER` or `hk:doc/KEY`;
`--requested-by NAME` is a self-reported label, not identity proof. The override
only satisfies the relevant consultation/escalation rule. It cannot bypass quota,
subscription or role checks. `--pace-override REF` is an audit field; pace never
refuses admission.

```sh
pacelever reps add --profile sonnet --model claude-sonnet-5-5 --effort high \
  --task hk:task/1 --tier T1 --role impl --grade A --rounds 1 \
  --blockers-found 0 --completed 1
pacelever reps list --profile sonnet --effort high --limit 20
pacelever grades propose --json --out proposal.json
pacelever grades apply proposal.json --only sonnet@high --out catalog.json \
  --decided-by operator --deviation-ref hk:task/1
pacelever bench push-catalog catalog.json --dry-run
```

`grades apply` only writes an artifact. A snapshot/local/stale evidence source
requires `--allow-degraded REASON`; changed evidence or a non-monotonic selected
subset is refused. A real catalog push is a separate explicit action and needs
the server's operator credential.

```sh
pacelever observe devin-free-limit --reset '<RFC3339_RESET>' \
  --observed '<RFC3339_OBSERVATION>' --rung devin-swe2 --source job:example
pacelever observe devin-free-limit --unparsed --observed '<RFC3339_OBSERVATION>' \
  --rung devin-swe2 --source job:example   # reset unreadable: held 60 minutes
pacelever observe devin-free-limit --clear
```

The recorded exceed makes the SWE-2 free rungs refuse with
`free limit exceeded until <reset>` (exit 3) until the reset passes.

`models verify` sends a minimal one-token completion to compare the serving model
with its recorded baseline. It returns 0 for matches, 1 for drift, 2 for unknown.
It is never run implicitly by gate or launch.

Exploration-aware calls add task attribution and a required floor:

```sh
pacelever policy launch opus --effort high --role worker \
  --task hk:task/1 --grade S+ --json
pacelever --recommend S+ --role worker --task hk:task/1 --json
```

Rep outcomes can arrive through `--notes` JSON or optional first-class flags:
`--first-ci-green true|false`, `--findings-by-severity JSON`,
`--reverted-within-7d true|false`, `--time-to-merge-s NUMBER`,
`--experiment-id ID`, and `--experiment-arm baseline|downshift`.
Legacy round/completion flags remain required for records without outcome data.
They may be omitted when real PR outcomes are supplied; quality is never invented
from zero rounds. See [the outcome contract](experiments.md).

<!-- review pane probe 2026-10-30 -->
