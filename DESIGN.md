# Design and migration

## Boundaries

The root Go package evaluates explicit inputs. `ResolveLaunch` chooses the model,
effort, pool, grade and gate. `Gate` checks roles, subscription, policy and quota.
`Recommend` ranks placed rows at the requested grade. These functions do not read
configuration, launch processes, send messages or write state.

The CLI owns configuration, collection, persistence and rendering. HTTP providers
return only measurement fields. The remaining PTY providers (Devin, Kimi and
Kiro) use an isolated work directory, a 50×200 terminal, bounded output and a
process-group deadline. No raw screen, credential or HTTP response body is
retained in an error or quota snapshot.

Claude, Codex, ClinePass, Command Code, Grok and Antigravity use provider HTTP
APIs. A provider `429` carries `Retry-After` into the negative cache; Claude uses
a 300-second default when the header is missing or zero, keeps a 900-second fresh
cache, and serves its last good value as stale rather than scraping a screen.
Command Code reads the saved API key and calls `whoami` then `credits`, so a
refused credential is an immediate `auth_failed` rather than a probe timeout.
Grok reads the CLI billing endpoint, and Antigravity reads the cloud quota
summary (weekly and five-hour buckets) with the `agy` login. The per-model quota
list is merged in as buckets; a model quota keeps a recognized window and falls
back to the session window when it has none, so a metering group the summary
omits is still measured. Antigravity buckets keep their metered group (gemini or
Claude/GPT); a profile is never gated on another group's numbers. A bucket whose
identity maps to no known group stays visible under `unknown` and never gates. Codex windows are labelled by `limit_window_seconds`,
never by primary/secondary position.

## Admission contract

Gate exits remain 0 admitted, 3 quota/policy, 4 unmeasurable, 5 role-denied and
6 unsubscribed; malformed input or output failures use 2. The CLI preserves the
admission first-line format and `scopefuel.gate.v1` audit schema. JSON quota
summaries keep `scopefuel.v1`, including provider status/source and scoped buckets.
Launch JSON keeps the original fields, including catalog provenance, billing,
provider family and the nullable E6 marker. Launch refusals use exit 3. The operator-requested `label` is an additive field on
every successful launch; it identifies actual profile/effort and experiment arm.

Consult seats are checked before quota. Credit rungs require an explicit effort
and seat. Builder admission is limited to a placed default-gate rung. Other
credit rungs remain worker/tester only; a marker or operator reference cannot
turn a consultation/escalation/unplaced rung into a builder placement.

A known automatic cutoff cannot be hidden by a lower manual observation. Manual
fallback requires unexpired evidence covering all required windows. Codex coverage
follows its plan: plus uses 5h+7d, prolite uses 7d, free uses 30d. SWE-2 requires
fresh evidence for the exact model-scoped Free rung and never borrows account
credits or another rung's tag. Observed short-window free limits are advisory.

Pool cutoffs default to 99% for spend and 90% for preserve. Inflight reservation
uses `used + count × reserve` for configured windows. Unknown counts remain
visible and preserve the base verdict. Peer snapshots require an account identity
match and at most 15 minutes of age. Inflight jobs are deduplicated by job ID and
age out after eight hours. Peer values never become local automatic measurements.

Weekly pace is usage divided by elapsed window fraction, unknown during the first
5% or without a valid reset. Every paid pool can trigger alternative promotion at
`settings.altlane_pace_multiple` (default 1.2). Promotion neither invents a grade
placement nor excludes the triggering pool. Proven SWE-2 Free rows precede paid
rows. Area exceptions match current model identity and only affect the requested
area's recommendation view.

## Catalog and evidence

The catalog ladder is server → fresh cache → last-good cache → bundled snapshot.
Cache files are bound to an endpoint digest. Degraded catalog provenance is
visible; a stale view cannot widen a non-default launch gate. A partially seeded
server fills only profiles it does not mention. Retirement and gate changes for
mentioned profiles are authoritative.

The public snapshot contains model and launcher metadata, not operator config,
credentials, quota values, host identities or evidence records. Legacy SQLite is
opened read-only through a pure Go file reader. New bench caches, manual events,
rep intents and observer state are atomic JSON files protected by kernel locks.
The SQLite reader supports rollback-journal files, not WAL files; unsupported or
corrupt evidence is reported as unavailable rather than guessed.

Rep writes save an origin ID before PUT. Conflict and uncertain requests keep the
same intent for explicit retry. Server IDs, origin IDs and historical local IDs
have distinct display namespaces. Pending records do not count as grade evidence.

Grade proposals use exact current model/effort evidence, launcher spelling pins,
latest counted disposition per task, qualifying passes and recent failure rates.
When PR outcome fields are present, they take precedence over tester round counts.
The approved outcome rule requires first CI green, no critical/high/blocking
findings and no seven-day revert for a promotion pass. A revert or critical/high
finding is failure evidence. Initial red CI alone prevents a clean pass; it is not
demotion evidence by itself. Unknown quality fields and unfinished producer
observations cannot count as passes. Merge-time metrics are reported, never used
as a standalone reason to change grade. Proposals include per-rung and experiment
arm summaries. Notes JSON keeps these fields compatible with older bench servers.
Proposals are read-only artifacts. Apply recomputes changes, checks the evidence
fingerprint, verifies the approved subset and effort monotonicity, and writes a
catalog artifact. Degraded evidence requires a recorded reason. Publishing is a
separate `bench push-catalog` operation.

## Scopefuel parity

| Feature | pacelever status |
| --- | --- |
| Gate, stable exit vocabulary, arbiter first line, audit JSON | Implemented; recorded CLI and library contracts |
| Policy launch, model/effort/pool/grade, seat checks | Implemented; newer credit builder-seat rule |
| Policy list/set/clear/profile/setting | Implemented; preserves unrelated config keys |
| Boost, subscribed, cutoff, plan, price, capacity, inflight reserve | Read-compatible pool policy |
| Weekly pace promotion | Implemented for every paid pool; ranking only |
| Nine retained providers | HTTP-first implementations; PTY only for Devin, Kimi and Kiro |
| SWE-2 Free model scope | Exact-rung fresh observation; free first |
| Manual set/list/clear, credit observations | Implemented; legacy JSON read-compatible |
| Bench catalog, push-catalog, bench grades | Implemented against `/v1/bench/*`; dry-run artifacts available |
| Bench score display and legacy evidence | Read-compatible JSON cache and SQLite file reader |
| Reps add/list/compare/pending | Implemented; durable intent, retry and namespaced IDs |
| Refresh, including detached workers | Implemented with independent deadlines and locks |
| Free-limit and Command Code limit observations | Implemented; account-scoped local state |
| Herdr event bridge, brief/watch gauge | Implemented; nested event data, scoped cache, stable metadata token |
| Inflight and quota sharing | Implemented; identity, age and duplicate guards |
| Grades propose/apply; area summaries | Implemented with evidence and monotonicity checks |
| Recommend and area exceptions | Implemented within current grade placements |
| Stale build warning; models verify | Implemented; advisory cached warning and serving-identity comparison |
| Optional downshift exploration | Per-pool deterministic cohort; headroom, placed floor and subscription guards; default off |
| Launch experiment labels | Actual profile, effort, baseline/downshift and nullable experiment ID on every launch |
| PR outcome reps and grade evidence | First-class optional fields; flat/nested notes JSON; legacy text receipts; per-rung/arm metrics |
| Retired providers, provider plugin loaders, one-shot migrations | Deliberately omitted |
| Quota-v2 experimental shadow CLI, historical benchmark ingestion/backfills | Outside this core; no legacy command aliases |

## Parity fixtures

`scripts/capture_goldens.py` executes the installed `scopefuel` command using a
temporary config, data directory and synthetic fresh cache. It disables remote
catalog selection, quota sharing and stale checks. No provider is invoked, no
operator files are changed and no server writes occur. The generated fixture
records 59 gate exits and 56 launch outcomes across profiles, rungs and seats.
The 39 successful reference JSONs are compared structurally after removing only
the new, mandatory `label` field requested for experiments. Every existing field
and its provenance must remain identical; separate assertions verify the label.
Raw JSON intentionally contains that additive extension.

Three credit builder cases deliberately differ from the older installed build.
They now resolve launch JSON successfully and reach ordinary quota evaluation:
Command Code admits on its fixture observation, while the two API-credit cases
remain unmeasurable without a manual balance. All other matrix outcomes match.
Exploration-off keeps every other legacy field and gate verdict unchanged.
These are contract tests over a representative matrix, not a proof that every
historical human-readable sentence or ranking score is identical.

Provider fixtures use recorded, redacted HTTP responses and sanitized usage
panels. HTTP tests use an injected client and a fake server, including the
`Retry-After` and `auth_failed` paths. PTY tests cover the remaining providers
with short-lived helpers only, verifying deadlines, killed-owner lock release
and parent-death cleanup. Backend tests use an injected HTTP client and local
files.

## Migration

1. Keep scopefuel installed and keep all existing callers unchanged. Build or
   download a separately named pacelever binary. Do not install a replacement yet.
2. Let pacelever read legacy config and observations. Confirm warnings name only
   retired/unknown pool sections and check `bench catalog status` provenance.
   New writes go to pacelever; the legacy files remain unchanged.
3. Run shadow comparisons with notifications and quota publishing disabled:

   ```sh
   PACELEVER_NOTIFY=0 PACELEVER_QUOTA_SHARE=0 PACELEVER_STALE_WARN=0 \
     ./dist/pacelever gate -m sonnet --effort high --role worker \
     --gate-output /tmp/pacelever-gate.json
   scopefuel gate -m sonnet --effort high --role worker \
     --gate-output /tmp/scopefuel-gate.json
   ```

   Capture each command's exit independently. Compare codes, pool, scope, selected
   rung, cutoff, subscription, manual provenance and alternatives. Preserve code 5
   separately from quota denial. Compare fresh observations close in time; usage
   can change between the two reads. Expect the documented credit builder change.
4. Witness authenticated provider probes on the intended platform, including a
   Command Code caller with a short timeout. Check fresh and stale/manual cases,
   free model scope, retired/unsubscribed pools and area routing. No production
   catalog push is needed for shadow validation.
5. After operator review, change one caller or let a Go spawner import the package.
   Monitor verdict differences before changing the next caller. Retain scopefuel
   and its original files for rollback. Installation and service changes are
   separate work, not part of building this repository.

## Continuous downshift experiments

[docs/experiments.md](docs/experiments.md) defines the data policy and outcome
contract. The cohort hash uses experiment ID, optional seed, pool and normalized
task ID. It is stable across repeated calls and equivalent task spellings. The
policy never uses randomness from call timing. Eligibility is proved before cohort
assignment: a current paid weekly pace strictly below 1.0 or an exact Free bucket,
a placed ordinary baseline, a one-step placed target at or above the required
task floor, the same pool, a subscribed target and its normal admission/seat checks.
Ineligible work remains a baseline with a null experiment ID. Eligible controls
and downshifts share the configured ID. `RecommendFor` offers the selected arm
while retaining the baseline. `ResolveLaunchWithInputs` records the actual arm.
