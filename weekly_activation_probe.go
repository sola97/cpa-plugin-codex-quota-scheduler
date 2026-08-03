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

type weeklyActivationEffectOutcome string

const (
	weeklyActivationEffectSync      weeklyActivationEffectOutcome = "sync"
	weeklyActivationEffectRetry     weeklyActivationEffectOutcome = "retry"
	weeklyActivationEffectCooldown  weeklyActivationEffectOutcome = "cooldown"
	weeklyActivationEffectManual    weeklyActivationEffectOutcome = "manual"
	weeklyActivationEffectConfirmed weeklyActivationEffectOutcome = "confirmed"
)

// weeklyActivationEffect carries facts observed by a held sequence. Its
// state transition is applied only after the Coordinator validates the
// binding token, so an obsolete sequence cannot replace current state.
type weeklyActivationEffect struct {
	Instance        AuthInstanceID
	AttemptID       string
	Outcome         weeklyActivationEffectOutcome
	Quota           *ParsedQuota
	ResetAt         time.Time
	ProbeAt         time.Time
	RetryNotBefore  time.Time
	CooldownUntil   time.Time
	Err             error
	CompleteAttempt bool
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
		if state.State != WeeklyActivationReady && state.State != WeeklyActivationRetryWait {
			continue
		}
		deadline := state.NextCheckAt
		if deadline.IsZero() {
			deadline = now
		}
		consider(deadline)
	}
	for _, attempt := range persisted.ProbeAttempts {
		if attempt.Purpose != ProbePurposeWeeklyActivation {
			continue
		}
		switch attempt.Phase {
		case ProbeAttemptPrepared:
			consider(now)
		case ProbeAttemptSending, ProbeAttemptSent, ProbeAttemptSentUnknown:
			deadline := attempt.VerifyNotBefore
			if deadline.IsZero() {
				deadline = now
			}
			consider(deadline)
		}
	}
	return next
}

func weeklyActivationRetryDeadline(now time.Time, attempts int, notBefore time.Time) time.Time {
	next := now.Add(probeBackoff(attempts))
	if notBefore.After(next) {
		return notBefore
	}
	return next
}

func setWeeklyActivationError(state *WeeklyActivationProbeState, err error) {
	if err != nil {
		state.LastError = sanitizeResetProbeError(redactSecrets(err.Error()))
	}
}

func (r *QuotaRefresher) applyWeeklyActivationEffect(authID string, effect weeklyActivationEffect) error {
	if r == nil || r.runtimeStore == nil || r.state == nil || effect.Instance == 0 {
		return nil
	}
	now := r.now()
	applied := false
	_, err := r.runtimeStore.Update(func(persisted *PersistentState) error {
		attempt, exists := persisted.ProbeAttempts[effect.Instance]
		if effect.AttemptID != "" && (!exists || attempt.AttemptID != effect.AttemptID) {
			return nil
		}
		state := persisted.WeeklyActivationProbes[effect.Instance]
		switch effect.Outcome {
		case weeklyActivationEffectSync:
			if effect.Quota == nil {
				return nil
			}
			next, keep := deriveWeeklyActivationState(state, true, *effect.Quota, now, false)
			if keep {
				persisted.WeeklyActivationProbes[effect.Instance] = next
			} else {
				delete(persisted.WeeklyActivationProbes, effect.Instance)
			}
		case weeklyActivationEffectRetry:
			if state.State != WeeklyActivationManualRefreshRequired && state.State != WeeklyActivationConfirmed {
				state.State = WeeklyActivationRetryWait
				state.Attempts++
				state.NextCheckAt = weeklyActivationRetryDeadline(now, state.Attempts, effect.RetryNotBefore)
				state.CooldownUntil = time.Time{}
				setWeeklyActivationError(&state, effect.Err)
				persisted.WeeklyActivationProbes[effect.Instance] = state
			}
		case weeklyActivationEffectCooldown:
			if state.State != WeeklyActivationManualRefreshRequired && state.State != WeeklyActivationConfirmed {
				state.State = WeeklyActivationCooldown
				state.Attempts++
				state.CooldownUntil = effect.CooldownUntil
				state.NextCheckAt = time.Time{}
				setWeeklyActivationError(&state, effect.Err)
				persisted.WeeklyActivationProbes[effect.Instance] = state
			}
		case weeklyActivationEffectManual:
			state.State = WeeklyActivationManualRefreshRequired
			state.WeeklyResetAt = effect.ResetAt
			state.NextCheckAt = time.Time{}
			state.CooldownUntil = time.Time{}
			setWeeklyActivationError(&state, effect.Err)
			persisted.WeeklyActivationProbes[effect.Instance] = state
		case weeklyActivationEffectConfirmed:
			state.State = WeeklyActivationConfirmed
			state.WeeklyResetAt = effect.ResetAt
			state.CooldownUntil = time.Time{}
			state.NextCheckAt = time.Time{}
			state.LastProbeAt = effect.ProbeAt
			state.LastError = ""
			persisted.WeeklyActivationProbes[effect.Instance] = state
		default:
			return nil
		}
		if effect.CompleteAttempt {
			delete(persisted.ProbeAttempts, effect.Instance)
		}
		applied = true
		return nil
	})
	if err != nil || !applied {
		return err
	}
	if effect.Quota != nil && r.state.ApplyProbeQuota(authID, effect.Instance, *effect.Quota) {
		publishSchedulerState(r.state, highestTierSet(r.runtimeRoster()), now)
	}
	r.wakeRefreshLoop()
	return nil
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

func weeklyActivationPostSendEvidence(quota ParsedQuota) bool {
	return quota.FiveHour != nil && quota.FiveHour.UsedPercent != nil && *quota.FiveHour.UsedPercent > 0
}
