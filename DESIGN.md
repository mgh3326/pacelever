# Design and migration

## Boundaries

The root Go package evaluates explicit inputs. `ResolveLaunch` chooses the model,
effort, pool, grade and gate. `Gate` checks roles, subscription, policy and quota.
`Recommend` ranks placed rows at the requested grade. These functions do not read
configuration, launch processes, send messages or write state.

The CLI owns configuration, collection, persistence and rendering. HTTP providers
return only measurement fields. PTY providers use an isolated work directory,
a 50×200 terminal, bounded output and a process-group deadline. No raw screen,
credential or HTTP response body is retained in an error or quota snapshot.

Command Code resolves an explicit environment/config path before PATH and known
installation directories. Ordinary collection queues an independent refresh
worker with its own budget. Kernel locks and a bounded live-worker record prevent
concurrent probes and short-lived callers from starving collection. A watchdog
pipe closes on parent death and reaps the probe group. `/usage` and Enter are
separate writes, with an input-settle interval between them. A screen that only
shows typed `/usage` is not a measurement.

## Admission contract

Gate exits remain 0 admitted, 3 quota/policy, 4 unmeasurable, 5 role-denied and
6 unsubscribed; malformed input or output failures use 2. The CLI preserves the
admission first-line format and `scopefuel.gate.v1` audit schema. JSON quota
summaries keep `scopefuel.v1`, including provider status/source and scoped buckets.
Launch JSON keeps the original fields, including catalog provenance, billing,
provider family and the nullable E6 marker. Launch refusals use exit 3.

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
latest counted disposition per task, clean passes and recent failure rates.
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
| Eight retained providers | HTTP/PTY implementations, fake-provider and screen tests |
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
| Retired providers, provider plugin loaders, one-shot migrations | Deliberately omitted |
| Quota-v2 experimental shadow CLI, historical benchmark ingestion/backfills | Outside this core; no legacy command aliases |

## Parity fixtures

`scripts/capture_goldens.py` executes the installed `scopefuel` command using a
temporary config, data directory and synthetic fresh cache. It disables remote
catalog selection, quota sharing and stale checks. No provider is invoked, no
operator files are changed and no server writes occur. The generated fixture
records 59 gate exits and 56 launch outcomes across profiles, rungs and seats.
The 39 successful reference JSONs are compared structurally without deleting
fields or normalizing provenance.

Three credit builder cases deliberately differ from the older installed build.
They now resolve launch JSON successfully and reach ordinary quota evaluation:
Command Code admits on its fixture observation, while the two API-credit cases
remain unmeasurable without a manual balance. All other matrix outcomes match.
These are contract tests over a representative matrix, not a proof that every
historical human-readable sentence or ranking score is identical.

Provider fixtures include the recorded unsubmitted Command Code screen, with its
probe directory generalized, plus sanitized usage panels. PTY tests use short-lived
helpers only and verify separate Enter, deadlines, killed-owner lock release and
parent-death cleanup. Backend tests use an injected HTTP client and local files.

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
