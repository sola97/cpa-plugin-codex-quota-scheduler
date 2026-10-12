package main

import (
	"fmt"
	"math"
	"time"
)

func validateQuotaReserves(accounts map[string]AccountAnnotation) error {
	for key, annotation := range accounts {
		if annotation.FiveHourReservePercent < 0 || annotation.FiveHourReservePercent > 100 {
			return fmt.Errorf("account %q: five_hour_reserve_percent must be an integer from 0 to 100", key)
		}
		if annotation.WeeklyReservePercent < 0 || annotation.WeeklyReservePercent > 100 {
			return fmt.Errorf("account %q: weekly_reserve_percent must be an integer from 0 to 100", key)
		}
		for _, ruleSet := range []struct {
			name       string
			steps      []QuotaReserveStep
			maxMinutes int
		}{
			{"five_hour_reserve_steps", annotation.FiveHourReserveSteps, 300},
			{"weekly_reserve_steps", annotation.WeeklyReserveSteps, 10080},
		} {
			seen := make(map[int]bool, len(ruleSet.steps))
			for _, step := range ruleSet.steps {
				if step.MinutesBeforeReset < 1 || step.MinutesBeforeReset > ruleSet.maxMinutes || step.ReservePercent < 0 || step.ReservePercent > 100 {
					return fmt.Errorf("account %q: %s requires minutes from 1 to %d and reserve_percent from 0 to 100", key, ruleSet.name, ruleSet.maxMinutes)
				}
				if seen[step.MinutesBeforeReset] {
					return fmt.Errorf("account %q: %s has duplicate minutes_before_reset %d", key, ruleSet.name, step.MinutesBeforeReset)
				}
				seen[step.MinutesBeforeReset] = true
			}
		}
	}
	return nil
}

func hasQuotaReserve(annotation AccountAnnotation) bool {
	return annotation.FiveHourReservePercent > 0 || annotation.WeeklyReservePercent > 0
}

type QuotaReservation struct {
	Annotation AccountAnnotation
	FiveHour   *QuotaWindow
	LongWindow *QuotaWindow
	Family     AccountFamily
	Unknown    bool
}

func accountReservationState(account AccountState) QuotaReservation {
	return QuotaReservation{Annotation: account.Annotation, FiveHour: account.Quota.FiveHour, LongWindow: account.Quota.LongWindow, Family: account.Family, Unknown: account.LastSuccessAt.IsZero() || account.Stale}
}

func accountReservationReason(account AccountState, now time.Time) string {
	return reservationReason(accountReservationState(account), now)
}

func reservationReason(state QuotaReservation, now time.Time) string {
	if !hasQuotaReserve(state.Annotation) {
		return ""
	}
	if state.Unknown {
		return "reserve_quota_unknown"
	}
	// A successful read can omit a 5h window for accounts without that limit.
	if state.FiveHour != nil {
		reserve := effectiveQuotaReserve(state.Annotation.FiveHourReservePercent, state.Annotation.FiveHourReserveSteps, WindowFiveHour, state.FiveHour, now)
		if reason := windowReservationReason(state.FiveHour, reserve, "five_hour_reserved"); reason != "" {
			return reason
		}
	}
	if state.Annotation.WeeklyReservePercent > 0 {
		if state.Family == AccountFamilyUnknown || (state.Family == AccountFamilyWeekly && state.LongWindow == nil) {
			return "reserve_quota_unknown"
		}
		if state.LongWindow != nil && state.LongWindow.Kind == WindowWeekly {
			reserve := effectiveQuotaReserve(state.Annotation.WeeklyReservePercent, state.Annotation.WeeklyReserveSteps, WindowWeekly, state.LongWindow, now)
			return windowReservationReason(state.LongWindow, reserve, "weekly_reserved")
		}
	}
	return ""
}

func defaultQuotaReserveSteps(kind WindowKind) []QuotaReserveStep {
	if kind == WindowFiveHour {
		return []QuotaReserveStep{{20, 3}, {10, 1}}
	}
	return []QuotaReserveStep{{60, 10}, {30, 5}, {15, 3}, {5, 1}}
}

func effectiveQuotaReserve(base int, steps []QuotaReserveStep, kind WindowKind, window *QuotaWindow, now time.Time) int {
	if base == 0 || window == nil || window.ResetAt.IsZero() || !window.ResetAt.After(now) {
		return base
	}
	if steps == nil {
		steps = defaultQuotaReserveSteps(kind)
	}
	untilReset := window.ResetAt.Sub(now)
	closest := time.Duration(0)
	reserve := base
	for _, step := range steps {
		threshold := time.Duration(step.MinutesBeforeReset) * time.Minute
		if untilReset <= threshold && (closest == 0 || threshold < closest) {
			closest = threshold
			reserve = step.ReservePercent
		}
	}
	return reserve
}

func windowReservationReason(window *QuotaWindow, reserve int, reason string) string {
	if reserve == 0 {
		return ""
	}
	if window.UsedPercent == nil || math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) {
		return "reserve_quota_unknown"
	}
	// Keep the observed block through reset until new quota evidence arrives.
	if window.Exhausted || *window.UsedPercent >= float64(100-reserve) {
		return reason
	}
	return ""
}
