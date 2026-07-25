package main

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func reservePolicyAccount(now, resetAt time.Time, remaining float64, resetCredits *int) AccountState {
	used := 100 - remaining
	return AccountState{
		AuthID:   "reserve-account",
		Provider: "codex",
		Family:   AccountFamilyWeekly,
		Quota: ParsedQuota{
			Family:                     AccountFamilyWeekly,
			LongWindow:                 &QuotaWindow{Kind: WindowWeekly, UsedPercent: &used, ResetAt: resetAt},
			ResetCreditsAvailableCount: resetCredits,
		},
		LastSuccessAt: now,
	}
}

func TestWeeklyQuotaReserveDefaultsAndOverrides(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.EnableWeeklyQuotaReserve || cfg.WeeklyQuotaReservePercent != 20 || cfg.WeeklyQuotaReserveUnlockWindow != 5*time.Hour {
		t.Fatalf("defaults = %#v", cfg)
	}

	override, err := DecodeConfig([]byte("enable_weekly_quota_reserve: false\nweekly_quota_reserve_percent: 12.5\nweekly_quota_reserve_unlock_window: 2h\n"))
	if err != nil {
		t.Fatalf("DecodeConfig returned error: %v", err)
	}
	if override.EnableWeeklyQuotaReserve || override.WeeklyQuotaReservePercent != 12.5 || override.WeeklyQuotaReserveUnlockWindow != 2*time.Hour {
		t.Fatalf("override = %#v", override)
	}
}

func TestDecodeConfigRejectsInvalidWeeklyQuotaReserveSettings(t *testing.T) {
	for _, raw := range []string{
		"weekly_quota_reserve_percent: -1\n",
		"weekly_quota_reserve_percent: 100.1\n",
		"weekly_quota_reserve_unlock_window: 0s\n",
	} {
		if _, err := DecodeConfig([]byte(raw)); err == nil {
			t.Fatalf("DecodeConfig accepted invalid config %q", raw)
		}
	}
}

func TestWeeklyQuotaReserveBlocksOnlyOutsideUnlockWindow(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	resetCredits := 0
	account := reservePolicyAccount(now, now.Add(24*time.Hour), 19.9, &resetCredits)
	cfg := DefaultConfig()

	if !weeklyQuotaReserveBlocks(account, cfg, now) {
		t.Fatal("account should be held in reserve")
	}
	if got := weeklyQuotaReserveUnlockAt(account, cfg); !got.Equal(now.Add(19 * time.Hour)) {
		t.Fatalf("unlockAt = %s, want %s", got, now.Add(19*time.Hour))
	}

	account.Quota.LongWindow.ResetAt = now.Add(4 * time.Hour)
	if weeklyQuotaReserveBlocks(account, cfg, now) || !weeklyQuotaReserveNearUnlock(account, cfg, now) {
		t.Fatal("account should be released inside the five-hour unlock window")
	}
	account.Quota.LongWindow.ResetAt = now.Add(5 * time.Hour)
	if !weeklyQuotaReserveBlocks(account, cfg, now) || weeklyQuotaReserveNearUnlock(account, cfg, now) {
		t.Fatal("account should remain reserved at the exact five-hour boundary")
	}
}

func TestWeeklyQuotaReserveBoundaryAndUnknownData(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	resetCredits := 0

	tests := []struct {
		name      string
		account   AccountState
		wantBlock bool
	}{
		{name: "exact threshold", account: reservePolicyAccount(now, now.Add(24*time.Hour), 20, &resetCredits)},
		{name: "reset credit available", account: reservePolicyAccount(now, now.Add(24*time.Hour), 10, intPtr(1))},
		{name: "reset credit data missing", account: reservePolicyAccount(now, now.Add(24*time.Hour), 10, nil)},
		{name: "stale quota", account: staleReservePolicyAccount(now, &resetCredits)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := weeklyQuotaReserveBlocks(test.account, cfg, now); got != test.wantBlock {
				t.Fatalf("weeklyQuotaReserveBlocks = %v, want %v", got, test.wantBlock)
			}
		})
	}

	disabled := cfg
	disabled.EnableWeeklyQuotaReserve = false
	if weeklyQuotaReserveBlocks(reservePolicyAccount(now, now.Add(24*time.Hour), 10, &resetCredits), disabled, now) {
		t.Fatal("disabled reserve policy should not block an account")
	}
}

func staleReservePolicyAccount(now time.Time, resetCredits *int) AccountState {
	account := reservePolicyAccount(now, now.Add(24*time.Hour), 10, resetCredits)
	account.Stale = true
	return account
}

func intPtr(value int) *int {
	return &value
}

func TestPickReservesWeeklyAccountAndDoesNotDelegateFillFirst(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	resetCredits := 0
	account := reservePolicyAccount(now, now.Add(24*time.Hour), 10, &resetCredits)
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{account}}

	decision := PickCodexAccount(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: account.AuthID, Provider: "codex"}}}, snapshot, now)
	if !decision.Reject || decision.DelegateBuiltin != "" || !decision.Handled || decision.Reason != weeklyQuotaReserveReason {
		t.Fatalf("decision = %#v", decision)
	}
	if len(decision.Ordered) != 1 || decision.Ordered[0].UnavailableReason != weeklyQuotaReserveReason {
		t.Fatalf("ordered = %#v", decision.Ordered)
	}
}

func TestPickReleasesWeeklyReserveInsideUnlockWindow(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	resetCredits := 0
	account := reservePolicyAccount(now, now.Add(4*time.Hour), 10, &resetCredits)
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{account}}

	decision := PickCodexAccount(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: account.AuthID, Provider: "codex"}}}, snapshot, now)
	if decision.AuthID != account.AuthID || decision.Reject {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestSettingsPayloadIncludesWeeklyQuotaReserve(t *testing.T) {
	cfg := DefaultConfig()
	payload := SettingsFromConfig(cfg)
	if !payload.EnableWeeklyQuotaReserve || payload.WeeklyQuotaReservePercent != 20 || payload.WeeklyQuotaReserveUnlockWindow != "5h0m0s" {
		t.Fatalf("payload = %#v", payload)
	}

	cfg, err := ConfigFromSettings(cfg, SettingsPayload{
		HandleEnabled:                  true,
		MonthlyMode:                    MonthlyModeExpiryOrder,
		QuotaRefreshInterval:           "30m",
		StaleAfter:                     "5h",
		EnableUsageFeedback:            true,
		EnableWeeklyQuotaReserve:       true,
		WeeklyQuotaReservePercent:      15,
		WeeklyQuotaReserveUnlockWindow: "3h",
		MaxRefreshConcurrency:          1,
	})
	if err != nil {
		t.Fatalf("ConfigFromSettings returned error: %v", err)
	}
	if cfg.WeeklyQuotaReservePercent != 15 || cfg.WeeklyQuotaReserveUnlockWindow != 3*time.Hour {
		t.Fatalf("cfg = %#v", cfg)
	}
}

func TestReserveStatusNoteIncludesUnlockTime(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	resetCredits := 0
	account := reservePolicyAccount(now, now.Add(24*time.Hour), 10, &resetCredits)
	status := BuildStatusPayload(StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{account}}, []ScheduledAccount{
		{AuthID: account.AuthID, Family: account.Family, QueueStatus: QueueStatusUnavailable, UnavailableReason: weeklyQuotaReserveReason, SortTime: now.Add(19 * time.Hour)},
	})
	if len(status.Accounts) != 1 || !strings.Contains(status.Accounts[0].StatusNote, "解封") || !strings.Contains(status.Accounts[0].StatusNote, "2026-07-26T04:00:00Z") {
		t.Fatalf("status = %#v", status.Accounts)
	}
}
