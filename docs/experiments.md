# Continuous downshift experiments and PR outcomes

Exploration is opt-in data on each pool. With no configuration, its fraction is
zero and all launches remain baseline. With a policy enabled, a stable hash of
experiment ID, optional seed, pool and task ID selects the requested fraction of
eligible tasks. Retries and equivalent `hk:task/1` / `hk1` spellings keep their
cohort assignment.

```toml
[pools.claude.exploration]
fraction = 0.05
experiment_id = "downshift-cohort-1"
```

Eligibility requires all of the following:

- A task ID and its required grade floor.
- A placed default-gate baseline and a placed default-gate target meeting that
  floor. Carrier grades on unmeasured rungs do not count as placements.
- A current paid weekly pace strictly below 1.0, or a proven Free model bucket.
  Unknown/stale pace and the first 5% of a window remain ineligible.
- The same subscription pool, an enabled target subscription, an allowed seat,
  and the target's ordinary quota admission.

The tool tries the nearest lower known effort on the same model. It never skips
an ineligible intermediate effort to take a second step. It can instead try the
next model in the pool's ordered ladder: Claude opus → sonnet → haiku, Codex sol →
terra61 → luna, or API-credit pi-opus55 → pi-sonnet55 → pi-haiku55. A model missing
its required placement blocks that route; the tool never skips it to take two
model steps. The policy can provide `model_order` as data for other pools. Its
hard eligibility guards still apply.

```sh
pacelever policy launch opus --effort high --role worker \
  --task hk:task/1 --grade S+ --json
pacelever --recommend S+ --role worker --task hk:task/1 --json
```

A successful launch always includes a label:

```json
{
  "profile": "opus",
  "effort": "medium",
  "arm": "downshift",
  "experiment_id": "downshift-cohort-1"
}
```

The label describes the actual resolved profile and effort. An eligible baseline
control carries the same experiment ID and `arm = "baseline"`. Disabled or
ineligible work has a baseline label with `experiment_id = null`. An exploration
audit names the normal pick and explains eligibility/selection. Profiles whose
rung is baked into their spelling retain their existing empty launch effort;
the profile still identifies that rung exactly.

Recommend offers the selected downshift ahead of its baseline and retains the
ordinary alternatives. Launch resolves the actual selected arm. The baseline
library resolver `ResolveLaunch` has no quota inputs and remains baseline;
use `ResolveLaunchWithInputs` and `RecommendFor` for exploration. Spawners should
persist the returned label and attach it to the resulting PR outcome. Rotate an
experiment ID when intentionally changing cohort policy or the comparison design.

## Outcome fields

Reps expose these optional first-class fields:

| Field | Type | Meaning |
| --- | --- | --- |
| `first_ci_green` | nullable boolean | Whether the first CI result was green |
| `findings_by_severity` | nullable map of nonnegative counts | Explicit counts; `{}` means observed zero, `null` means unknown |
| `reverted_within_7d` | nullable boolean | Observed seven-day revert outcome |
| `time_to_merge_s` | nullable nonnegative number | Merge duration in seconds |
| `experiment_id` | optional string | Experiment cohort identifier |
| `experiment_arm` | optional `baseline` or `downshift` | Actual launch arm |

False is distinct from unknown. An empty findings map survives storage. A
`downshift` arm requires an experiment ID. These values can be direct CLI flags,
flat notes JSON, or the existing publisher envelope:

```json
{
  "source": "mergewake",
  "outcome": {
    "outcome_id": "example-outcome-1",
    "profile": "sonnet",
    "model": "claude-sonnet-5-5",
    "effort": "high",
    "decision": "landed",
    "finalized": true,
    "first_ci_green": true,
    "findings_by_severity": {"low": 1},
    "reverted_within_7d": false,
    "time_to_merge_s": 900,
    "experiment_id": "downshift-cohort-1",
    "arm": "downshift"
  }
}
```

`arm` in that envelope is accepted as an alias for `experiment_arm`. Conflicting
explicit/notes values and conflicting profile/model/effort attribution are
refused. Other notes keys remain intact. Older bench servers receive the fields
inside notes JSON through the existing rep wire shape; read/list hydrates the
first-class fields again. No server schema upgrade is required to start recording.

```sh
pacelever reps add --profile sonnet --model claude-sonnet-5-5 --effort high \
  --task hk:task/1 --tier T1 --role impl --grade A \
  --notes '{"first_ci_green":true,"findings_by_severity":{},"reverted_within_7d":false,"time_to_merge_s":900,"experiment_id":"downshift-cohort-1","experiment_arm":"downshift"}'
pacelever reps list --profile sonnet --json
pacelever grades propose --json --out proposal.json
```

Existing text publishers continue to receive `recorded rep id=srv:…` and
`id=srv:… … notes={…}`. Pending/local rows have their own namespaces and never
claim a server receipt. Revised outcomes are new observations with the same task,
profile and effort; the latest counted disposition wins. A later revert therefore
supersedes that task's earlier pass without mutating the old receipt.

## Approved grading rule

When outcome fields are present, grading uses them instead of tester round
counts. A promotion pass requires first CI green, no critical/high/blocking
findings, an observed no-revert result and evidence that the PR merged. Unknown
quality fields, unfinished producer observations and unknown positive severity
labels cannot become passes. Low/medium findings do not by themselves fail this
approved rule. Initial red CI prevents a pass but does not alone trigger demotion.
A critical/high/blocking finding is failure evidence; a seven-day revert is a
post-merge failure and can trigger the existing single-rollback demotion rule.
Publisher-supplied evidence labels are advisory: the grader recomputes from fields.

Legacy reps without outcome fields keep the round/completion rule. Evidence still
matches the exact current model and effort, counts each task once, obeys the
promotion/demotion evidence thresholds, and must leave the effort ladder
monotonic. Proposals contain per-profile@effort outcome summaries and experiment
arm groups: CI-green rate, finding counts, revert rate, and mean/median merge time.
Merge time is an efficiency metric; speed alone never creates a grade placement.
Apply recomputes those metrics and the proposed changes before writing an artifact.

A truncated server rep window is visible and requires an explicit degraded-evidence
reason for apply. Neither a missing quality field nor a missing historic window
is treated as proof of success.
