# Account quota through handoffkeep

`pacelever gate -m sonnet --account-key claude:0123456789abcdef --json` judges
the named account using the same gate rules as a local launch. The key must
match the profile's pool. This path judges only that account's fresh automatic
shared observation; it never substitutes the invoking host's cache, manual
observations or local limit records. Missing, stale, malformed or unreadable
shared evidence exits 4. Account-only manual evidence is insufficient for this
placement preflight. Normal role, policy, window and pool-class checks still apply.
JSON and `--gate-output` receipts include `account_key`; admitted text headers
include it as well. The placed host must still run its local gate before starting
work, including any locally recorded provider limits. Omitting `--account-key`
keeps the local gate path.

When the shared reading is stale or missing and this host holds the same account
(its derived key equals the requested key), the gate measures once, publishes
under the normal lease and judges the fresh
value. Any other host keeps the read-only behavior and exits 4 with the reason
(`this host does not hold the account`, or `this host may not measure the
account`); a failed refresh exits 4 with `local refresh failed (<kind>)`. A fresh
reading is never re-measured, and a malformed one (invalid, future-dated or
non-automatic) exits 4 without a refresh.

A free SWE-2 profile (`devin-swe2`, `devin-swe2-medium`, `devin-swe2-max`) on a
named account is judged on the shared `free_bucket`: `available` admits,
`exhausted` before its `resets_at` exits 3, and `unknown` or an absent field
exits 4. Paid Devin rungs keep their percentage buckets.

Deploy handoffkeep's quota API and additive startup migration first. Then enable
this opt-in configuration in each host's private pacelever config:

```toml
[quota]
url = "https://YOUR_HK_ENDPOINT"
credential_env = "HANDOFFKEEP_TOKEN"
host_id = "mac-work"
fresh_ttl_s = 300
lease_ttl_s = 120
wait_s = 1
presence_every_s = 600
measurable_accounts = ["codex:0123456789abcdef"]

# Optional per-pool freshness overrides; these take precedence over
# fresh_ttl_s. The defaults are claude = 900 and every other pool = 300.
[quota.pool_fresh_ttl_s]
claude = 900
codex = 300

[quota.accounts]
codex = "0123456789abcdef"
claude = "fedcba9876543210"
```

Replace the illustrative fingerprints and endpoint with private configuration.
Never put the credential in TOML. A non-empty value in the environment variable
named by `credential_env` is used as the token. When that variable is unset or
empty (agents and daemons usually have no `HANDOFFKEEP_TOKEN`), pacelever keeps
the token and CF Access pair it loaded from the handoffkeep `config.env`, the same
file the catalog path reads (see `docs/config.md`, "Catalog credentials"), but only
when `quota.url` has the same scheme, host and port as the `HANDOFFKEEP_URL` that
credential was stored for. For any other endpoint (different host or port, or a
plaintext downgrade) no stored token or CF Access pair is sent; set the variable
named by `credential_env` explicitly. Tokens are never logged. HTTPS is required; the existing explicit `bench.allow_plaintext_quota_share` opt-in is
available for controlled testing. Existing CF Access credential support applies.

The account key is pool plus a 16-hex SHA-256 fingerprint. For Codex,
Claude and Devin it is resolved before measuring from the local CLI identity,
using the exact existing `codex-account|account_id` or
`claude-account|accountUuid` hash, or `devin-account|org_id` for Devin.
Devin reads `devin.org_id` from `$XDG_CONFIG_HOME/devin/config.json` and requires
a non-empty `windsurf_api_key` in `$XDG_DATA_HOME/devin/credentials.toml` to
confirm a local login. The XDG defaults are `~/.config` and `~/.local/share`.
The API key is never a fingerprint input. A missing login, missing organization
or unreadable file leaves Devin's identity unknown; a token alone is insufficient.
No identity lookup makes a network request. Neither raw identity nor tokens are sent.
An explicit `quota.accounts` mapping lets a host read an account without its CLI
login. For Codex/Claude/Devin the derived local identity always decides the key
and the right to probe; a configured fingerprint never renames or blocks it.
For other pools, configure the fingerprint explicitly and list the key in
`measurable_accounts` only where that account's CLI is logged in.
The herdr event's CLI selectors also select the identity used for the key.

## Per-host setup checklist

Items 1 and 2 gate reads — without them the read fails before any provider is
contacted. Item 3 gates probing and publishing; a host that only reads fresh
shared observations does not need it.

1. **An account key for every measured pool.** `claude`, `codex` and `devin` derive
   their fingerprint from the local CLI login; every other pool (`grok`,
   `commandcode`, `agy`, …) has no local derivation and gets its key
   only from `[quota.accounts]`. Without an entry the account key is empty and
   every read for that pool fails as `account_unknown`.
2. **Plaintext opt-in for an `http://` hk endpoint.** HTTPS is the default;
   an `http://` `quota.url` also requires `allow_plaintext_quota_share = true`
   under `[bench]` (see `docs/config.md`). Without it every shared read fails
   as `shared_unavailable` even though hk answers.
3. **Measurable and hk-granted keys.** `claude`, `codex` and `devin` need no
   allowlist: holding the login is enough to probe and publish. For every
   other pool, each `pool:fingerprint` key must appear in
   `measurable_accounts` to be allowed to probe, and the matching account
   must be granted on the hk side (`HANDOFFKEEP_QUOTA_<client>`). A key that
   is measurable locally but not granted on hk is readable but can never
   publish, so it stays `shared_unavailable`/`shared_wait` until another
   granted host measures it.

Minimal example (placeholders only — use your own endpoint, fingerprints and
host id):

```toml
[bench]
allow_plaintext_quota_share = true   # only if quota.url is http://

[quota]
url = "http://YOUR_HK_HOST:PORT"
credential_env = "HANDOFFKEEP_TOKEN"
host_id = "your-host-id"
measurable_accounts = [
  "grok:fedcba9876543210",
]

[quota.accounts]
grok = "fedcba9876543210"            # required: grok has no local derivation
# claude/codex/devin derive their key and their right to measure from the
# local CLI login; no quota.accounts entry or allowlist is needed.
```

Failure signatures:

| Symptom | Cause |
|---|---|
| `account_unknown` | pool has no `quota.accounts` entry and no local claude/codex/devin derivation |
| `shared_unavailable` on every read | `http://` url without `allow_plaintext_quota_share = true`, or hk unreachable/not granting |
| reads but never probes | non-derivable pool key missing from `measurable_accounts`, or not granted via `HANDOFFKEEP_QUOTA_<client>` |

Every collection reads hk first, including `refresh` and `--no-cache`. A fresh
shared observation bypasses local cache and probing. Stale/missing data causes
an eligible host to acquire a short lease, re-read, probe within the lease
budget, publish, and release. A measuring host that loses the race waits for
the in-flight publisher until that holder's lease expires; an expired window
means the publication failed, so the host takes the slot over once and
measures itself. Read-only hosts wait briefly and read again. The report is
`shared_pending` while a publisher is still inside its lease window,
`shared_failed` when a window closed without a publication, and
`shared_unavailable` when hk itself cannot be reached. A holder that fails to
measure or publish releases the lease before returning, so no lease is held
across a failed measurement. No host probes independently during an hk
outage. Lease expiry permits takeover; hk fences a late publisher. There are
no new workers or persistent processes.

A same-account local snapshot is used when hk calls fail, and also when hk is
reachable but has no usable observation and the local probe is rate-limited or
still inside its recorded 429 backoff. Such output is always stale with its age;
during an hk outage it is also degraded. A different account's cache is
discarded. A `rate_limited` probe result honours `Retry-After` and otherwise
backs off for five minutes; other provider failures do not suppress the next
shared probe. Local cache TTL and non-rate-limit provider backoff never bypass
the shared read. Shared mode disables the legacy document-based quota
fallback/publication so it cannot turn a degraded local reading into a fresh
one. Inflight tracking keeps its existing behavior.

`manual set`, `manual list`, and `manual clear` use the same account API in shared
mode. Manual entries are independent per quota window; a write on one host is
visible on all readers. A failed shared write returns an error for retry.
Free-form reasons and local legacy audit fields are not transmitted. Host-local
manual entries are ignored in shared mode; re-enter a valid observation once
with the shared configuration enabled. Manual expiry and reset checks still
apply, and a shared outage cannot be hidden by old local manual data.

## Devin SWE-2 free state (runtug wire contract)

**Server prerequisite:** deploy additive handoffkeep support for the optional
`free_bucket` observation field and allow `kind: "auto"` with `buckets: []` when
that field is valid. Earlier servers reject the unknown field or the empty
percentage list. Their rejection follows the existing shared publication failure
path (`shared_publish_failed`, stale/degraded local fallback); it does not change
the local SWE-2 gate result. This client change does not deploy the server.
The server must validate, persist, return and include `free_bucket` in observation
deduplication. Scope this field to `devin` account keys and automatic observations;
manual observations and all other pool schemas retain their existing behavior.

Devin's SWE-2 free bucket has no measurable utilization. For `devin:<fingerprint>`,
an automatic observation therefore carries this additive field alongside the
existing paid percentage buckets. The same lease and presence paths apply.
The JSON below is the `observation` object inside the existing publication
envelope (`{"holder":"<lease-holder>","observation":...}`); `GET .../latest`
returns it as `observation` with the usual server-assigned `id`.

Available, with no percentage measurement:

```json
{
  "kind": "auto",
  "measured_at": "2026-10-30T00:00:00Z",
  "buckets": [],
  "free_bucket": {"state": "available"}
}
```

An exceed recorded with `pacelever observe devin-free-limit ...`, held until T:

```json
{
  "kind": "auto",
  "measured_at": "2026-10-30T00:00:00Z",
  "buckets": [],
  "free_bucket": {
    "state": "exhausted",
    "resets_at": "2026-10-30T01:00:00Z"
  }
}
```

`free_bucket` has exactly these fields:

| Field | Contract |
|---|---|
| `state` | `available`, `exhausted`, or `unknown` |
| `resets_at` | RFC3339 timestamp, required only for `exhausted`; absent for the other states |

`available` means no active exceed is known, not measured 0% usage. `exhausted`
applies to `devin-swe2`, `devin-swe2-medium` and `devin-swe2-max` until
`resets_at`; at or after T a reader treats that state as `available`. A damaged
local record uses the existing conservative `observed_at + 60 minutes` hold
when possible; if neither time is readable it publishes
`{"free_bucket":{"state":"unknown"}}`. `unknown`, an absent field, an invalid
field, or an observation outside the normal freshness window is insufficient
evidence for automatic free placement. Apply freshness to the observation as
usual; an expired exhausted reset does not make an old observation fresh.

A new local exceed bypasses an otherwise fresh shared `available` state and
is published under the normal account lease. Refreshing hosts preserve any
active shared exceed, choosing the later reset when a local exceed also exists.
A damaged local record cannot replace an active shared exceed with `unknown`.
Consequently a local `--clear` does not erase an active hold published by another
host; shared readers continue to honor its reset. The local `gate -m devin-swe2*`
still uses its existing local free-limit records and return codes.

A paid probe is not required to publish this free state. Successful paid
percentage buckets and plan data keep their existing representation; failed
paid probes contribute no paid measurement. Free model tags from `models list`
are omitted from `buckets`, and `free_bucket` never contains `used_pct`.
An optional paid web source must select the same organization as the CLI login;
a mismatch is refused as `account_mismatch` rather than published under its key.
Consumers must not use `free_bucket` as evidence about paid credit availability.
No account ID, credential, host ID, source job or local audit text appears in
the observation. A matching, locally logged-in measurable host publishes
presence through the existing separate endpoint after a successful shared read
or publication.

## Host presence

After a successful shared measurement, the measuring host also publishes a
presence record so placement can see which host last saw an account. A host
that only reads fresh shared observations publishes presence too, provided its
local identity resolves to the measured key (the same checks that gate
probing); read-only hosts never publish:

```
PUT /v1/quota/presence
{"host_id":"host-<stable-id>","account_key":"<pool>:<fingerprint>","last_seen":"<RFC3339>"}
```

It is a separate record from the quota observation, which deliberately carries
no host metadata. `host_id` is, in order of precedence: `PACELEVER_HOST_ID` or
`HANDOFFKEEP_HOST_ID` from the environment; `[quota] host_id` from the private
config (use the machine id placement matches on, e.g. `mac-work`); otherwise a
truncated SHA-256 of the local hostname. The raw hostname is never sent. The
configured value may contain only letters, digits, `.`, `_` and `-` (at most 64
characters); anything else makes the configuration invalid. Publishes are
rate-limited to at most one per `presence_every_s` seconds (default 600) per
host and account, recorded in a `presence.json` next to the cache file.
Publication is
best-effort: a missing endpoint or an hk error never changes a quota verdict and never fails collection. hk must expose this endpoint
for records to persist; pacelever does not create it.

Omit `quota.url` to retain the existing local collection behavior. Tests use an
in-memory fake hk and an HTTP test server; no database or real provider probes
are needed. The six-host contention test asserts one probe, one shared publication and one
presence record per host.
