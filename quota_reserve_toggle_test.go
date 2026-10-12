package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestQuotaReserveAccountSwitchIsIndependentAndDefaultsOff(t *testing.T) {
	var legacy AccountAnnotation
	if err := json.Unmarshal([]byte(`{"five_hour_reserve_percent":10,"weekly_reserve_percent":10}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if hasQuotaReserve(legacy) || legacy.QuotaReserveEnabled {
		t.Fatal("saved legacy ratios must not implicitly enable protection")
	}
	previous := publishedSchedulerSnapshot.Load()
	t.Cleanup(func() { publishedSchedulerSnapshot.Store(previous) })
	now := time.Now()
	a := reserveTestAccount(now, 95, 20)
	b := reserveTestAccount(now, 95, 20)
	b.AuthID, b.Instance = "independent", 2
	b.Annotation.QuotaReserveEnabled = false
	state := StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{a, b},
		CPAAdmission: CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{a.AuthID: {}, b.AuthID: {}}}, Now: now}
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: a.AuthID, Provider: "codex"}, {ID: b.AuthID, Provider: "codex"}}}
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, nil))
	decision := schedulerPickPublished(req, now)
	if decision.AuthID != b.AuthID || decision.Reject {
		t.Fatalf("disabled account must remain selectable independently: %#v", decision)
	}
	if snapshotDecision := PickCodexAccount(req, state, now); snapshotDecision.AuthID != b.AuthID {
		t.Fatalf("snapshot path did not respect independent switches: %#v", snapshotDecision)
	}
	a.Annotation.QuotaReserveEnabled = false
	state.Accounts = []AccountState{a}
	req.Candidates = req.Candidates[:1]
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, nil))
	if d := schedulerPickPublished(req, now); d.AuthID != a.AuthID || d.Reject {
		t.Fatalf("switch off must stop quota exclusion: %#v", d)
	}
	a.Refresh.AuthFailure = true
	state.Accounts = []AccountState{a}
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, nil))
	if d := schedulerPickPublished(req, now); d.Reject || d.DelegateBuiltin == "" {
		t.Fatalf("switch off must not suppress normal fallback: %#v", d)
	}
	state.Accounts = nil
	state.Annotations = AnnotationState{Accounts: map[string]AccountAnnotation{"auth:" + a.AuthID: a.Annotation}}
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, nil))
	if d := schedulerPickPublished(req, now); d.Reject {
		t.Fatalf("disabled saved rules blocked unloaded-account fallback: %#v", d)
	}
	other := a.Annotation
	other.QuotaReserveEnabled = true
	state.Annotations.Accounts["auth:unrelated"] = other
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, nil))
	if d := schedulerPickPublished(req, now); d.Reject {
		t.Fatalf("unrelated enabled account blocked disabled candidate fallback: %#v", d)
	}
}

func TestQuotaReserveDifferentAccountStepsMatchTheirOwnLogs(t *testing.T) {
	previous := publishedSchedulerSnapshot.Load()
	t.Cleanup(func() { publishedSchedulerSnapshot.Store(previous) })
	now := time.Now()
	a := reserveTestAccount(now, 98, 20)
	a.Quota.FiveHour.ResetAt = now.Add(15 * time.Minute)
	b := reserveTestAccount(now, 98, 20)
	b.AuthID, b.Instance = "other-rule", 2
	b.Quota.FiveHour.ResetAt = now.Add(15 * time.Minute)
	b.Annotation.FiveHourReserveSteps = []QuotaReserveStep{{20, 1}}
	state := StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{a, b},
		CPAAdmission: CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{a.AuthID: {}, b.AuthID: {}}}, Now: now}
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: a.AuthID, Provider: "codex"}, {ID: b.AuthID, Provider: "codex"}}}
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, nil))
	d := schedulerPickPublished(req, now)
	if d.AuthID != b.AuthID || len(d.QuotaReserveDetails) != 2 {
		t.Fatalf("decision %#v", d)
	}
	if !strings.Contains(d.QuotaReserveDetails[0], "保留 3%") || !strings.Contains(d.QuotaReserveDetails[0], "暂停选择") {
		t.Fatal(d.QuotaReserveDetails[0])
	}
	if !strings.Contains(d.QuotaReserveDetails[1], "保留 1%") || !strings.Contains(d.QuotaReserveDetails[1], "未触发保留限制") {
		t.Fatal(d.QuotaReserveDetails[1])
	}
}

func TestQuotaReserveChineseExplanationCoversEffectiveAndInactiveRules(t *testing.T) {
	now := time.Now()
	a := reserveTestAccount(now, 98, 20)
	a.Quota.FiveHour.ResetAt = now.Add(15 * time.Minute)
	text := quotaReserveExplanation(accountReservationState(a), now)
	for _, wanted := range []string{"命中最后 20 分钟规则", "保留 3%", "剩余 2.00%", "暂停选择", "未进入任何临近重置时间规则", "未触发保留限制"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("missing %q in %s", wanted, text)
		}
	}
	*a.Quota.FiveHour.UsedPercent = 40
	text = quotaReserveExplanation(accountReservationState(a), now)
	if !strings.Contains(text, "剩余 60.00% > 生效保留 3%") {
		t.Fatal(text)
	}
	a.Annotation.QuotaReserveEnabled = false
	if text := quotaReserveExplanation(accountReservationState(a), now); !strings.Contains(text, "保留保护未开启") {
		t.Fatal(text)
	}
	a.Annotation.QuotaReserveEnabled = true
	a.LastSuccessAt = time.Time{}
	if text := quotaReserveExplanation(accountReservationState(a), now); !strings.Contains(text, "无法判定剩余比例") {
		t.Fatal(text)
	}
	a.LastSuccessAt = now
	a.Family = AccountFamilyMonthly
	a.Quota.LongWindow.Kind = WindowMonthly
	if text := quotaReserveExplanation(accountReservationState(a), now); !strings.Contains(text, "规则不适用") {
		t.Fatal(text)
	}
}

func TestQuotaReserveLogUsesPickTimeSnapshot(t *testing.T) {
	previous := publishedSchedulerSnapshot.Load()
	t.Cleanup(func() { publishedSchedulerSnapshot.Store(previous) })
	now := time.Now()
	a := reserveTestAccount(now, 98, 20)
	a.Quota.FiveHour.ResetAt = now.Add(15 * time.Minute)
	state := StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{a},
		CPAAdmission: CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{a.AuthID: {}}}, Now: now}
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: a.AuthID, Provider: "codex"}}}
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, nil))
	decision := schedulerPickPublished(req, now)
	if !decision.Reject || len(decision.QuotaReserveDetails) != 1 {
		t.Fatalf("decision %#v", decision)
	}
	a.Annotation.QuotaReserveEnabled = false
	state.Accounts[0] = a
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, nil))
	store := NewPluginState(DefaultConfig())
	logSchedulerDecision(store, req, decision, now.Add(10*time.Minute))
	entry := store.Snapshot(now.Add(10 * time.Minute)).Logs[0]
	if !strings.Contains(entry.Message, "额度保留保护已生效") {
		t.Fatal(entry.Message)
	}
	if !strings.Contains(entry.Fields["reason_text"].(string), "禁止内置调度回退") {
		t.Fatal(entry.Fields)
	}
	details := entry.Fields["quota_reserve_details"].([]string)
	if !strings.Contains(details[0], "保留 3%") || strings.Contains(details[0], "保护未开启") {
		t.Fatalf("async log used a newer configuration/time: %s", details[0])
	}
}
