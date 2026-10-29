# Changelog

## Unreleased

- Track the manual `anthropic-api` organisation credit as a balance with an
  expiry: `manual set --credit-balance/--credit-total/--expires` shows balance,
  days left and the paced daily budget, flags `WASTE` when that budget exceeds
  the recent hand-entered burn rate so ranking prefers it, admits while the
  balance is positive and unexpired under the pool cutoff and inflight policy
  (default 99, or 90 for preserve), refuses at zero, past the cutoff or past
  expiry, and admits a stale entry with a warning bounded by `credit_max_age_s`
  (default 3 days).
- Merge Antigravity's per-model quota list into the `agy` observation so a
  metering group the quota summary omits (e.g. this account's gemini session
  window) still reaches the gate; an unmappable model stays under the `unknown`
  group and fails closed.
- Read Command Code quota over HTTP (`whoami` then `credits`) and retire its PTY
  probe; refused credentials are `auth_failed` instead of a probe timeout.
- Read Grok billing over HTTP and add Antigravity's cloud quota summary as `agy`.
- Refresh a refused Antigravity token by running `agy models` once and retrying,
  with a read-only macOS Keychain fallback when the token file is missing or
  stays refused. A legacy scopefuel backoff no longer blocks pools pacelever
  measures itself; imported measurements still serve as stale fallbacks.
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
