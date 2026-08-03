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
	mu       sync.Mutex
	quota    [][]byte
	requests []pluginapi.HTTPRequest
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
	defer h.mu.Unlock()
	h.requests = append(h.requests, req)
	if req.URL == codexResetProbeEndpoint {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{"total_tokens":1}}`)}, nil
	}
	if len(h.quota) == 0 {
		return pluginapi.HTTPResponse{}, errors.New("weekly activation test quota exhausted")
	}
	body := h.quota[0]
	h.quota = h.quota[1:]
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
