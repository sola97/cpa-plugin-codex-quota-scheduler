package main

import (
	"fmt"
	"math"
	"time"
)

const weeklyQuotaReserveReason = "weekly_quota_reserve"

func DefaultWeeklyQuotaReservePolicy() WeeklyQuotaReservePolicy {
	return WeeklyQuotaReservePolicy{
		Enabled:     false,
		Percent:     20,
		UnlockHours: 5,
	}
}

type WeeklyQuotaReserveState struct {
	Policy           WeeklyQuotaReservePolicy
	Applies          bool
	Blocked          bool
	UnlockAt         time.Time
	RemainingPercent float64
}

func effectiveWeeklyQuotaReservePolicy(annotation AccountAnnotation) WeeklyQuotaReservePolicy {
	policy := DefaultWeeklyQuotaReservePolicy()
	if annotation.WeeklyQuotaReserve == nil {
		return policy
	}

	configured := *annotation.WeeklyQuotaReserve
	policy.Enabled = configured.Enabled
	if configured.Percent > 0 {
		policy.Percent = configured.Percent
	}
	if configured.UnlockHours > 0 {
		policy.UnlockHours = configured.UnlockHours
	}
	return policy
}

func validateWeeklyQuotaReservePolicy(policy *WeeklyQuotaReservePolicy) error {
	if policy == nil || !policy.Enabled {
		return nil
	}
	if policy.Percent <= 0 || policy.Percent > 100 {
		return fmt.Errorf("weekly_quota_reserve.percent must be greater than 0 and no greater than 100")
	}
	if policy.UnlockHours <= 0 {
		return fmt.Errorf("weekly_quota_reserve.unlock_hours must be positive")
	}
	return nil
}

func weeklyQuotaReserveState(account AccountState, now time.Time) WeeklyQuotaReserveState {
	state := WeeklyQuotaReserveState{Policy: effectiveWeeklyQuotaReservePolicy(account.Annotation)}
	if !state.Policy.Enabled || account.Stale || account.Family != AccountFamilyWeekly {
		return state
	}

	window := account.Quota.LongWindow
	if window == nil || window.UsedPercent == nil || window.ResetAt.IsZero() || !window.ResetAt.After(now) {
		return state
	}
	if windowExhausted(window, now) || *window.UsedPercent >= 100 {
		return state
	}
	if account.Quota.ResetCreditsAvailableCount == nil || *account.Quota.ResetCreditsAvailableCount != 0 {
		return state
	}

	state.RemainingPercent = 100 - *window.UsedPercent
	if state.RemainingPercent >= state.Policy.Percent {
		return state
	}

	state.Applies = true
	state.UnlockAt = window.ResetAt.Add(-state.Policy.UnlockDuration())
	state.Blocked = !now.After(state.UnlockAt)
	return state
}

func (p WeeklyQuotaReservePolicy) UnlockDuration() time.Duration {
	return time.Duration(math.Round(p.UnlockHours * float64(time.Hour)))
}

func weeklyQuotaReserveBlocks(account AccountState, now time.Time) bool {
	return weeklyQuotaReserveState(account, now).Blocked
}

func weeklyQuotaReserveNearUnlock(account AccountState, now time.Time) bool {
	state := weeklyQuotaReserveState(account, now)
	return state.Applies && !state.Blocked
}

func weeklyQuotaReserveUnlockAt(account AccountState, now time.Time) time.Time {
	return weeklyQuotaReserveState(account, now).UnlockAt
}

func weeklyQuotaReserveSnapshotBlocks(account AccountView, now time.Time) bool {
	return account.WeeklyQuotaReserveBlocked && (account.WeeklyQuotaReserveUnlockAt.IsZero() || !now.After(account.WeeklyQuotaReserveUnlockAt))
}
