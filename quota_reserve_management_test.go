package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func quotaReserveManagementRequest(t *testing.T, store *PluginState, method, path, body string, now time.Time) pluginapi.ManagementResponse {
	t.Helper()
	return HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: method,
		Path:   managementBasePath + path,
		Body:   []byte(body),
	}, now)
}

func quotaReserveManagementStatePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous := defaultStatePath
	previousSnapshot := publishedSchedulerSnapshot.Load()
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() {
		defaultStatePath = previous
		publishedSchedulerSnapshot.Store(previousSnapshot)
	})
	return filepath.Join(dir, ".user-data.json")
}

func TestQuotaReservePatchKeepsIndependentValuesAndEmptySteps(t *testing.T) {
	statePath := quotaReserveManagementStatePath(t)
	store := NewPluginState(DefaultConfig())
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	initial := `{"accounts":{"auth:acct":{"five_hour_reserve_percent":8,"weekly_reserve_percent":12,"five_hour_reserve_steps":[{"minutes_before_reset":20,"reserve_percent":3}],"weekly_reserve_steps":[{"minutes_before_reset":60,"reserve_percent":10}]}}}`
	if resp := quotaReserveManagementRequest(t, store, http.MethodPut, "/annotations", initial, now); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT annotations status=%d body=%s", resp.StatusCode, resp.Body)
	}
	resp := quotaReserveManagementRequest(t, store, http.MethodPatch, "/annotations/account", `{"auth_id":"acct","five_hour_reserve_percent":17,"weekly_reserve_steps":[]}`, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH account status=%d body=%s", resp.StatusCode, resp.Body)
	}
	annotation := store.Annotations().Accounts["auth:acct"]
	if annotation.FiveHourReservePercent != 17 || annotation.WeeklyReservePercent != 12 {
		t.Fatalf("base values were not independently updated: %#v", annotation)
	}
	if annotation.WeeklyReserveSteps == nil || len(annotation.WeeklyReserveSteps) != 0 {
		t.Fatalf("explicit empty weekly rules were not preserved: %#v", annotation.WeeklyReserveSteps)
	}
	if len(annotation.FiveHourReserveSteps) != 1 || annotation.FiveHourReserveSteps[0].MinutesBeforeReset != 20 {
		t.Fatalf("omitted 5-hour rules changed: %#v", annotation.FiveHourReserveSteps)
	}
	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var disk PluginDiskState
	if err := json.Unmarshal(persisted, &disk); err != nil {
		t.Fatal(err)
	}
	got := disk.Accounts["auth:acct"]
	if got.WeeklyReserveSteps == nil || len(got.WeeklyReserveSteps) != 0 || got.WeeklyReservePercent != 12 {
		t.Fatalf("persisted annotation lost independent values or empty rules: %#v", got)
	}
	snapshot := publishedSchedulerSnapshot.Load()
	if snapshot == nil || snapshot.ReserveAnnotations["auth:acct"].FiveHourReservePercent != 17 {
		t.Fatalf("successful PATCH did not publish updated scheduler state: %#v", snapshot)
	}
}

func TestAccountAnnotationPatchResolvesInstanceIdentityFromAuthID(t *testing.T) {
	quotaReserveManagementStatePath(t)
	now := time.Now()
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"acct": {}}})
	used := 95.0
	store.UpsertQuota(AccountState{
		AuthID:        "acct",
		Instance:      1,
		Provider:      "codex",
		Priority:      1,
		LastSuccessAt: now,
		Quota: ParsedQuota{FiveHour: &QuotaWindow{
			Kind: WindowFiveHour, UsedPercent: &used, ResetAt: now.Add(time.Hour),
		}},
	})
	request := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "acct", Provider: "codex", Priority: 1}}}
	publishSchedulerState(store, nil, now)
	if decision := schedulerPickPublished(request, now); decision.AuthID != "acct" {
		t.Fatalf("account was not selectable before reserve update: %#v", decision)
	}
	resp := quotaReserveManagementRequest(t, store, http.MethodPatch, "/annotations/account", `{"auth_id":"acct","alias":"saved","quota_reserve_enabled":true,"five_hour_reserve_percent":10}`, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH account status=%d body=%s", resp.StatusCode, resp.Body)
	}
	annotations := store.Annotations()
	if got := annotations.Accounts["instance:1"].Alias; got != "saved" {
		t.Fatalf("instance annotation alias=%q, want saved; annotations=%#v", got, annotations.Accounts)
	}
	if _, found := annotations.Accounts["auth:acct"]; found {
		t.Fatalf("PATCH created auth annotation instead of updating instance identity: %#v", annotations.Accounts)
	}
	if decision := schedulerPickPublished(request, now); !decision.Reject || decision.Reason != "quota_reserve_protected" {
		t.Fatalf("saved reserve did not immediately affect published scheduler: %#v", decision)
	}
	accounts := ApplyAnnotations(store.Snapshot(now).Accounts, annotations)
	if len(accounts) != 1 || accounts[0].Annotation.Alias != "saved" {
		t.Fatalf("saved alias was not applied to account: %#v", accounts)
	}
}

func TestQuotaReserveInvalidPutPatchAndImportDoNotPersist(t *testing.T) {
	statePath := quotaReserveManagementStatePath(t)
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	invalid := `{"accounts":{"auth:acct":{"five_hour_reserve_steps":[{"minutes_before_reset":20,"reserve_percent":3},{"minutes_before_reset":20,"reserve_percent":1}]}}}`
	for _, test := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "put", method: http.MethodPut, path: "/annotations", body: invalid},
		{name: "patch", method: http.MethodPatch, path: "/annotations/account", body: `{"auth_id":"acct","weekly_reserve_percent":101}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := quotaReserveManagementRequest(t, store, test.method, test.path, test.body, now)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", resp.StatusCode, resp.Body)
			}
		})
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("invalid annotation request wrote state file (stat error=%v)", err)
	}
	importBody := `{"config":{},"accounts":{"auth:acct":{"weekly_reserve_steps":[{"minutes_before_reset":10081,"reserve_percent":2}]}}}`
	resp := quotaReserveManagementRequest(t, store, http.MethodPost, "/import", importBody, now)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid import status=%d body=%s, want 400", resp.StatusCode, resp.Body)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("invalid import wrote state file (stat error=%v)", err)
	}
}

func TestQuotaReserveStatusReportsConfiguredAndEffectiveValues(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	used := 40.0
	account := AccountState{
		AuthID:        "acct",
		Priority:      1,
		Family:        AccountFamilyWeekly,
		LastSuccessAt: now,
		Annotation: AccountAnnotation{
			QuotaReserveEnabled:    true,
			FiveHourReservePercent: 9,
			WeeklyReservePercent:   12,
			FiveHourReserveSteps:   []QuotaReserveStep{{MinutesBeforeReset: 15, ReservePercent: 4}},
			WeeklyReserveSteps:     []QuotaReserveStep{{MinutesBeforeReset: 45, ReservePercent: 6}},
		},
		Quota: ParsedQuota{
			FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: now.Add(10 * time.Minute)},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &used, ResetAt: now.Add(30 * time.Minute)},
		},
	}
	snapshot := StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{account}, Now: now}
	payload := BuildStatusPayload(snapshot, []ScheduledAccount{{AuthID: "acct", Annotation: account.Annotation}})
	if len(payload.Accounts) != 1 {
		t.Fatalf("status accounts=%#v", payload.Accounts)
	}
	status := payload.Accounts[0]
	if status.FiveHourReservePercent != 9 || status.FiveHourEffectiveReserve != 4 || status.WeeklyReservePercent != 12 || status.WeeklyEffectiveReserve != 6 {
		t.Fatalf("status reserve values=%#v", status)
	}
	if len(status.FiveHourReserveSteps) != 1 || len(status.WeeklyReserveSteps) != 1 {
		t.Fatalf("status omitted configured rules: %#v", status)
	}
}

func TestQuotaReserveManagementPageContainsEditableIndependentRules(t *testing.T) {
	resp := HandleManagementRequest(NewPluginState(DefaultConfig()), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/status"}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	page := string(resp.Body)
	for _, want := range []string{
		"five_hourReservePercent", "weeklyReservePercent", "five_hourReserveRules", "weeklyReserveRules",
		"minutes_before_reset", "reserve_percent", "edit.addRule", "edit.removeRule", "collectReserveRules",
		"five_hour_reserve_steps:fiveHourSteps", "weekly_reserve_steps:weeklySteps",
		"five_hour_effective_reserve_percent", "weekly_effective_reserve_percent",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("status page is missing reserve editor/status contract %q", want)
		}
	}
}
