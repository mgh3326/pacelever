package pacelever

// Scope.Kind values.
const (
	ScopeAccount = "account" // the whole account or plan
	ScopeModel   = "model"   // one model's own limit
	ScopeGroup   = "group"   // a provider metering group (Antigravity)
)

// Observation.ErrorKind values. They are part of the JSON quota contract and
// of the cache/backoff rules, so every producer and comparison uses these
// names rather than a string literal.
const (
	// Provider measurement failures.
	ErrKindAuth            = "auth"              // 401/403 without a provider-specific hint
	ErrKindAuthFailed      = "auth_failed"       // refused credential with a login hint
	ErrKindCredentials     = "credentials"       // no usable local credential
	ErrKindEdgeRejected    = "edge_rejected"     // an edge proxy refused the request
	ErrKindHTTP            = "http"              // any other non-200 response
	ErrKindNetwork         = "network"           // the request never got a response
	ErrKindParse           = "parse_error"       // a response or screen had no usable windows
	ErrKindProbe           = "probe_error"       // the CLI probe could not run
	ErrKindProbeInProgress = "probe_in_progress" // another process holds the probe lock
	ErrKindRateLimited     = "rate_limited"      // 429; RetryAfterS drives the backoff
	ErrKindTimeout         = "timeout"           // the collection deadline passed
	ErrKindTokenExpired    = "token_expired"     // the saved login is past its expiry

	// Collector and pool state.
	ErrKindBackoff       = "backoff"        // a negative cache entry without a recorded kind
	ErrKindCache         = "cache_error"    // the observation could not be persisted
	ErrKindExcluded      = "excluded"       // pool class is exclude
	ErrKindLegacyFailure = "legacy_failure" // imported scopefuel failure
	ErrKindUnsubscribed  = "unsubscribed"   // pool is not subscribed

	// Shared quota.
	ErrKindAccountMismatch     = "account_mismatch"
	ErrKindAccountUnknown      = "account_unknown"
	ErrKindManualOnly          = "manual_only"
	ErrKindProbeFailed         = "probe_failed"
	ErrKindSharedPending       = "shared_pending"
	ErrKindSharedPublishFailed = "shared_publish_failed"
	ErrKindSharedReleaseFailed = "shared_release_failed"
	ErrKindSharedUnavailable   = "shared_unavailable"
	ErrKindSharedWait          = "shared_wait"
)
