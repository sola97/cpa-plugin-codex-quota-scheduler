package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func resetCreditPriorityIntPtr(value int) *int {
	return &value
}

func resetCreditPriorityWeeklyAccount(id string, resetAt time.Time) AccountState {
	usedLong := 15.0
	usedFiveHour := 10.0
	return AccountState{
		AuthID:   id,
		Provider: "codex",
		Family:   AccountFamilyWeekly,
		Quota: ParsedQuota{
			Family:     AccountFamilyWeekly,
			LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &usedLong, ResetAt: resetAt},
			FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &usedFiveHour, ResetAt: resetAt.Add(-12 * time.Hour)},
		},
		LastSuccessAt: resetAt.Add(-48 * time.Hour),
	}
}

func resetCreditPriorityMonthlyAccount(id string, resetAt time.Time) AccountState {
	usedLong := 15.0
	return AccountState{
		AuthID:   id,
		Provider: "codex",
		Family:   AccountFamilyMonthly,
		Quota: ParsedQuota{
			Family:     AccountFamilyMonthly,
			LongWindow: &QuotaWindow{Kind: WindowMonthly, UsedPercent: &usedLong, ResetAt: resetAt},
		},
		LastSuccessAt: resetAt.Add(-48 * time.Hour),
	}
}

func resetCreditPriorityRequest(ids ...string) pluginapi.SchedulerPickRequest {
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Priority: 1})
	}
	return pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: candidates}
}

func TestEarliestAvailableResetCreditExpiryFiltersInactiveCredits(t *testing.T) {
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	earliest := now.Add(24 * time.Hour)
	later := now.Add(48 * time.Hour)
	quota := ParsedQuota{
		ResetCreditsAvailableCount: resetCreditPriorityIntPtr(2),
		ResetCredits: []ResetCredit{
			{ID: "used", Status: "available", ExpiresAt: now.Add(time.Hour), UsedAt: now.Add(-time.Hour)},
			{ID: "expired", Status: "available", ExpiresAt: now.Add(-time.Hour)},
			{ID: "unavailable", Status: "used", ExpiresAt: now.Add(2 * time.Hour)},
			{ID: "later", Status: "AVAILABLE", ExpiresAt: later},
			{ID: "earliest", Status: "", ExpiresAt: earliest},
		},
	}

	if got := earliestAvailableResetCreditExpiry(quota, now); !got.Equal(earliest) {
		t.Fatalf("earliest available reset credit expiry = %s, want %s", got, earliest)
	}

	quota.ResetCreditsAvailableCount = resetCreditPriorityIntPtr(0)
	if got := earliestAvailableResetCreditExpiry(quota, now); !got.IsZero() {
		t.Fatalf("zero available count selected expiry %s", got)
	}
}

func TestExpiringResetCreditPriorityUsesSharedConsumptionDeadline(t *testing.T) {
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	naturalFirst := resetCreditPriorityWeeklyAccount("natural-first", now.Add(4*24*time.Hour))
	creditUrgent := resetCreditPriorityWeeklyAccount("credit-urgent", now.Add(5*24*time.Hour))
	creditExpiry := now.Add(18 * time.Hour)
	creditUrgent.Quota.ResetCreditsAvailableCount = resetCreditPriorityIntPtr(1)
	creditUrgent.Quota.ResetCredits = []ResetCredit{{ID: "credit-1", Status: "available", ExpiresAt: creditExpiry}}

	cfg := DefaultConfig()
	snapshot := StateSnapshot{
		Config:   cfg,
		Now:      now,
		Accounts: []AccountState{naturalFirst, creditUrgent},
		CPAAdmission: CPAAdmissionState{
			Observed: true,
			AuthIDs:  map[string]struct{}{naturalFirst.AuthID: {}, creditUrgent.AuthID: {}},
		},
	}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: naturalFirst.AuthID, Provider: "codex", Priority: 1},
			{ID: creditUrgent.AuthID, Provider: "codex", Priority: 1},
		},
	}

	ordered := BuildOrderedAccounts(req, snapshot, now)
	if len(ordered) != 2 || ordered[0].AuthID != creditUrgent.AuthID {
		t.Fatalf("management order = %#v, want %q first", ordered, creditUrgent.AuthID)
	}
	if !ordered[0].selectionView.ResetCreditPriority || !ordered[0].selectionView.ConsumeBy.Equal(creditExpiry) {
		t.Fatalf("priority view = %#v", ordered[0].selectionView)
	}

	if got := PickCodexAccount(req, snapshot, now); got.AuthID != creditUrgent.AuthID {
		t.Fatalf("direct pick = %#v, want %q", got, creditUrgent.AuthID)
	}

	PublishSchedulerSnapshot(schedulerSnapshotFromState(snapshot, nil))
	t.Cleanup(func() { PublishSchedulerSnapshot(&SchedulerSnapshot{}) })
	if got := schedulerPickPublished(req, now); got.AuthID != creditUrgent.AuthID {
		t.Fatalf("published snapshot pick = %#v, want %q", got, creditUrgent.AuthID)
	}

	payload := BuildStatusPayload(snapshot, ordered)
	if len(payload.Accounts) != 2 || !payload.Accounts[0].ResetCreditPriority || !payload.Accounts[0].ResetCreditPriorityExpiresAt.Equal(creditExpiry) {
		t.Fatalf("management status = %#v", payload.Accounts)
	}
	var page bytes.Buffer
	if err := statusTemplateV2.Execute(&page, payload); err != nil {
		t.Fatalf("render status page: %v", err)
	}
	for _, want := range []string{"主动重置优先", "最早有效期", creditExpiry.Format(time.RFC3339)} {
		if !bytes.Contains(page.Bytes(), []byte(want)) {
			t.Fatalf("status page missing %q", want)
		}
	}
}

func TestExpiringResetCreditPriorityKeepsExistingPriorityTiers(t *testing.T) {
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	pluginHigh := resetCreditPriorityWeeklyAccount("plugin-high", now.Add(4*24*time.Hour))
	pluginHigh.Annotation.SchedulerPriority = 10
	creditUrgent := resetCreditPriorityWeeklyAccount("credit-urgent", now.Add(5*24*time.Hour))
	creditUrgent.Quota.ResetCreditsAvailableCount = resetCreditPriorityIntPtr(1)
	creditUrgent.Quota.ResetCredits = []ResetCredit{{ID: "credit-1", Status: "available", ExpiresAt: now.Add(time.Hour)}}

	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{pluginHigh, creditUrgent}}
	if got := PickCodexAccount(resetCreditPriorityRequest(pluginHigh.AuthID, creditUrgent.AuthID), snapshot, now); got.AuthID != pluginHigh.AuthID {
		t.Fatalf("plugin priority was overridden: %#v", got)
	}

	monthly := resetCreditPriorityMonthlyAccount("monthly", now.Add(7*24*time.Hour))
	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModePriority
	snapshot = StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{monthly, creditUrgent}}
	if got := PickCodexAccount(resetCreditPriorityRequest(monthly.AuthID, creditUrgent.AuthID), snapshot, now); got.AuthID != monthly.AuthID {
		t.Fatalf("monthly mode priority was overridden: %#v", got)
	}
}

func TestResetCreditExpiryAfterNaturalResetDoesNotChangeOrderOrStatus(t *testing.T) {
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	naturalFirst := resetCreditPriorityWeeklyAccount("natural-first", now.Add(24*time.Hour))
	creditLater := resetCreditPriorityWeeklyAccount("credit-later", now.Add(2*24*time.Hour))
	creditLater.Quota.ResetCreditsAvailableCount = resetCreditPriorityIntPtr(1)
	creditLater.Quota.ResetCredits = []ResetCredit{{ID: "credit-1", Status: "available", ExpiresAt: now.Add(3 * 24 * time.Hour)}}
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{naturalFirst, creditLater}}
	ordered := BuildOrderedAccounts(resetCreditPriorityRequest(naturalFirst.AuthID, creditLater.AuthID), snapshot, now)

	if len(ordered) != 2 || ordered[0].AuthID != naturalFirst.AuthID {
		t.Fatalf("order = %#v, want natural deadline first", ordered)
	}
	if ordered[1].selectionView.ResetCreditPriority {
		t.Fatalf("later reset credit unexpectedly marked priority: %#v", ordered[1].selectionView)
	}
	payload := BuildStatusPayload(snapshot, ordered)
	if payload.Accounts[1].ResetCreditPriority || !payload.Accounts[1].ResetCreditPriorityExpiresAt.IsZero() {
		t.Fatalf("later reset credit unexpectedly exposed as priority: %#v", payload.Accounts[1])
	}
}
