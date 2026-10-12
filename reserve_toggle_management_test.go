package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func reserveToggleManagementRequest(t *testing.T, store *PluginState, body string, now time.Time) pluginapi.ManagementResponse {
	t.Helper()
	return HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPatch,
		Path:   managementBasePath + "/annotations/account",
		Body:   []byte(body),
	}, now)
}

func reserveToggleStatePath(t *testing.T) string {
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

func TestQuotaReserveAccountTogglePatchPersistenceAndOmission(t *testing.T) {
	statePath := reserveToggleStatePath(t)
	store := NewPluginState(DefaultConfig())
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	if resp := reserveToggleManagementRequest(t, store, `{"auth_id":"acct","five_hour_reserve_percent":9}`, now); resp.StatusCode != http.StatusOK {
		t.Fatalf("initial annotation status=%d body=%s", resp.StatusCode, resp.Body)
	}

	for _, enabled := range []bool{true, false} {
		body := `{"auth_id":"acct","quota_reserve_enabled":` + map[bool]string{true: "true", false: "false"}[enabled] + `}`
		resp := reserveToggleManagementRequest(t, store, body, now)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("toggle enabled=%t status=%d body=%s", enabled, resp.StatusCode, resp.Body)
		}
		if got := store.Annotations().Accounts["auth:acct"].QuotaReserveEnabled; got != enabled {
			t.Fatalf("QuotaReserveEnabled=%t after setting %t", got, enabled)
		}
		persisted, _, err := loadUserData(statePath)
		if err != nil {
			t.Fatalf("load persisted user data: %v", err)
		}
		if got := persisted.Accounts["auth:acct"].QuotaReserveEnabled; got != enabled {
			t.Fatalf("persisted QuotaReserveEnabled=%t after setting %t", got, enabled)
		}
	}

	if resp := reserveToggleManagementRequest(t, store, `{"auth_id":"acct","alias":"saved"}`, now); resp.StatusCode != http.StatusOK {
		t.Fatalf("annotation update omitting toggle status=%d body=%s", resp.StatusCode, resp.Body)
	}
	if store.Annotations().Accounts["auth:acct"].QuotaReserveEnabled {
		t.Fatal("omitting quota_reserve_enabled did not preserve disabled state")
	}
	if resp := reserveToggleManagementRequest(t, store, `{"auth_id":"acct","quota_reserve_enabled":true}`, now); resp.StatusCode != http.StatusOK {
		t.Fatalf("re-enable status=%d body=%s", resp.StatusCode, resp.Body)
	}
	if resp := reserveToggleManagementRequest(t, store, `{"auth_id":"acct","alias":"still enabled"}`, now); resp.StatusCode != http.StatusOK {
		t.Fatalf("annotation update omitting enabled toggle status=%d body=%s", resp.StatusCode, resp.Body)
	}
	if !store.Annotations().Accounts["auth:acct"].QuotaReserveEnabled {
		t.Fatal("omitting quota_reserve_enabled did not preserve enabled state")
	}
	exported := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementBasePath + "/export"}, now)
	if exported.StatusCode != http.StatusOK {
		t.Fatalf("export status=%d body=%s", exported.StatusCode, exported.Body)
	}
	restored := NewPluginState(DefaultConfig())
	imported := HandleManagementRequest(restored, pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementBasePath + "/import", Body: exported.Body}, now)
	if imported.StatusCode != http.StatusOK {
		t.Fatalf("import status=%d body=%s", imported.StatusCode, imported.Body)
	}
	if !restored.Annotations().Accounts["auth:acct"].QuotaReserveEnabled {
		t.Fatal("configuration export/import did not preserve the account toggle")
	}
}

func TestQuotaReserveStatusUsesAccountToggleForEffectiveValues(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	used := 40.0
	base := AccountState{
		Family: AccountFamilyWeekly,
		Quota: ParsedQuota{
			FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: now.Add(10 * time.Minute)},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &used, ResetAt: now.Add(30 * time.Minute)},
		},
	}
	disabled := base
	disabled.AuthID = "disabled"
	disabled.Annotation = AccountAnnotation{FiveHourReservePercent: 9, WeeklyReservePercent: 12,
		FiveHourReserveSteps: []QuotaReserveStep{{MinutesBeforeReset: 15, ReservePercent: 4}},
		WeeklyReserveSteps:   []QuotaReserveStep{{MinutesBeforeReset: 45, ReservePercent: 6}}}
	enabled := base
	enabled.AuthID = "enabled"
	enabled.Annotation = disabled.Annotation
	enabled.Annotation.QuotaReserveEnabled = true
	snapshot := StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{disabled, enabled}, Now: now}
	ordered := []ScheduledAccount{
		{AuthID: "disabled", Annotation: disabled.Annotation},
		{AuthID: "enabled", Annotation: enabled.Annotation},
	}
	payload := BuildStatusPayload(snapshot, ordered)
	if len(payload.Accounts) != 2 {
		t.Fatalf("status accounts=%#v", payload.Accounts)
	}
	if payload.Accounts[0].QuotaReserveEnabled || payload.Accounts[0].FiveHourEffectiveReserve != 0 || payload.Accounts[0].WeeklyEffectiveReserve != 0 {
		t.Fatalf("disabled account status=%#v", payload.Accounts[0])
	}
	if !payload.Accounts[1].QuotaReserveEnabled || payload.Accounts[1].FiveHourEffectiveReserve != 4 || payload.Accounts[1].WeeklyEffectiveReserve != 6 {
		t.Fatalf("enabled account status=%#v", payload.Accounts[1])
	}
}

func TestQuotaReserveManagementPageAndLogDiagnosticsAreReadable(t *testing.T) {
	resp := HandleManagementRequest(NewPluginState(DefaultConfig()), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/status"}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status page request status=%d body=%s", resp.StatusCode, resp.Body)
	}
	page := string(resp.Body)
	for _, want := range []string{
		"accountQuotaReserveEnabled", "quota_reserve_enabled", "启用此账号的额度保留保护",
		"Enable quota reserve protection", "fields.reason_text", "fields.quota_reserve_details",
		"logField.quota_reserve_details", "未开启（规则已保存）",
		"quotaReserveWindowSummary", "：当前 ", "：不限制（基础 0%）",
		"if(currentLocale!=='en')return dictionary[key]||log.message", "whiteSpace='pre-line'",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("status page is missing quota reserve UI contract %q", want)
		}
	}
}
