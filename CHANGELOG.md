# Changelog

## Unreleased

- Read Command Code quota over HTTP (`whoami` then `credits`) and retire its PTY
  probe; refused credentials are `auth_failed` instead of a probe timeout.
- Claude keeps a 900-second cache, honours `Retry-After` with a 300-second
  default, and serves its last good value as stale after a rate limit.
- ClinePass accepts `CLINEPASS_API_KEY` and
  `~/.cline/data/settings/providers.json`.

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
