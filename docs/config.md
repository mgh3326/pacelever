# Configuration

pacelever reads `PACELEVER_CONFIG` when set. Otherwise it tries
`$XDG_CONFIG_HOME/pacelever/config.toml`, then the corresponding scopefuel file;
`XDG_CONFIG_HOME` defaults to `~/.config`. It never edits the legacy file.
The first policy edit copies all parsed keys into the pacelever file and changes
only the requested values. Comments and formatting may be rewritten.

Unknown pool sections are ignored with a warning naming the section. A malformed
configuration is reported without echoing values. Retained automatic pools are
`claude`, `codex`, `devin`, `kimi`, `grok`, `kiro`, `clinepass`, `commandcode`
and `agy`. `anthropic-api` is a manual credit pool for `pi-*` profiles.

```toml
[pools.claude]
class = "spend"                 # spend | preserve | exclude
until = "2027-01-01"            # expiry for class and boost
boost = 10
subscribed = true
cutoff = 99.0
capacity_weight = 1.0
price_usd = 0.0
plan = "example-plan"
on_exhaust = "block"            # block | operator-switch

[pools.codex.inflight_reserve_pct]
"5h" = 2.0
"7d" = 1.0

[pools.kimi]
bin = "/path/to/kimi"

[profiles.sonnet]
subscribed = true              # overrides the pool flag in either direction

[settings]
altlane_pace_multiple = 1.2

[bench]
backend = "auto"               # auto | handoffkeep | local
catalog_ttl_s = 3600
catalog_stale_max_s = 86400
cache_ttl_s = 86400
allow_plaintext_catalog = false
allow_plaintext_reps = false
allow_plaintext_quota_share = false
```

Class and boost expire with `until`; subscription flags do not. Cutoff defaults
to 99% for spend or 90% for preserve. The weekly pace threshold never blocks a
launch. `exclude` and `subscribed = false` skip automatic probes. A subscribed
profile override can re-enable that profile on an unsubscribed pool.
`fusion_billing_fp` optionally selects the billing account for a Fusion lead;
its value is not printed by policy list.

Credit pools (`anthropic-api`) accept `credit_max_age_s`, defaulting to 259200
(three days). A hand-entered balance older than that still admits with a
`credit_stale` warning; pacelever never replaces an old balance with a guess.

```toml
[pools.anthropic-api]
credit_max_age_s = 259200
```

```sh
pacelever policy set claude spend --until 2027-01-01 --boost 10
pacelever policy set codex --inflight-reserve 5h=2 --inflight-reserve 7d=1
pacelever policy profile sonnet on
pacelever policy setting altlane_pace_multiple 1.4
pacelever policy clear claude
```

Use `none` to remove boost, subscribed, billing-fingerprint or reserve keys;
`policy profile NAME clear` removes the profile override. Read-only policy
metadata such as plan, price, capacity and cutoff can be edited in TOML.

## Credentials and probe paths

Claude reads the existing credentials under `CLAUDE_CONFIG_DIR` or the default
Claude directory, with a read-only macOS Keychain fallback. Codex reads
`$CODEX_HOME/auth.json` or its default directory. Command Code reads
`COMMAND_CODE_API_KEY` or `COMMANDCODE_API_KEY`, then the API key saved by `cmd`
in `~/.commandcode/auth.json`. Grok uses the login saved by the `grok` CLI in
`$GROK_HOME/auth.json` or its default directory; a login that expired or is
refused is refreshed once in memory with its saved `refresh_token` at the
login's own OIDC issuer, and the CLI's `auth.json` is never written back.
Antigravity uses the login saved
by `agy` in `~/.gemini/antigravity-cli/antigravity-oauth-token`, falling back to
the read-only macOS Keychain entry, and a refused token is refreshed once by
running `agy models` when the `agy` CLI is installed. ClinePass uses
`CLINE_API_KEY` or `CLINEPASS_API_KEY`, then `~/.cline/data/settings/providers.json`,
the existing opencode credential store, or its API-key file. Credentials remain
in memory and request headers; errors never include their values or HTTP bodies.

Devin tries a read-only organization quota endpoint first when
`DEVIN_BEARER_TOKEN` and `pools.devin.org_id` are both set: it requests
`app.devin.ai/api/<org_id>/billing/quota/usage` with a Bearer token and
`x-cog-org-id`, and maps the daily and weekly usage from either the flat or
nested response shape. Any missing credential, malformed organization id or
unrecognized answer falls through to the existing CLI probe, which still
supplies the per-model Free tags. No browser storage is read and no credential
is written back.

```toml
[pools.devin]
org_id = "org-example"          # with DEVIN_BEARER_TOKEN in the environment
```

Kimi and Kiro still read the existing authenticated CLIs. Set
`PACELEVER_<POOL>_BIN` to an explicit executable; the corresponding
`SCOPEFUEL_<POOL>_BIN` remains supported. Kimi respects its existing location
selector. The tool does not install CLIs or initiate login flows.

A provider `429` honours `Retry-After` (seconds or an HTTP date). Claude uses a
300-second wait when the header is missing or zero and keeps a 900-second fresh
cache; a refused refresh serves the last good value as stale. When the Claude
usage API fails, pacelever makes one bounded `claude -p "/usage" --allowed-tools ""`
attempt in an empty directory and uses that reading only when its session and
weekly quotas parse cleanly.

Catalog credentials come from `HANDOFFKEEP_URL` plus `HANDOFFKEEP_TOKEN`, or the
existing handoffkeep `config.env` file. `HANDOFFKEEP_CONFIG` can select that file.
An environment override is an entire credential context: a half-specified pair
never borrows the stored token or stored CF Access credentials. Optional CF Access
credentials use the `HANDOFFKEEP_CF_ACCESS_CLIENT_ID` and
`HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET` pair. Credentials are never CLI arguments.

HTTPS is required by default. The three per-use plaintext opt-ins are independent.
The legacy `allow_plaintext_url` alias remains readable. `--allow-plaintext-http`
is a one-command opt-in for bench/reps operations. Redirects never receive a
forwarded bearer token.

## State and manual evidence

New state lives under `$XDG_CACHE_HOME/pacelever` (default `~/.cache/pacelever`).
`PACELEVER_CACHE` selects a snapshot file. Existing `SCOPEFUEL_CACHE` or legacy
snapshot/manual/limit files are read as a fallback. Native snapshot entries win.
Historical bench data is read from `$XDG_DATA_HOME/scopefuel/bench.db`; the default
is `~/.local/share/scopefuel/bench.db`. `SCOPEFUEL_BENCH_DB` can select a read-only
copy. No database server, SQL service or CGO driver is required.

New files use mode 0600, atomic rename and stable kernel-lock inodes. A killed
owner releases its lock. Provider errors retain the last successful snapshot and
apply a bounded retry backoff. A fresh snapshot suppresses a new probe; Command
Code collection queues a bounded independent worker when refresh is needed.

```sh
pacelever manual set claude --window 5h --used 20 \
  --measured-at '<RFC3339_TIME>' --ttl 30m --reason 'operator observation'
pacelever manual set claude --window 7d --used 30 \
  --measured-at '<RFC3339_TIME>' --ttl 30m --reason 'operator observation'
pacelever manual set --pool anthropic-api --credit-balance 150 --credit-total 200 \
  --expires '2026-11-16T00:00:00Z' --reason 'console balance'
pacelever manual set anthropic-api --window credit --remaining 75 --total 100 \
  --expires-on 2027-01-01 --measured-at '<RFC3339_TIME>' --reason 'balance observed'
pacelever manual list
```

Non-credit observations default to 30 minutes and are limited to two hours.
Credit observations last until their recorded expiry. Reset timestamps cap the
observation's effective life. Required-window coverage must be complete for a
manual fallback. It is visibly self-reported and unverified; it cannot hide a
confirmed automatic cutoff.

### SWE-2 free bucket

The SWE-2 free bucket (`devin-swe2`, `devin-swe2-medium`, `devin-swe2-max`)
cannot be measured, so the gate never asks for a fresh measurement of it. A rung
is admitted as `available (free)` unless a free-limit exceed is on record. The
exceed is stored by `pacelever observe devin-free-limit` with its reset time and
covers all three rungs. Until that reset passes, the gate refuses with exit 3 and
`free limit exceeded until <reset>`; afterwards the rung is admitted again. The
same wording appears in the text output, `--json` and `--gate-output`.

An observation whose reset time cannot be read (`--unparsed` with no `--reset`)
is held for a conservative 60 minutes from its observed time (now, when
`--observed` is omitted), and the hold is logged to stderr. A malformed
`--observed` or `--reset` is refused; an empty reset is never stored. The gate
also fails closed on a stored record whose `reset_at` is empty or unreadable: it
holds for 60 minutes after `observed_at`, and refuses with
`free limit record unreadable` when `observed_at` is unusable too (clear it with
`observe devin-free-limit --clear`). Account-level daily and weekly measurements are untouched:
they still gate every non-free Devin rung, and are only echoed on an admitted
free rung.

A credit entry records the operator's Console balance: `--credit-balance` and
`--credit-total` (aliases `--remaining` and `--total`) plus `--expires` (alias
`--expires-on`), which accepts RFC3339 or `YYYY-MM-DD`. The credit form defaults
`--measured-at` to now and `--reason` to `credit balance observed`. Status shows
the balance, expiry, days left and the paced daily budget (`balance ÷ days
left`), and flags `WASTE` when that budget is above the burn rate derived from
the two most recent hand-entered balances — credit that would otherwise be left
unused at expiry, so ranking prefers it. The gate applies the same pool policy as
any other pool to the balance-derived used percentage: an operator `cutoff`
(default 99, or 90 for `preserve`) and any `inflight_reserve_pct` headroom.
It admits while the balance is positive and unexpired and below that cutoff;
`balance ≤ 0`, a balance at or past the cutoff, or an expired entry refuses
admission. A stale entry still admits, with a warning, so the operator can
update it by hand.

`PACELEVER_QUOTA_SHARE=0` disables peer snapshot exchange. `PACELEVER_NOTIFY=0`
disables exhaustion notices. `PACELEVER_STALE_WARN=0` disables stale-build checks.
The legacy quota-share and stale-warning switches remain supported. Use these
controls while comparing shadow verdicts. `operator-switch` is an explicit
notification policy; it changes neither the quota verdict nor the login account.

## Exploration policy

Exploration is optional data under each pool. `fraction = 0` is off by default;
`enabled = false` can suspend an existing cohort. An active policy requires an ID.

```toml
[pools.claude.exploration]
fraction = 0.05
experiment_id = "downshift-cohort-1"
# enabled = false
# seed = "optional-stable-seed"
# model_order = ["opus", "sonnet", "haiku"]
```

The default ladders are opus → sonnet → haiku, codex-sol → codex-terra61 →
codex-luna, and pi-opus55 → pi-sonnet55 → pi-haiku55. Devin's baked SWE-2
profiles can step max → high → medium. A lower effort on the same model is tried
first. A configured model order can describe other pool-specific ladders, but
same-pool, subscription, default-gate, placed-grade and normal admission checks
remain mandatory. An unplaced/retired intermediate model is never skipped.

Supply `--task KEY --grade FLOOR` to `policy launch`, or `--task KEY` alongside
`--recommend GRADE`. Missing attribution, unknown/stale weekly pace, pace ≥1.0,
or a missing sufficient placement keeps the baseline. Paid credit pools need an
actual weekly observation to prove pace; a credit balance alone is insufficient.
The policy does not infer extra headroom from an unknown reset.

See [experiments](experiments.md) for labels, PR outcome fields and grading.
