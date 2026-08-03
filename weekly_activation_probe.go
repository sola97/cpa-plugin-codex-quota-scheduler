package main

import "time"

type WeeklyActivationState string

const (
	WeeklyActivationIdle                  WeeklyActivationState = "idle"
	WeeklyActivationReady                 WeeklyActivationState = "ready_for_probe"
	WeeklyActivationCooldown              WeeklyActivationState = "cooldown"
	WeeklyActivationRetryWait             WeeklyActivationState = "retry_wait"
	WeeklyActivationConfirmed             WeeklyActivationState = "confirmed"
	WeeklyActivationManualRefreshRequired WeeklyActivationState = "manual_refresh_required"
)

const (
	weeklyActivationResetTolerance = 2 * time.Minute
	weeklyActivationCooldownTime   = 10 * time.Minute
)

type WeeklyActivationProbeState struct {
	State         WeeklyActivationState `json:"state"`
	WeeklyResetAt time.Time             `json:"weekly_reset_at,omitempty"`
	CooldownUntil time.Time             `json:"cooldown_until,omitempty"`
	NextCheckAt   time.Time             `json:"next_check_at,omitempty"`
	LastProbeAt   time.Time             `json:"last_probe_at,omitempty"`
	Attempts      int                   `json:"attempts,omitempty"`
	LastError     string                `json:"last_error,omitempty"`
}

func weeklyActivationPrimaryReady(quota ParsedQuota) bool {
	return quota.FiveHour != nil && quota.FiveHour.UsedPercent != nil && *quota.FiveHour.UsedPercent == 0
}

func weeklyActivationResetAt(quota ParsedQuota) (time.Time, bool) {
	if quota.LongWindow == nil || quota.LongWindow.Kind != WindowWeekly || quota.LongWindow.ResetAt.IsZero() {
		return time.Time{}, false
	}
	return quota.LongWindow.ResetAt, true
}

func weeklyResetAligned(resetAt, now time.Time) bool {
	return !resetAt.IsZero() && absDuration(resetAt.Sub(now.Add(7*24*time.Hour))) <= weeklyActivationResetTolerance
}

func weeklyActivationEligible(quota ParsedQuota, now time.Time) bool {
	resetAt, ok := weeklyActivationResetAt(quota)
	return weeklyActivationPrimaryReady(quota) && ok && weeklyResetAligned(resetAt, now)
}

func weeklyActivationStateDue(state WeeklyActivationProbeState, now time.Time) bool {
	if state.State != WeeklyActivationReady && state.State != WeeklyActivationRetryWait {
		return false
	}
	return state.NextCheckAt.IsZero() || !state.NextCheckAt.After(now)
}

func deriveWeeklyActivationState(previous WeeklyActivationProbeState, exists bool, quota ParsedQuota, now time.Time, manualRefresh bool) (WeeklyActivationProbeState, bool) {
	next := previous
	primaryReady := weeklyActivationPrimaryReady(quota)
	resetAt, hasWeekly := weeklyActivationResetAt(quota)

	if !primaryReady {
		switch previous.State {
		case WeeklyActivationManualRefreshRequired, WeeklyActivationConfirmed, WeeklyActivationCooldown:
			return previous, exists
		default:
			if !exists {
				return WeeklyActivationProbeState{}, false
			}
			next.State = WeeklyActivationIdle
			next.NextCheckAt = time.Time{}
			next.CooldownUntil = time.Time{}
			next.LastError = ""
			return next, true
		}
	}

	if !hasWeekly {
		if exists && previous.State != WeeklyActivationIdle {
			next.State = WeeklyActivationManualRefreshRequired
			next.NextCheckAt = time.Time{}
			next.CooldownUntil = time.Time{}
			return next, true
		}
		return WeeklyActivationProbeState{}, false
	}

	if previous.State == WeeklyActivationConfirmed && previous.WeeklyResetAt.Equal(resetAt) {
		return previous, true
	}
	if exists && !previous.WeeklyResetAt.IsZero() && !previous.WeeklyResetAt.Equal(resetAt) && !manualRefresh && previous.State != WeeklyActivationIdle {
		next.State = WeeklyActivationManualRefreshRequired
		next.WeeklyResetAt = resetAt
		next.NextCheckAt = time.Time{}
		next.CooldownUntil = time.Time{}
		return next, true
	}

	if !weeklyResetAligned(resetAt, now) {
		next.State = WeeklyActivationManualRefreshRequired
		next.WeeklyResetAt = resetAt
		next.NextCheckAt = time.Time{}
		next.CooldownUntil = time.Time{}
		return next, true
	}

	if previous.State == WeeklyActivationManualRefreshRequired && !manualRefresh {
		return previous, true
	}

	if manualRefresh || !exists || previous.State == WeeklyActivationIdle || previous.State == WeeklyActivationManualRefreshRequired {
		next.State = WeeklyActivationReady
		next.WeeklyResetAt = resetAt
		next.NextCheckAt = now
		next.CooldownUntil = time.Time{}
		next.LastError = ""
		return next, true
	}

	next.WeeklyResetAt = resetAt
	return next, true
}

func (r *QuotaRefresher) bootstrapWeeklyActivationStates() error {
	if r == nil || r.runtimeStore == nil || r.state == nil || !r.state.Config().EnableWeeklyActivationProbe {
		return nil
	}
	now := r.now()
	snapshot := r.state.Snapshot(now)
	_, err := r.runtimeStore.Update(func(persisted *PersistentState) error {
		for _, account := range snapshot.Accounts {
			if account.Instance == 0 {
				continue
			}
			previous, exists := persisted.WeeklyActivationProbes[account.Instance]
			next, keep := deriveWeeklyActivationState(previous, exists, account.Quota, now, false)
			if !keep {
				continue
			}
			persisted.WeeklyActivationProbes[account.Instance] = next
		}
		return nil
	})
	return err
}

func (r *QuotaRefresher) syncWeeklyActivationState(instance AuthInstanceID, quota ParsedQuota, manualRefresh bool) error {
	if r == nil || r.runtimeStore == nil || instance == 0 || r.state == nil || !r.state.Config().EnableWeeklyActivationProbe {
		return nil
	}
	now := r.now()
	_, err := r.runtimeStore.Update(func(persisted *PersistentState) error {
		previous, exists := persisted.WeeklyActivationProbes[instance]
		next, keep := deriveWeeklyActivationState(previous, exists, quota, now, manualRefresh)
		if !keep {
			delete(persisted.WeeklyActivationProbes, instance)
			return nil
		}
		persisted.WeeklyActivationProbes[instance] = next
		return nil
	})
	if err == nil {
		r.wakeRefreshLoop()
	}
	return err
}

func (r *QuotaRefresher) weeklyActivationNextDeadline() time.Time {
	if r == nil || r.runtimeStore == nil || r.state == nil || !r.state.Config().EnableWeeklyActivationProbe {
		return time.Time{}
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return time.Time{}
	}
	now := r.now()
	var next time.Time
	consider := func(deadline time.Time) {
		if deadline.IsZero() {
			return
		}
		if next.IsZero() || deadline.Before(next) {
			next = deadline
		}
	}
	for _, state := range persisted.WeeklyActivationProbes {
		if !weeklyActivationStateDue(state, now) {
			continue
		}
		deadline := state.NextCheckAt
		if deadline.IsZero() {
			deadline = now
		}
		consider(deadline)
	}
	for _, attempt := range persisted.ProbeAttempts {
		if attempt.Purpose != ProbePurposeWeeklyActivation || !nonterminalProbeAttempt(attempt) {
			continue
		}
		consider(attempt.VerifyNotBefore)
	}
	return next
}

func (r *QuotaRefresher) weeklyActivationState(instance AuthInstanceID) (WeeklyActivationProbeState, bool) {
	if r == nil || r.runtimeStore == nil {
		return WeeklyActivationProbeState{}, false
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return WeeklyActivationProbeState{}, false
	}
	state, ok := persisted.WeeklyActivationProbes[instance]
	return state, ok
}

func (r *QuotaRefresher) markWeeklyActivationRetry(instance AuthInstanceID, err error) error {
	if r == nil || r.runtimeStore == nil || instance == 0 {
		return nil
	}
	now := r.now()
	_, updateErr := r.runtimeStore.Update(func(persisted *PersistentState) error {
		state := persisted.WeeklyActivationProbes[instance]
		if state.State == WeeklyActivationManualRefreshRequired || state.State == WeeklyActivationConfirmed {
			return nil
		}
		state.State = WeeklyActivationRetryWait
		state.Attempts++
		state.NextCheckAt = now.Add(probeBackoff(state.Attempts))
		state.CooldownUntil = time.Time{}
		if err != nil {
			state.LastError = sanitizeResetProbeError(redactSecrets(err.Error()))
		}
		persisted.WeeklyActivationProbes[instance] = state
		return nil
	})
	return updateErr
}

func (r *QuotaRefresher) markWeeklyActivationCooldown(instance AuthInstanceID, until time.Time, err error) error {
	if r == nil || r.runtimeStore == nil || instance == 0 {
		return nil
	}
	_, updateErr := r.runtimeStore.Update(func(persisted *PersistentState) error {
		state := persisted.WeeklyActivationProbes[instance]
		if state.State == WeeklyActivationManualRefreshRequired || state.State == WeeklyActivationConfirmed {
			return nil
		}
		state.State = WeeklyActivationCooldown
		state.CooldownUntil = until
		state.NextCheckAt = time.Time{}
		if err != nil {
			state.LastError = sanitizeResetProbeError(redactSecrets(err.Error()))
		}
		persisted.WeeklyActivationProbes[instance] = state
		return nil
	})
	return updateErr
}

func (r *QuotaRefresher) markWeeklyActivationManual(instance AuthInstanceID, resetAt time.Time, err error) error {
	if r == nil || r.runtimeStore == nil || instance == 0 {
		return nil
	}
	_, updateErr := r.runtimeStore.Update(func(persisted *PersistentState) error {
		state := persisted.WeeklyActivationProbes[instance]
		state.State = WeeklyActivationManualRefreshRequired
		state.WeeklyResetAt = resetAt
		state.NextCheckAt = time.Time{}
		state.CooldownUntil = time.Time{}
		if err != nil {
			state.LastError = sanitizeResetProbeError(redactSecrets(err.Error()))
		}
		persisted.WeeklyActivationProbes[instance] = state
		return nil
	})
	return updateErr
}

func (r *QuotaRefresher) markWeeklyActivationConfirmed(instance AuthInstanceID, resetAt, probeAt time.Time) error {
	if r == nil || r.runtimeStore == nil || instance == 0 {
		return nil
	}
	_, updateErr := r.runtimeStore.Update(func(persisted *PersistentState) error {
		state := persisted.WeeklyActivationProbes[instance]
		state.State = WeeklyActivationConfirmed
		state.WeeklyResetAt = resetAt
		state.CooldownUntil = time.Time{}
		state.NextCheckAt = time.Time{}
		state.LastProbeAt = probeAt
		state.LastError = ""
		persisted.WeeklyActivationProbes[instance] = state
		return nil
	})
	return updateErr
}

func weeklyActivationPostSendEvidence(quota ParsedQuota) bool {
	return quota.FiveHour != nil && quota.FiveHour.UsedPercent != nil && *quota.FiveHour.UsedPercent > 0
}
