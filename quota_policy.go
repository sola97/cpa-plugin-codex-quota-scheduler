package main

import "time"

const weeklyQuotaReserveReason = "weekly_quota_reserve"

func weeklyQuotaReserveApplies(account AccountState, cfg Config, now time.Time) (time.Time, time.Time, bool) {
	cfg = NormalizeConfig(cfg)
	if !cfg.EnableWeeklyQuotaReserve || cfg.WeeklyQuotaReservePercent <= 0 {
		return time.Time{}, time.Time{}, false
	}
	if account.Stale || account.Family != AccountFamilyWeekly {
		return time.Time{}, time.Time{}, false
	}
	window := account.Quota.LongWindow
	if window == nil || window.UsedPercent == nil || window.ResetAt.IsZero() || !window.ResetAt.After(now) {
		return time.Time{}, time.Time{}, false
	}
	if account.Quota.ResetCreditsAvailableCount == nil || *account.Quota.ResetCreditsAvailableCount != 0 {
		return time.Time{}, time.Time{}, false
	}
	if 100-*window.UsedPercent >= cfg.WeeklyQuotaReservePercent {
		return time.Time{}, time.Time{}, false
	}
	return window.ResetAt, window.ResetAt.Add(-cfg.WeeklyQuotaReserveUnlockWindow), true
}

func weeklyQuotaReserveBlocks(account AccountState, cfg Config, now time.Time) bool {
	resetAt, _, ok := weeklyQuotaReserveApplies(account, cfg, now)
	if !ok {
		return false
	}
	return resetAt.Sub(now) >= NormalizeConfig(cfg).WeeklyQuotaReserveUnlockWindow
}

func weeklyQuotaReserveNearUnlock(account AccountState, cfg Config, now time.Time) bool {
	resetAt, _, ok := weeklyQuotaReserveApplies(account, cfg, now)
	if !ok {
		return false
	}
	remaining := resetAt.Sub(now)
	return remaining > 0 && remaining < NormalizeConfig(cfg).WeeklyQuotaReserveUnlockWindow
}

func weeklyQuotaReserveUnlockAt(account AccountState, cfg Config) time.Time {
	if account.Quota.LongWindow == nil || account.Quota.LongWindow.ResetAt.IsZero() {
		return time.Time{}
	}
	return account.Quota.LongWindow.ResetAt.Add(-NormalizeConfig(cfg).WeeklyQuotaReserveUnlockWindow)
}
