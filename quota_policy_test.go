package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func reservePolicyAccount(now, resetAt time.Time, remaining float64, resetCredits *int) AccountState {
	used := 100 - remaining
	return AccountState{
		AuthID:        "reserve-account",
		AuthIndex:     "reserve-account.json",
		Instance:      101,
		Provider:      "codex",
		Family:        AccountFamilyWeekly,
		LastSuccessAt: now,
		Quota: ParsedQuota{
			Family:                     AccountFamilyWeekly,
			LongWindow:                 &QuotaWindow{Kind: WindowWeekly, UsedPercent: &used, ResetAt: resetAt},
			ResetCreditsAvailableCount: resetCredits,
		},
		Annotation: AccountAnnotation{WeeklyQuotaReserve: &WeeklyQuotaReservePolicy{Enabled: true, Percent: 20, UnlockHours: 5}},
	}
}

func quotaIntPtr(value int) *int { return &value }

func TestWeeklyQuotaReserveDefaultsToDisabled(t *testing.T) {
	policy := effectiveWeeklyQuotaReservePolicy(AccountAnnotation{})
	if policy.Enabled || policy.Percent != 20 || policy.UnlockHours != 5 {
		t.Fatalf("policy = %#v, want disabled 20%% / 5h", policy)
	}
}

func TestWeeklyQuotaReserveIsAccountScoped(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	resetCredits := 0
	reserved := reservePolicyAccount(now, now.Add(24*time.Hour), 10, &resetCredits)
	notReserved := reserved
	notReserved.AuthID = "ordinary-account"
	notReserved.Annotation.WeeklyQuotaReserve = nil

	if !weeklyQuotaReserveBlocks(reserved, now) {
		t.Fatal("enabled account should be held in reserve")
	}
	if weeklyQuotaReserveBlocks(notReserved, now) {
		t.Fatal("account without an enabled policy must remain schedulable")
	}
}

func TestWeeklyQuotaReserveEligibilityAndBoundary(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	resetCredits := 0
	account := reservePolicyAccount(now, now.Add(24*time.Hour), 19.9, &resetCredits)

	if !weeklyQuotaReserveBlocks(account, now) {
		t.Fatal("account below the threshold should be held in reserve")
	}
	if got := weeklyQuotaReserveUnlockAt(account, now); !got.Equal(now.Add(19 * time.Hour)) {
		t.Fatalf("unlockAt = %s, want %s", got, now.Add(19*time.Hour))
	}

	account.Quota.LongWindow.ResetAt = now.Add(5 * time.Hour)
	if !weeklyQuotaReserveBlocks(account, now) || weeklyQuotaReserveNearUnlock(account, now) {
		t.Fatal("account must remain reserved at the exact unlock boundary")
	}

	account.Quota.LongWindow.ResetAt = now.Add(4*time.Hour + 59*time.Minute)
	if weeklyQuotaReserveBlocks(account, now) || !weeklyQuotaReserveNearUnlock(account, now) {
		t.Fatal("account should be released inside the unlock window")
	}

	for _, test := range []struct {
		name    string
		account AccountState
	}{
		{name: "equal threshold", account: reservePolicyAccount(now, now.Add(24*time.Hour), 20, &resetCredits)},
		{name: "positive reset credits", account: reservePolicyAccount(now, now.Add(24*time.Hour), 10, quotaIntPtr(1))},
		{name: "unknown reset credits", account: reservePolicyAccount(now, now.Add(24*time.Hour), 10, nil)},
		{name: "monthly account", account: func() AccountState {
			a := reservePolicyAccount(now, now.Add(24*time.Hour), 10, &resetCredits)
			a.Family = AccountFamilyMonthly
			a.Quota.Family = AccountFamilyMonthly
			return a
		}()},
		{name: "stale account", account: func() AccountState {
			a := reservePolicyAccount(now, now.Add(24*time.Hour), 10, &resetCredits)
			a.Stale = true
			return a
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if weeklyQuotaReserveBlocks(test.account, now) {
				t.Fatalf("%s should not be held in reserve", test.name)
			}
		})
	}
}

func TestWeeklyQuotaReserveDoesNotMaskExhaustionAndReadmitsOnRefresh(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	resetCredits := 0
	exhausted := reservePolicyAccount(now, now.Add(24*time.Hour), 0, &resetCredits)
	exhausted.Quota.LongWindow.Exhausted = true
	if weeklyQuotaReserveBlocks(exhausted, now) {
		t.Fatal("fully exhausted account must not be described as reserve quota")
	}
	_, available, reason, _ := accountQueueStateWithConfig(exhausted, now, DefaultConfig())
	if available || reason != "weekly_exhausted" {
		t.Fatalf("exhausted account = available:%v reason:%q", available, reason)
	}

	account := reservePolicyAccount(now, now.Add(24*time.Hour), 10, &resetCredits)
	if !weeklyQuotaReserveBlocks(account, now) {
		t.Fatal("initial account should be reserved")
	}
	usedAtThreshold := 80.0
	account.Quota.LongWindow.UsedPercent = &usedAtThreshold
	if weeklyQuotaReserveBlocks(account, now) {
		t.Fatal("quota refresh at the threshold should readmit the account")
	}
	usedLow := 90.0
	account.Quota.LongWindow.UsedPercent = &usedLow
	account.Quota.ResetCreditsAvailableCount = quotaIntPtr(1)
	if weeklyQuotaReserveBlocks(account, now) {
		t.Fatal("positive reset credits should readmit the account")
	}
}

func TestWeeklyQuotaReserveStatusPayloadShowsPolicyAndUnlockTime(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	resetCredits := 0
	account := reservePolicyAccount(now, now.Add(24*time.Hour), 10, &resetCredits)
	snapshot := StateSnapshot{
		Now:      now,
		Config:   DefaultConfig(),
		Accounts: []AccountState{account},
		Annotations: AnnotationState{
			Accounts: map[string]AccountAnnotation{"auth:" + account.AuthID: account.Annotation},
		},
	}
	ordered := BuildOrderedAccounts(
		pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: account.AuthID, Provider: "codex"}}},
		snapshot,
		now,
	)
	payload := BuildStatusPayload(snapshot, ordered)
	if len(payload.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(payload.Accounts))
	}
	status := payload.Accounts[0]
	if !status.WeeklyQuotaReserve.Enabled || status.WeeklyQuotaReserve.Percent != 20 || status.WeeklyQuotaReserve.UnlockHours != 5 {
		t.Fatalf("reserve status = %#v", status.WeeklyQuotaReserve)
	}
	if status.UnavailableReason != weeklyQuotaReserveReason {
		t.Fatalf("unavailable reason = %q, want %q", status.UnavailableReason, weeklyQuotaReserveReason)
	}
	if !strings.Contains(status.StatusNote, "当前剩余 10.0%") || !strings.Contains(status.StatusNote, "解封") {
		t.Fatalf("status note = %q, want remaining quota and unlock wording", status.StatusNote)
	}
}

func TestPickCodexAccountRejectsReservedCandidateWithoutDelegatingFallback(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	resetCredits := 0
	account := reservePolicyAccount(now, now.Add(24*time.Hour), 10, &resetCredits)
	decision := PickCodexAccount(
		pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: account.AuthID, Provider: "codex"}}},
		StateSnapshot{Now: now, Config: DefaultConfig(), Accounts: []AccountState{account}},
		now,
	)
	if !decision.Handled || !decision.Reject || decision.DelegateBuiltin != "" || decision.Reason != weeklyQuotaReserveReason {
		t.Fatalf("decision = %#v", decision)
	}
	if len(decision.Ordered) != 1 || decision.Ordered[0].UnavailableReason != weeklyQuotaReserveReason {
		t.Fatalf("ordered = %#v", decision.Ordered)
	}
}

func TestSchedulerPickPublishedRejectsReservedCandidateAtUnlockBoundary(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	resetCredits := 0
	account := reservePolicyAccount(now, now.Add(5*time.Hour), 10, &resetCredits)
	view := accountViewFromState(account, DefaultConfig(), now, nil)
	PublishSchedulerSnapshot(&SchedulerSnapshot{
		HandleEnabled:     true,
		Fallback:          FallbackFillFirst,
		Accounts:          []AccountView{view},
		ActiveHighestTier: map[string]struct{}{account.AuthID: {}},
	})
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: account.AuthID, Provider: "codex"}}}
	if got := schedulerPickPublished(req, now); !got.Reject || got.Reason != weeklyQuotaReserveReason {
		t.Fatalf("boundary decision = %#v", got)
	}
	if got := schedulerPickPublished(req, now.Add(time.Nanosecond)); got.AuthID != account.AuthID || got.Reject {
		t.Fatalf("released decision = %#v", got)
	}
}

func TestHandleSchedulerPickRejectsReservedCandidate(t *testing.T) {
	now := time.Now().UTC()
	resetCredits := 0
	account := reservePolicyAccount(now, now.Add(24*time.Hour), 10, &resetCredits)
	PublishSchedulerSnapshot(&SchedulerSnapshot{
		HandleEnabled:     true,
		Fallback:          FallbackFillFirst,
		Accounts:          []AccountView{accountViewFromState(account, DefaultConfig(), now, nil)},
		ActiveHighestTier: map[string]struct{}{account.AuthID: {}},
	})
	raw, err := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: account.AuthID, Provider: "codex"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleSchedulerPick(raw); err == nil || !strings.Contains(err.Error(), weeklyQuotaReserveReason) {
		t.Fatalf("handleSchedulerPick error = %v, want reserve rejection", err)
	}
}

func TestSchedulerPickPublishedKeepsFallbackWhenReserveIsNotBlocking(t *testing.T) {
	now := time.Now().UTC()
	PublishSchedulerSnapshot(&SchedulerSnapshot{
		HandleEnabled:     true,
		Fallback:          FallbackFillFirst,
		Accounts:          []AccountView{{ID: "unavailable", Cache: CacheStale}},
		ActiveHighestTier: map[string]struct{}{"unavailable": {}},
	})
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "unavailable", Provider: "codex"}}}
	if got := schedulerPickPublished(req, now); got.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst || got.Reject {
		t.Fatalf("fallback decision = %#v", got)
	}
}

func TestAccountReservePatchPersistsAndRepublishesSnapshot(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	resetCredits := 0
	account := reservePolicyAccount(now, now.Add(24*time.Hour), 10, &resetCredits)
	account.Annotation.WeeklyQuotaReserve = nil
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(account)
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{account.AuthID: {}}})
	publishSchedulerState(store, nil, now)
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: account.AuthID, Provider: "codex"}}}
	if got := schedulerPickPublished(req, now); got.AuthID != account.AuthID {
		t.Fatalf("before patch decision = %#v", got)
	}

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "PATCH",
		Path:   "/plugins/codex-quota-scheduler/annotations/account",
		Body:   []byte(`{"auth_id":"reserve-account","weekly_quota_reserve":{"enabled":true,"percent":15,"unlock_hours":3}}`),
	}, now)
	if resp.StatusCode != 200 {
		t.Fatalf("patch status = %d body=%s", resp.StatusCode, resp.Body)
	}
	if got := schedulerPickPublished(req, now); !got.Reject || got.Reason != weeklyQuotaReserveReason {
		t.Fatalf("immediately republished decision = %#v", got)
	}

	persisted, _, err := loadUserData(semanticStatePaths(defaultStatePath()).UserData)
	if err != nil {
		t.Fatal(err)
	}
	policy := persisted.Accounts["instance:101"].WeeklyQuotaReserve
	if policy == nil || !policy.Enabled || policy.Percent != 15 || policy.UnlockHours != 3 {
		t.Fatalf("persisted policy = %#v", policy)
	}
}

func TestAccountReserveExportImportAndValidation(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{Accounts: map[string]AccountAnnotation{
		"auth:exported": {WeeklyQuotaReserve: &WeeklyQuotaReservePolicy{Enabled: true, Percent: 15, UnlockHours: 3}},
	}})
	export := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/export",
	}, now)
	if export.StatusCode != 200 {
		t.Fatalf("export status = %d body=%s", export.StatusCode, export.Body)
	}

	imported := NewPluginState(DefaultConfig())
	response := HandleManagementRequest(imported, pluginapi.ManagementRequest{
		Method: "POST",
		Path:   "/plugins/codex-quota-scheduler/import",
		Body:   export.Body,
	}, now)
	if response.StatusCode != 200 {
		t.Fatalf("import status = %d body=%s", response.StatusCode, response.Body)
	}
	policy := imported.Annotations().Accounts["auth:exported"].WeeklyQuotaReserve
	if policy == nil || !policy.Enabled || policy.Percent != 15 || policy.UnlockHours != 3 {
		t.Fatalf("imported policy = %#v", policy)
	}

	response = HandleManagementRequest(imported, pluginapi.ManagementRequest{
		Method: "POST",
		Path:   "/plugins/codex-quota-scheduler/import",
		Body:   []byte(`{"config":{"HandleEnabled":true,"MonthlyMode":"expiry_order","QuotaRefreshInterval":1800000000000,"StaleAfter":18000000000000,"MaxRefreshConcurrency":1,"QuotaEndpoint":"https://chatgpt.com/backend-api/wham/usage","MaxLogEntries":200,"LogRetention":86400000000000},"accounts":{"auth:invalid":{"weekly_quota_reserve":{"enabled":true,"percent":20,"unlock_hours":0}}}}`),
	}, now)
	if response.StatusCode != 400 {
		t.Fatalf("invalid import status = %d body=%s", response.StatusCode, response.Body)
	}
}
