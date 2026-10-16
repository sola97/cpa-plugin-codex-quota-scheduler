package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func reserveTestAccount(now time.Time, fiveHourUsed, weeklyUsed float64) AccountState {
	return AccountState{
		AuthID: "protected", Provider: "codex", Instance: 1, Family: AccountFamilyWeekly,
		LastSuccessAt: now,
		Quota: ParsedQuota{Family: AccountFamilyWeekly,
			FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &fiveHourUsed, ResetAt: now.Add(2 * time.Hour)},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &weeklyUsed, ResetAt: now.Add(24 * time.Hour)},
		},
		Annotation: AccountAnnotation{QuotaReserveEnabled: true, FiveHourReservePercent: 10, WeeklyReservePercent: 10},
	}
}

func TestQuotaReserveIndependentWindows(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name             string
		fiveHour, weekly float64
		want             string
	}{
		{"below", 89.9, 89.9, ""},
		{"five_hour_boundary", 90, 20, "five_hour_reserved"},
		{"five_hour_above", 90.1, 20, "five_hour_reserved"},
		{"weekly_boundary", 20, 90, "weekly_reserved"},
		{"weekly_above", 20, 90.1, "weekly_reserved"},
		{"both", 90, 90, "five_hour_reserved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := reserveTestAccount(now, tc.fiveHour, tc.weekly)
			if got := accountReservationReason(a, now); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
			if got := ClassifyAccount(accountViewFromState(a, DefaultConfig(), now, nil), now); (got == Excluded) != (tc.want != "") {
				t.Fatalf("class = %v, want excluded=%v", got, tc.want != "")
			}
			_, available, reason, _ := accountQueueState(a, now)
			if available != (tc.want == "") || reason != tc.want {
				t.Fatalf("queue available=%v reason=%q", available, reason)
			}
		})
	}
}

func TestQuotaReserveDefaultsAndCustomTimeSteps(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		kind  WindowKind
		until time.Duration
		want  int
	}{
		{WindowFiveHour, 20*time.Minute + time.Nanosecond, 10},
		{WindowFiveHour, 20 * time.Minute, 3},
		{WindowFiveHour, 10*time.Minute + time.Nanosecond, 3},
		{WindowFiveHour, 10 * time.Minute, 1},
		{WindowWeekly, time.Hour + time.Nanosecond, 10},
		{WindowWeekly, time.Hour, 10},
		{WindowWeekly, 30 * time.Minute, 5},
		{WindowWeekly, 15 * time.Minute, 3},
		{WindowWeekly, 5 * time.Minute, 1},
		{WindowWeekly, time.Nanosecond, 1},
		{WindowWeekly, 0, 10},
	} {
		window := &QuotaWindow{Kind: tc.kind, ResetAt: now.Add(tc.until)}
		if got := effectiveQuotaReserve(10, nil, tc.kind, window, now); got != tc.want {
			t.Errorf("%s until=%v: got %d want %d", tc.kind, tc.until, got, tc.want)
		}
	}
	custom := []QuotaReserveStep{{5, 2}, {40, 8}, {15, 4}}
	window := &QuotaWindow{ResetAt: now.Add(15 * time.Minute)}
	if got := effectiveQuotaReserve(20, custom, WindowFiveHour, window, now); got != 4 {
		t.Fatalf("unordered custom steps: got %d, want 4", got)
	}
	if got := effectiveQuotaReserve(2, custom, WindowFiveHour, window, now); got != 4 {
		t.Fatalf("configured step should replace base: got %d, want 4", got)
	}
	if got := effectiveQuotaReserve(20, []QuotaReserveStep{}, WindowFiveHour, window, now); got != 20 {
		t.Fatalf("empty steps should retain fixed base: got %d", got)
	}
	if got := effectiveQuotaReserve(0, custom, WindowFiveHour, window, now); got != 0 {
		t.Fatalf("disabled window should remain disabled: got %d", got)
	}
}

func TestQuotaReserveTransitionsWithoutRepublishing(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	a := reserveTestAccount(now, 95, 20)
	a.Quota.FiveHour.ResetAt = now.Add(21 * time.Minute)
	view := accountViewFromState(a, DefaultConfig(), now, nil)
	if got := ClassifyAccount(view, now); got != Excluded {
		t.Fatalf("before 20m step class=%v", got)
	}
	if got := ClassifyAccount(view, now.Add(time.Minute)); got != Preferred {
		t.Fatalf("within 20m step class=%v, want Preferred", got)
	}
	*a.Quota.FiveHour.UsedPercent = 98
	view = accountViewFromState(a, DefaultConfig(), now, nil)
	if got := ClassifyAccount(view, now.Add(10*time.Minute)); got != Excluded {
		t.Fatalf("11m remaining should reserve 3%%, class=%v", got)
	}
	if got := ClassifyAccount(view, now.Add(11*time.Minute)); got != Preferred {
		t.Fatalf("10m remaining should reserve 1%%, class=%v", got)
	}
	if got := ClassifyAccount(view, a.Quota.FiveHour.ResetAt); got != Excluded {
		t.Fatalf("reset without new evidence should not release block, class=%v", got)
	}
	*a.Quota.FiveHour.UsedPercent = 0
	a.Quota.FiveHour.ResetAt = now.Add(5 * time.Hour)
	if got := ClassifyAccount(accountViewFromState(a, DefaultConfig(), now, nil), now); got != Preferred {
		t.Fatalf("new quota evidence should release block, class=%v", got)
	}
}

func TestQuotaReserveMissingUsageAndMonthlySemantics(t *testing.T) {
	now := time.Now()
	a := reserveTestAccount(now, 99, 99)
	a.Annotation.FiveHourReservePercent = 0
	a.Annotation.WeeklyReservePercent = 0
	if got := accountReservationReason(a, now); got != "" {
		t.Fatalf("zero reserves changed legacy behavior: %q", got)
	}
	a.Annotation.FiveHourReservePercent = 100
	*a.Quota.FiveHour.UsedPercent = 0
	if got := accountReservationReason(a, now); got != "five_hour_reserved" {
		t.Fatalf("100%% reserve should block unused quota: %q", got)
	}
	a.Annotation.FiveHourReservePercent = 10
	a.Quota.FiveHour.UsedPercent = nil
	if got := accountReservationReason(a, now); got != "reserve_quota_unknown" {
		t.Fatalf("missing existing window usage should wait: %q", got)
	}
	a.Quota.FiveHour = nil
	if got := accountReservationReason(a, now); got != "" {
		t.Fatalf("successful read with no 5h window should retain upstream behavior: %q", got)
	}
	a.Annotation.WeeklyReservePercent = 10
	a.Family, a.Quota.LongWindow.Kind = AccountFamilyMonthly, WindowMonthly
	if got := accountReservationReason(a, now); got != "" {
		t.Fatalf("weekly rule must not reserve monthly quota: %q", got)
	}
	a.LastSuccessAt = time.Time{}
	if got := accountReservationReason(a, now); got != "reserve_quota_unknown" {
		t.Fatalf("unverified account should wait: %q", got)
	}
}

func TestQuotaReservePublishedPickCannotFallback(t *testing.T) {
	previous := publishedSchedulerSnapshot.Load()
	t.Cleanup(func() { publishedSchedulerSnapshot.Store(previous) })
	now := time.Now()
	a := reserveTestAccount(now, 90, 20)
	state := StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{a}, CPAAdmission: CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{a.AuthID: {}}}, Now: now}
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: a.AuthID, Provider: "codex"}}}
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, NewTrialRegistry()))
	decision := schedulerPickPublished(req, now)
	if !decision.Reject || decision.DelegateBuiltin != "" || decision.AuthID != "" {
		t.Fatalf("protected pick bypasses reserve: %#v", decision)
	}
	if decision := PickCodexAccount(req, state, now); !decision.Reject || decision.DelegateBuiltin != "" {
		t.Fatalf("snapshot pick bypasses reserve: %#v", decision)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	response, err := handleSchedulerPick(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(response, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "usage_limit_reached" || env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("ABI must reject rather than return an empty success: %s", response)
	}
	var body struct {
		Error struct{ Message, Type, Code string }
	}
	if json.Unmarshal([]byte(env.Error.Message), &body) != nil || body.Error.Message != "You've hit your 5-hour usage limit." || body.Error.Type != "usage_limit_reached" || body.Error.Code != "usage_limit_reached" {
		t.Fatalf("downstream usage-limit payload was not preserved: %s", response)
	}
	// Even before the roster/quota loads, saved protection forbids fallback.
	state.Accounts = nil
	state.Annotations = AnnotationState{Accounts: map[string]AccountAnnotation{"auth:protected": a.Annotation}}
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, NewTrialRegistry()))
	if decision := schedulerPickPublished(req, now); !decision.Reject {
		t.Fatalf("unloaded protected account bypasses reserve: %#v", decision)
	}
	response, err = handleSchedulerPick(raw)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(response, &env) != nil || env.Error == nil || env.Error.Code != "quota_not_ready" || env.Error.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("missing refresh service must not report exhausted quota: %s", response)
	}
	state.Annotations = AnnotationState{Accounts: map[string]AccountAnnotation{"instance:1": a.Annotation}}
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, NewTrialRegistry()))
	if decision := schedulerPickPublished(req, now); !decision.Reject {
		t.Fatalf("unhydrated instance-keyed reserve bypasses protection: %#v", decision)
	}
	// A usable lower-tier account still wins when cross-priority admission is enabled.
	b := reserveTestAccount(now, 20, 20)
	b.AuthID, b.Instance, b.Priority = "available", 2, -1
	state.Config.ScheduleAcrossPriorities = true
	state.Accounts = []AccountState{a, b}
	state.CPAAdmission.AuthIDs[b.AuthID] = struct{}{}
	req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: b.AuthID, Provider: "codex", Priority: -1})
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, NewTrialRegistry()))
	if decision := schedulerPickPublished(req, now); decision.AuthID != b.AuthID || decision.Reject {
		t.Fatalf("usable alternative must be selected: %#v", decision)
	}
}

func TestQuotaReservePersistenceAndSnapshotIsolation(t *testing.T) {
	previous := publishedSchedulerSnapshot.Load()
	t.Cleanup(func() { publishedSchedulerSnapshot.Store(previous) })
	now := time.Now()
	a := reserveTestAccount(now, 92, 20)
	a.Annotation.FiveHourReserveSteps = []QuotaReserveStep{{25, 6}, {7, 2}}
	a.Annotation.WeeklyReserveSteps = []QuotaReserveStep{}
	state := PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"auth:protected": a.Annotation}}
	path := filepath.Join(t.TempDir(), "user-data.json")
	if err := SaveUserData(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := readUserData(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Accounts["auth:protected"], a.Annotation) {
		t.Fatalf("annotation lost independent settings or empty steps: %#v", loaded.Accounts)
	}
	snapshot := schedulerSnapshotFromState(StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{a}, Now: now}, nil)
	PublishSchedulerSnapshot(snapshot)
	*a.Quota.FiveHour.UsedPercent = 0
	a.Annotation.FiveHourReserveSteps[0].ReservePercent = 0
	view := publishedSchedulerSnapshot.Load().Accounts[0]
	if reason := reservationReason(view.Reservation, now); reason != "five_hour_reserved" {
		t.Fatalf("published quota changed through caller pointers: %q", reason)
	}
	if view.Reservation.Annotation.FiveHourReserveSteps[0].ReservePercent != 6 {
		t.Fatal("published step rules changed through caller slice")
	}
}

func TestQuotaReserveWeeklyStepsDoNotBypassFiveHourReserve(t *testing.T) {
	now := time.Now()
	a := reserveTestAccount(now, 92, 96)
	a.Quota.LongWindow.ResetAt = now.Add(30 * time.Minute)
	if got := accountReservationReason(a, now); got != "five_hour_reserved" {
		t.Fatalf("weekly relaxation bypassed 5h reserve: %q", got)
	}
	*a.Quota.FiveHour.UsedPercent = 20
	if got := accountReservationReason(a, now); got != "weekly_reserved" {
		t.Fatalf("30m weekly reserve must still block 4%% remaining: %q", got)
	}
	if got := accountReservationReason(a, now.Add(15*time.Minute)); got != "" {
		t.Fatalf("15m weekly rule should allow 4%% remaining: %q", got)
	}
	*a.Quota.LongWindow.UsedPercent = 98
	if got := accountReservationReason(a, now.Add(15*time.Minute)); got != "weekly_reserved" {
		t.Fatalf("15m weekly rule must block 2%% remaining: %q", got)
	}
	if got := accountReservationReason(a, now.Add(25*time.Minute)); got != "" {
		t.Fatalf("5m weekly rule should allow 2%% remaining: %q", got)
	}
}

func TestQuotaReserveValidation(t *testing.T) {
	for _, annotation := range []AccountAnnotation{
		{FiveHourReservePercent: -1}, {FiveHourReservePercent: 101},
		{WeeklyReservePercent: -1}, {WeeklyReservePercent: 101},
		{FiveHourReserveSteps: []QuotaReserveStep{{0, 3}}},
		{FiveHourReserveSteps: []QuotaReserveStep{{301, 3}}},
		{WeeklyReserveSteps: []QuotaReserveStep{{10081, 3}}},
		{WeeklyReserveSteps: []QuotaReserveStep{{20, 101}}},
		{WeeklyReserveSteps: []QuotaReserveStep{{20, -1}}},
		{FiveHourReserveSteps: []QuotaReserveStep{{20, 3}, {20, 1}}},
	} {
		if err := validateQuotaReserves(map[string]AccountAnnotation{"auth:a": annotation}); err == nil {
			t.Errorf("invalid annotation accepted: %#v", annotation)
		}
	}
	if err := validateQuotaReserves(map[string]AccountAnnotation{"auth:a": {FiveHourReservePercent: 100, WeeklyReservePercent: 0, FiveHourReserveSteps: []QuotaReserveStep{{300, 0}, {1, 100}}, WeeklyReserveSteps: []QuotaReserveStep{{10080, 100}, {1, 0}}}}); err != nil {
		t.Fatalf("valid edge settings rejected: %v", err)
	}
}

func TestQuotaReserveNormalFallbackAndDisabledScheduling(t *testing.T) {
	previous := publishedSchedulerSnapshot.Load()
	t.Cleanup(func() { publishedSchedulerSnapshot.Store(previous) })
	now := time.Now()
	a := reserveTestAccount(now, 90, 20)
	state := StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{a}, CPAAdmission: CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{a.AuthID: {}}}, Now: now}
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: a.AuthID, Provider: "codex"}}}
	state.Config.HandleEnabled = false
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, nil))
	if decision := schedulerPickPublished(req, now); decision.Handled || decision.Reject {
		t.Fatalf("disabled scheduling should not enforce reserve: %#v", decision)
	}
	state.Config.HandleEnabled = true
	a.Annotation = AccountAnnotation{}
	a.Refresh.AuthFailure = true
	state.Accounts = []AccountState{a}
	PublishSchedulerSnapshot(schedulerSnapshotFromState(state, nil))
	if decision := schedulerPickPublished(req, now); decision.Reject || decision.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("unconfigured requests must retain upstream fallback: %#v", decision)
	}
}
