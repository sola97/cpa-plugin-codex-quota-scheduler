package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type weeklyActivationTestHost struct {
	mu            sync.Mutex
	quota         [][]byte
	probeResponse []byte
	probeStatus   int
	probeStarted  chan struct{}
	releaseProbe  chan struct{}
	requests      []pluginapi.HTTPRequest
}

func (h *weeklyActivationTestHost) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	return []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: 9}}, nil
}

func (h *weeklyActivationTestHost) GetAuth(string) (pluginapi.HostAuthGetResponse, error) {
	return pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"refresh","account_id":"acct"}`)}, nil
}

func (h *weeklyActivationTestHost) SaveAuth(string, json.RawMessage) error { return nil }
func (h *weeklyActivationTestHost) Log(string, string, map[string]any)     {}

func (h *weeklyActivationTestHost) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.mu.Lock()
	h.requests = append(h.requests, req)
	probeResponse := h.probeResponse
	probeStatus := h.probeStatus
	probeStarted := h.probeStarted
	releaseProbe := h.releaseProbe
	if req.URL == codexResetProbeEndpoint {
		h.mu.Unlock()
		if probeStarted != nil {
			select {
			case <-probeStarted:
			default:
				close(probeStarted)
			}
		}
		if releaseProbe != nil {
			<-releaseProbe
		}
		if probeResponse == nil {
			probeResponse = []byte(`{"usage":{"total_tokens":1}}`)
		}
		if probeStatus == 0 {
			probeStatus = http.StatusOK
		}
		return pluginapi.HTTPResponse{StatusCode: probeStatus, Body: probeResponse}, nil
	}
	if len(h.quota) == 0 {
		h.mu.Unlock()
		return pluginapi.HTTPResponse{}, errors.New("weekly activation test quota exhausted")
	}
	body := h.quota[0]
	h.quota = h.quota[1:]
	h.mu.Unlock()
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}, nil
}

func weeklyQuotaForTest(now time.Time, used float64, weeklyOffset time.Duration) ParsedQuota {
	return ParsedQuota{
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: now.Add(5 * time.Hour)},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(7*24*time.Hour + weeklyOffset)},
	}
}

func TestWeeklyActivationEligibilityRequiresPrimaryAndAlignedWeeklyQuota(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		quota ParsedQuota
		want  bool
	}{
		"aligned and unused":    {quota: weeklyQuotaForTest(now, 0, 2*time.Minute), want: true},
		"outside tolerance":     {quota: weeklyQuotaForTest(now, 0, 2*time.Minute+time.Second), want: false},
		"primary already used":  {quota: weeklyQuotaForTest(now, 0.01, 0), want: false},
		"missing primary usage": {quota: ParsedQuota{LongWindow: weeklyQuotaForTest(now, 0, 0).LongWindow}, want: false},
		"monthly long window":   {quota: ParsedQuota{FiveHour: weeklyQuotaForTest(now, 0, 0).FiveHour, LongWindow: &QuotaWindow{Kind: WindowMonthly, ResetAt: now.Add(7 * 24 * time.Hour)}}, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := weeklyActivationEligible(test.quota, now); got != test.want {
				t.Fatalf("weeklyActivationEligible() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWeeklyActivationStateStopsOnMismatchAndManualRefreshRearms(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	valid := weeklyQuotaForTest(now, 0, 0)
	ready, keep := deriveWeeklyActivationState(WeeklyActivationProbeState{}, false, valid, now, false)
	if !keep || ready.State != WeeklyActivationReady || !ready.NextCheckAt.Equal(now) {
		t.Fatalf("initial state = %#v, keep=%v, want ready at now", ready, keep)
	}

	mismatch := weeklyQuotaForTest(now, 0, 3*time.Minute)
	manual, keep := deriveWeeklyActivationState(ready, true, mismatch, now, false)
	if !keep || manual.State != WeeklyActivationManualRefreshRequired {
		t.Fatalf("mismatch state = %#v, keep=%v, want manual refresh required", manual, keep)
	}
	stillManual, keep := deriveWeeklyActivationState(manual, true, valid, now, false)
	if !keep || stillManual.State != WeeklyActivationManualRefreshRequired {
		t.Fatalf("automatic recheck state = %#v, keep=%v, want manual hold", stillManual, keep)
	}
	rearmed, keep := deriveWeeklyActivationState(stillManual, true, valid, now, true)
	if !keep || rearmed.State != WeeklyActivationReady || !rearmed.NextCheckAt.Equal(now) {
		t.Fatalf("manual refresh state = %#v, keep=%v, want ready at now", rearmed, keep)
	}
}

func TestWeeklyActivationConfirmedSameCycleDoesNotResend(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(7 * 24 * time.Hour)
	confirmed := WeeklyActivationProbeState{
		State:         WeeklyActivationConfirmed,
		WeeklyResetAt: resetAt,
		LastProbeAt:   now,
	}
	quota := weeklyQuotaForTest(now.Add(10*time.Minute), 0.5, -10*time.Minute)
	quota.LongWindow.ResetAt = resetAt
	next, keep := deriveWeeklyActivationState(confirmed, true, quota, now.Add(10*time.Minute), false)
	if !keep || next.State != WeeklyActivationConfirmed || !next.WeeklyResetAt.Equal(resetAt) {
		t.Fatalf("confirmed state = %#v, keep=%v, want same-cycle confirmation preserved", next, keep)
	}
}

func TestWeeklyActivationConfirmedNewCycleRequiresManualRefresh(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	confirmed := WeeklyActivationProbeState{
		State:         WeeklyActivationConfirmed,
		WeeklyResetAt: now.Add(6 * 24 * time.Hour),
	}
	newCycle := weeklyQuotaForTest(now, 0, 0)
	next, keep := deriveWeeklyActivationState(confirmed, true, newCycle, now, false)
	if !keep || next.State != WeeklyActivationManualRefreshRequired {
		t.Fatalf("new cycle state = %#v, keep=%v, want manual refresh required", next, keep)
	}
}

func TestWeeklyActivationConfigAndPayload(t *testing.T) {
	if DefaultConfig().EnableWeeklyActivationProbe {
		t.Fatal("weekly activation probe is enabled by default")
	}
	cfg, err := DecodeConfig([]byte("enable_weekly_activation_probe: true\n"))
	if err != nil {
		t.Fatalf("DecodeConfig() error = %v", err)
	}
	if !cfg.EnableWeeklyActivationProbe {
		t.Fatal("decoded weekly activation probe flag = false, want true")
	}
	settings := SettingsFromConfig(cfg)
	if !settings.EnableWeeklyActivationProbe {
		t.Fatal("settings payload weekly activation flag = false, want true")
	}
	roundTrip, err := ConfigFromSettings(DefaultConfig(), settings)
	if err != nil {
		t.Fatalf("ConfigFromSettings() error = %v", err)
	}
	if !roundTrip.EnableWeeklyActivationProbe {
		t.Fatal("settings round-trip weekly activation flag = false, want true")
	}
	html := string(RenderStatusHTML(StatusPayload{Settings: settings}))
	if !strings.Contains(html, `id="enableWeeklyActivationProbe"`) || !strings.Contains(html, "enable_weekly_activation_probe") {
		t.Fatalf("status HTML does not expose weekly activation setting: %s", html)
	}
	payload := string(weeklyActivationProbePayloadBytes())
	if !strings.Contains(payload, `"text":"你好"`) {
		t.Fatalf("activation payload = %s, want Chinese greeting", payload)
	}
	if strings.Contains(payload, `"text":"ping"`) {
		t.Fatalf("activation payload unexpectedly contains legacy ping: %s", payload)
	}
}

func TestWeeklyActivationNextDeadlineIncludesFutureRetry(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableWeeklyActivationProbe = true
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	if _, err := store.Update(func(persisted *PersistentState) error {
		persisted.WeeklyActivationProbes[1] = WeeklyActivationProbeState{
			State:       WeeklyActivationRetryWait,
			NextCheckAt: now.Add(time.Minute),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := &QuotaRefresher{runtimeStore: store, state: NewPluginState(cfg), now: func() time.Time { return now }}
	if got, want := r.weeklyActivationNextDeadline(), now.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("weekly activation deadline = %s, want %s", got, want)
	}
}

func TestScheduledWeeklyActivationRecoveryRunsWhenVerifyBecomesDue(t *testing.T) {
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	current := start
	quota := func(now time.Time, used float64) []byte {
		return []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%.2f,"limit_window_seconds":18000,"reset_at":%q},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, used, now.Add(5*time.Hour).Format(time.RFC3339), now.Add(7*24*time.Hour).Format(time.RFC3339)))
	}
	host := &weeklyActivationTestHost{}
	cfg := DefaultConfig()
	cfg.EnableWeeklyActivationProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	instance := legacyAuthInstanceID("a")
	used := 0.0
	priority := 9
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Instance: instance, Quota: ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: start.Add(5 * time.Hour)},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: start.Add(7 * 24 * time.Hour)},
	}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: &priority}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return current })
	if err != nil {
		t.Fatalf("NewProductionQuotaRefresher() error = %v", err)
	}
	t.Cleanup(r.coordinator.Close)
	adapter.bindings = r.bindings
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	if _, _, err = r.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatalf("BootstrapBinding() error = %v", err)
	}
	if _, err = r.probeFence.Next(); err != nil {
		t.Fatalf("probeFence.Next() error = %v", err)
	}
	sentAt := start
	if _, err = r.runtimeStore.Update(func(persisted *PersistentState) error {
		persisted.WeeklyActivationProbes[instance] = WeeklyActivationProbeState{
			State:         WeeklyActivationCooldown,
			WeeklyResetAt: start.Add(7 * 24 * time.Hour),
			CooldownUntil: start.Add(weeklyActivationCooldownTime),
		}
		persisted.ProbeAttempts[instance] = ProbeAttempt{
			Instance:        instance,
			AttemptID:       "weekly-recover",
			Purpose:         ProbePurposeWeeklyActivation,
			Phase:           ProbeAttemptSentUnknown,
			SendFenceSeq:    1,
			CreatedAt:       start,
			SentAt:          &sentAt,
			VerifyNotBefore: start.Add(3 * time.Second),
			SuppressUntil:   start.Add(weeklyActivationCooldownTime),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = r.RunProbeRecoveryOnce(context.Background()); err != nil {
		t.Fatalf("RunProbeRecoveryOnce() before verify deadline error = %v", err)
	}
	host.mu.Lock()
	requestCount := len(host.requests)
	host.mu.Unlock()
	if requestCount != 0 {
		t.Fatalf("recovery before verify deadline made %d requests", requestCount)
	}
	current = start.Add(4 * time.Second)
	host.mu.Lock()
	host.quota = append(host.quota, quota(start, 0.5))
	host.mu.Unlock()
	r.launchProbe()
	r.wg.Wait()

	activation, ok := r.weeklyActivationState(instance)
	if !ok || activation.State != WeeklyActivationConfirmed {
		t.Fatalf("activation state = %#v, ok=%v, want confirmed after scheduled recovery", activation, ok)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := persisted.ProbeAttempts[instance]; exists {
		t.Fatalf("scheduled recovery left weekly WAL attempt: %#v", persisted.ProbeAttempts[instance])
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != 1 || host.requests[0].URL != cfg.QuotaEndpoint {
		t.Fatalf("scheduled recovery requests = %#v, want one quota verify GET", host.requests)
	}
}

func TestWeeklyActivationNoEvidenceKeepsTenMinuteSuppression(t *testing.T) {
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	current := start
	quota := func(used float64) []byte {
		return []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%.2f,"limit_window_seconds":18000,"reset_at":%q},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, used, start.Add(5*time.Hour).Format(time.RFC3339), start.Add(7*24*time.Hour).Format(time.RFC3339)))
	}
	host := &weeklyActivationTestHost{
		quota:         [][]byte{quota(0), quota(0), quota(0), quota(0)},
		probeResponse: []byte(`{"usage":{}}`),
	}
	cfg := DefaultConfig()
	cfg.EnableWeeklyActivationProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	instance := legacyAuthInstanceID("a")
	used := 0.0
	priority := 9
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Instance: instance, Quota: ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: start.Add(5 * time.Hour)},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: start.Add(7 * 24 * time.Hour)},
	}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: &priority}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return current })
	if err != nil {
		t.Fatalf("NewProductionQuotaRefresher() error = %v", err)
	}
	t.Cleanup(r.coordinator.Close)
	adapter.bindings = r.bindings
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	if _, _, err = r.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatalf("BootstrapBinding() error = %v", err)
	}
	if err = r.RunProbeDueOnce(context.Background()); err == nil {
		t.Fatal("RunProbeDueOnce() succeeded without usage evidence")
	}
	activation, ok := r.weeklyActivationState(instance)
	if !ok || activation.State != WeeklyActivationRetryWait {
		t.Fatalf("activation state = %#v, ok=%v, want retry wait", activation, ok)
	}
	if want := start.Add(weeklyActivationCooldownTime); !activation.NextCheckAt.Equal(want) {
		t.Fatalf("retry deadline = %s, want suppression deadline %s", activation.NextCheckAt, want)
	}
	host.mu.Lock()
	requestCount := len(host.requests)
	host.mu.Unlock()
	if requestCount != 3 {
		t.Fatalf("initial no-evidence attempt made %d requests, want precheck, POST, verify", requestCount)
	}
	current = start.Add(time.Minute)
	if err = r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatalf("RunProbeDueOnce() during suppression error = %v", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != requestCount {
		t.Fatalf("suppressed pass made additional requests: %#v", host.requests)
	}
}

func TestWeeklyActivationSentFailureDefersVerifyRecovery(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	quota := func(used float64) []byte {
		return []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%.2f,"limit_window_seconds":18000,"reset_at":%q},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, used, now.Add(5*time.Hour).Format(time.RFC3339), now.Add(7*24*time.Hour).Format(time.RFC3339)))
	}
	host := &weeklyActivationTestHost{quota: [][]byte{quota(0)}, probeStatus: http.StatusInternalServerError}
	cfg := DefaultConfig()
	cfg.EnableWeeklyActivationProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	instance := legacyAuthInstanceID("a")
	used := 0.0
	priority := 9
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Instance: instance, Quota: ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: now.Add(5 * time.Hour)},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(7 * 24 * time.Hour)},
	}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: &priority}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewProductionQuotaRefresher() error = %v", err)
	}
	t.Cleanup(r.coordinator.Close)
	adapter.bindings = r.bindings
	if _, _, err = r.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatalf("BootstrapBinding() error = %v", err)
	}
	if err = r.RunProbeDueOnce(context.Background()); err == nil {
		t.Fatal("RunProbeDueOnce() succeeded after failed activation POST")
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	attempt, ok := persisted.ProbeAttempts[instance]
	if !ok || attempt.Phase != ProbeAttemptSentUnknown {
		t.Fatalf("failed POST attempt = %#v, ok=%v, want sent unknown", attempt, ok)
	}
	if want := now.Add(probeBackoff(1)); !attempt.VerifyNotBefore.Equal(want) {
		t.Fatalf("verify recovery deadline = %s, want %s", attempt.VerifyNotBefore, want)
	}
	if got := r.weeklyActivationNextDeadline(); !got.Equal(attempt.VerifyNotBefore) {
		t.Fatalf("next scheduler deadline = %s, want durable verify deadline %s", got, attempt.VerifyNotBefore)
	}
}

func TestStaleWeeklyActivationResultKeepsSentAttemptRecoverable(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	quota := func(used float64) []byte {
		return []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%.2f,"limit_window_seconds":18000,"reset_at":%q},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, used, now.Add(5*time.Hour).Format(time.RFC3339), now.Add(7*24*time.Hour).Format(time.RFC3339)))
	}
	host := &weeklyActivationTestHost{
		quota:        [][]byte{quota(0), quota(0.5)},
		probeStarted: make(chan struct{}),
		releaseProbe: make(chan struct{}),
	}
	cfg := DefaultConfig()
	cfg.EnableWeeklyActivationProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	instance := legacyAuthInstanceID("a")
	used := 0.0
	priority := 9
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Instance: instance, Quota: ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: now.Add(5 * time.Hour)},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(7 * 24 * time.Hour)},
	}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: &priority}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewProductionQuotaRefresher() error = %v", err)
	}
	t.Cleanup(r.coordinator.Close)
	adapter.bindings = r.bindings
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	if _, _, err = r.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatalf("BootstrapBinding() error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- r.RunProbeDueOnce(context.Background())
	}()
	<-host.probeStarted
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("weekly activation binding is missing")
	}
	if err = r.bindings.ObserveExternalLogin("a", binding.Login+1, binding.Fingerprint); err != nil {
		t.Fatalf("ObserveExternalLogin() error = %v", err)
	}
	close(host.releaseProbe)
	if err = <-done; err != nil {
		t.Fatalf("RunProbeDueOnce() error = %v", err)
	}

	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	attempt, exists := persisted.ProbeAttempts[instance]
	if !exists || attempt.Phase != ProbeAttemptSentUnknown {
		t.Fatalf("stale result probe attempt = %#v, exists=%v, want sent unknown recovery record", attempt, exists)
	}
	activation, ok := r.weeklyActivationState(instance)
	if !ok || activation.State != WeeklyActivationReady {
		t.Fatalf("stale result activation state = %#v, ok=%v, want untouched ready state", activation, ok)
	}
	snapshot := state.Snapshot(now)
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].Quota.FiveHour == nil || snapshot.Accounts[0].Quota.FiveHour.UsedPercent == nil || *snapshot.Accounts[0].Quota.FiveHour.UsedPercent != 0 {
		t.Fatalf("stale result changed plugin quota state: %#v", snapshot.Accounts)
	}
}

func TestProductionWeeklyActivationProbeSendsChineseGreetingOnce(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	quota := func(used float64) []byte {
		return []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%.2f,"limit_window_seconds":18000,"reset_at":%q},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, used, now.Add(5*time.Hour).Format(time.RFC3339), now.Add(7*24*time.Hour).Format(time.RFC3339)))
	}
	host := &weeklyActivationTestHost{quota: [][]byte{quota(0), quota(0.5)}}
	cfg := DefaultConfig()
	cfg.EnableWeeklyActivationProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	instance := legacyAuthInstanceID("a")
	used := 0.0
	priority := 9
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Instance: instance, Quota: ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: now.Add(5 * time.Hour)},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(7 * 24 * time.Hour)},
	}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: &priority}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewProductionQuotaRefresher() error = %v", err)
	}
	adapter.bindings = r.bindings
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	if _, _, err = r.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatalf("BootstrapBinding() error = %v", err)
	}
	if err = r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatalf("RunProbeDueOnce() error = %v", err)
	}
	activation, ok := r.weeklyActivationState(instance)
	if !ok || activation.State != WeeklyActivationConfirmed {
		t.Fatalf("activation state = %#v, ok=%v, want confirmed", activation, ok)
	}
	snapshot := state.Snapshot(now)
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].Quota.FiveHour == nil || snapshot.Accounts[0].Quota.FiveHour.UsedPercent == nil || *snapshot.Accounts[0].Quota.FiveHour.UsedPercent != 0.5 {
		t.Fatalf("verified quota was not written back: %#v", snapshot.Accounts)
	}
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 3 || requests[0].URL != cfg.QuotaEndpoint || requests[1].URL != codexResetProbeEndpoint || requests[2].URL != cfg.QuotaEndpoint {
		t.Fatalf("request sequence = %#v, want quota GET, activation POST, quota GET", requests)
	}
	if !strings.Contains(string(requests[1].Body), `"text":"你好"`) {
		t.Fatalf("activation request body = %s, want Chinese greeting", requests[1].Body)
	}
	if err = r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatalf("second RunProbeDueOnce() error = %v", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != 3 {
		t.Fatalf("second due pass sent another request: %d requests", len(host.requests))
	}
}
