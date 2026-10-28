# Account quota through handoffkeep

Deploy handoffkeep's quota API and additive startup migration first. Then enable
this opt-in configuration in each host's private pacelever config:

```toml
[quota]
url = "https://YOUR_HK_ENDPOINT"
credential_env = "HANDOFFKEEP_TOKEN"
fresh_ttl_s = 300
lease_ttl_s = 120
wait_s = 1
measurable_accounts = ["codex:0123456789abcdef"]

[quota.accounts]
codex = "0123456789abcdef"
claude = "fedcba9876543210"
```

Replace the illustrative fingerprints and endpoint with private configuration.
Put the credential in the named environment variable, never in TOML. HTTPS is
required; the existing explicit `bench.allow_plaintext_quota_share` opt-in is
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

Omit `quota.url` to retain the existing local collection behavior. Tests use an
in-memory fake hk and an HTTP test server; no database or real provider probes
are needed. The six-host contention test asserts one probe and one publication.
