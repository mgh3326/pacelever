package pacelever

import "time"

// FreeBucketState describes SWE-2 availability, never a usage measurement.
// An exhausted state requires a reset; unknown means a limit record is damaged.
type FreeBucketState struct {
	State    string     `json:"state"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

const (
	FreeBucketAvailable = "available"
	FreeBucketExhausted = "exhausted"
	FreeBucketUnknown   = "unknown"
)

func localDevinFreeBucket(now time.Time) *FreeBucketState {
	if limit := ActiveFreeLimit(now); limit != nil {
		reset, err := limit.EffectiveReset()
		if err != nil {
			return &FreeBucketState{State: FreeBucketUnknown}
		}
		return &FreeBucketState{State: FreeBucketExhausted, ResetsAt: &reset}
	}
	return &FreeBucketState{State: FreeBucketAvailable}
}

func sharedFreeBucket(b *FreeBucketState, pool string, now time.Time) (*FreeBucketState, bool) {
	if b == nil {
		return nil, true
	}
	if pool != "devin" {
		return nil, false
	}
	switch b.State {
	case FreeBucketAvailable, FreeBucketUnknown:
		return &FreeBucketState{State: b.State}, b.ResetsAt == nil
	case FreeBucketExhausted:
		if b.ResetsAt == nil || b.ResetsAt.IsZero() {
			return nil, false
		}
		if !b.ResetsAt.After(now) {
			return &FreeBucketState{State: FreeBucketAvailable}, true
		}
		return &FreeBucketState{State: b.State, ResetsAt: b.ResetsAt}, true
	}
	return nil, false
}

// A locally recorded exceed must reach hk even while its available reading is
// fresh. A host without that record must not clear another host's active hold.
func devinFreeNeedsPublish(shared, local *FreeBucketState, now time.Time) bool {
	prior, ok := sharedFreeBucket(shared, "devin", now)
	if !ok || prior == nil {
		return true
	}
	if local.State == FreeBucketUnknown {
		return prior.State != FreeBucketUnknown && prior.State != FreeBucketExhausted
	}
	return local.State == FreeBucketExhausted && (prior.State != FreeBucketExhausted || local.ResetsAt.After(*prior.ResetsAt))
}

func mergeDevinFreeBucket(shared, local *FreeBucketState, now time.Time) *FreeBucketState {
	prior, ok := sharedFreeBucket(shared, "devin", now)
	if ok && prior != nil && prior.State == FreeBucketExhausted {
		if local.State != FreeBucketExhausted || prior.ResetsAt.After(*local.ResetsAt) {
			return prior
		}
	}
	return local
}

func devinFreeTag(b Bucket) bool {
	if b.Scope.Kind != ScopeModel || b.Window != "" {
		return false
	}
	switch b.Scope.Name {
	case "swe-2-high", "swe-2-medium", "swe-2-max":
		return true
	}
	return false
}
