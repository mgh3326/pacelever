# Account quota through handoffkeep

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
measurable_accounts = ["codex:0123456789abcdef"]

[quota.accounts]
codex = "0123456789abcdef"
claude = "fedcba9876543210"
```

Replace the illustrative fingerprints and endpoint with private configuration.
Never put the credential in TOML. A non-empty value in the environment variable
named by `credential_env` is used as the token. When that variable is unset or
empty (agents and daemons usually have no `HANDOFFKEEP_TOKEN`), pacelever keeps
the token and CF Access pair it loaded from the handoffkeep `config.env`, the same
file the catalog path reads (see `docs/config.md`, "Catalog credentials"). Tokens
are never logged. HTTPS is required; the existing explicit `bench.allow_plaintext_quota_share` opt-in is
available for controlled testing. Existing CF Access credential support applies.

The account key is pool plus the existing 16-hex SHA-256 fingerprint. For Codex
and Claude it is resolved before measuring from the selected local CLI identity,
using the exact existing `codex-account|account_id` or
`claude-account|accountUuid` hash. Neither raw identity nor tokens are sent.
An explicit `quota.accounts` mapping lets a host read an account without its CLI
login. Only keys in `measurable_accounts` may probe; for Codex/Claude the selected
local identity must also match. For other pools, configure the fingerprint
explicitly and list it as measurable only where that account's CLI is logged in.
The herdr event's CLI selectors also select the identity used for the key.

Every collection reads hk first, including `refresh` and `--no-cache`. A fresh
shared observation bypasses local cache and probing. Stale/missing data causes
an eligible host to acquire a short lease, re-read, probe within the lease
budget, publish, and release. Other hosts wait briefly and read again. No host
probes independently during an hk outage. Lease expiry permits takeover; hk
fences a late publisher. There are no new workers or persistent processes.

A same-account local snapshot is used only when hk calls fail. Such output always
has `stale: true`, `degraded: true` and an error kind. A different account's cache
is discarded. Local cache TTL and provider backoff never bypass the shared read.
Shared mode disables the legacy document-based quota fallback/publication so it
cannot turn a degraded local reading into a fresh one. Inflight tracking keeps
its existing behavior.

`manual set`, `manual list`, and `manual clear` use the same account API in shared
mode. Manual entries are independent per quota window; a write on one host is
visible on all readers. A failed shared write returns an error for retry.
Free-form reasons and local legacy audit fields are not transmitted. Host-local
manual entries are ignored in shared mode; re-enter a valid observation once
with the shared configuration enabled. Manual expiry and reset checks still
apply, and a shared outage cannot be hidden by old local manual data.

## Host presence

After a successful shared measurement, the measuring host also publishes a
presence record so placement can see which host last saw an account:

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
characters); anything else makes the configuration invalid. Publication is
best-effort: a missing endpoint or an hk error never changes a quota verdict and never fails collection. hk must expose this endpoint
for records to persist; pacelever does not create it.

Omit `quota.url` to retain the existing local collection behavior. Tests use an
in-memory fake hk and an HTTP test server; no database or real provider probes
are needed. The six-host contention test asserts one probe and one publication.
