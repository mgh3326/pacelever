# Changelog

## Unreleased

- Remove every trace of the retired `scopefuel` and `wrk` tools without
  compatibility shims: the `SCOPEFUEL_*` environment aliases (including
  `SCOPEFUEL_BIN`-style `<POOL>_BIN` names), the legacy cache, manual and limit
  fallbacks, the read-only SQLite bench reader and its dependency, the capture
  script, and the `scopefuel` herdr metadata names (`pacelever.gauge`,
  `pacelever_quota`). Inflight documents are now `pacelever.quota-inflight.v1`; documents under the old name are not read, so
  in-flight counts reconverge within one gate cycle after hosts upgrade.
  Kept for one release, as the only exceptions: a configuration at the previous
  tool's config path, and a manual store at the old cache path with its old schema
  name, still load.
  The wire identifiers `scopefuel.v1` and `scopefuel.gate.v1` are unchanged.
  `testdata/scopefuel_golden.json` is now `testdata/gate_golden.json`.
- Remove the legacy document publish path (`PublishQuota`, `QuotaFallback`, the
  `scopefuel.quota-share.v1` snapshot and `/v1/documents/quota/<pool>/<fp>/latest`):
  nothing read it. A Fusion lead with `fusion_billing_fp` is now judged only on a
  host whose own measurement is for that account.
- Account keys: for `claude`, `codex` and `devin` the local login decides the key
  and whether the host may measure and publish; `measurable_accounts` applies only
  to pools with no local derivation, and a configured `quota.accounts` entry that
  differs from the login never blocks measurement, presence or publication.
- Presence and in-flight publishes finish before the command exits. A failed one
  prints a `warning:` line on stderr, and `refresh` exits 4 for a refused
  presence publish instead of dropping it.
- Give `fable` a default effort (`high`) so a seat that requires an effort can
  launch it without `--effort`; a test now requires every claude/codex-family
  profile to resolve an effort.

- `gate --account-key`: when the shared reading is stale and this host holds the
  same account and may measure it, measure, publish and judge on the fresh value
  instead of refusing; other hosts still exit 4 with the reason. A free SWE-2
  profile on a named account is judged on the shared `free_bucket`
  (`available` admits, `exhausted` blocks until its reset, `unknown` is exit 4).
- Refresh an expired or refused Grok login in memory with the `refresh_token`
  and OIDC issuer from `auth.json`, and read the optional `/v1/settings` tier
  step; product usage, the on-demand cap share and the prepaid balance become
  their own buckets. The CLI's `auth.json` is never written back.
- Read Devin's organization quota over its read-only web endpoint first when
  `DEVIN_BEARER_TOKEN` and `pools.devin.org_id` are set, mapping daily and weekly
  usage from the flat and nested response shapes; any failure falls back to the
  existing CLI probe, which still supplies the per-model Free tags. The SWE-2
  free rung remains `available (free)` unless a free-limit exceed is recorded.
- Track the manual `anthropic-api` organisation credit as a balance with an
  expiry: `manual set --credit-balance/--credit-total/--expires` shows balance,
  days left and the paced daily budget, flags `WASTE` when that budget exceeds
  the recent hand-entered burn rate so ranking prefers it, admits while the
  balance is positive and unexpired under the pool cutoff and inflight policy
  (default 99, or 90 for preserve), refuses at zero, past the cutoff or past
  expiry, and admits a stale entry with a warning bounded by `credit_max_age_s`
  (default 3 days).
- Shared quota keeps the handoffkeep `config.env` token and CF Access credentials
  when the `credential_env` variable is unset or empty instead of blanking the
  token, but only if `quota.url` is the same origin as the stored
  `HANDOFFKEEP_URL`; a non-empty variable still wins.
- Add `[quota] host_id` for the presence host id; precedence is environment, then
  config, then the hashed hostname.
- Merge Antigravity's per-model quota list into the `agy` observation so a
  metering group the quota summary omits (e.g. this account's gemini session
  window) still reaches the gate; an unmappable model stays under the `unknown`
  group and fails closed.
- Treat the SWE-2 free bucket as unmeasurable by design: `devin-swe2*` rungs are
  admitted as `available (free)` without a fresh Free tag, and refused with
  `free limit exceeded until <reset>` only while a recorded free-limit exceed has
  not reached its reset time. An exceed with an unreadable reset is held for 60
  minutes (`observe devin-free-limit --unparsed`); a stored record with an
  unreadable reset holds the same way and never admits. Account daily/weekly gating of
  non-free Devin rungs is unchanged.
- Read Command Code quota over HTTP (`whoami` then `credits`) and retire its PTY
  probe; refused credentials are `auth_failed` instead of a probe timeout.
- Read Grok billing over HTTP and add Antigravity's cloud quota summary as `agy`.
- Refresh a refused Antigravity token by running `agy models` once and retrying,
  with a read-only macOS Keychain fallback when the token file is missing or
  stays refused.
- Claude keeps a 900-second cache, honours `Retry-After` with a 300-second
  default, and serves its last good value as stale after a rate limit.
- ClinePass accepts `CLINEPASS_API_KEY` and
  `~/.cline/data/settings/providers.json`.
- Publish a separate host-presence record after a successful shared measurement.

## v0.1.1

- Read both legacy text and native structured `last_error` snapshot metadata.
- Ignore retired-provider cache entries before decoding their provider-specific
  shapes, retaining measurements for supported pools.
- Preserve imported last-good measurements across a failed first refresh and
  verify the current account identity before accepting stale values.

## v0.1.0

Initial Go CLI/library, admission and launch parity, eight providers, compatible
configuration and observations, bench/rep/grade workflows, optional downshift
experiments, and PR outcome evidence. Superseded for legacy-cache migration by
v0.1.1.
