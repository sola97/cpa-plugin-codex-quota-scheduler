package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestObservedQuotaIndependentWindows(t *testing.T) {
	now := time.Now()
	for _, transport := range []string{"sse", "headers"} {
		for _, test := range []struct {
			name         string
			five, weekly float64
			limited      bool
			want         string
		}{
			{"weekly_reserve", 21, 98, false, "weekly_reserved"},
			{"five_hour_reserve", 88, 21, false, "five_hour_reserved"},
			{"weekly_exhausted", 21, 100, true, "weekly_exhausted"},
			{"five_hour_exhausted", 100, 21, true, "five_hour_exhausted"},
		} {
			t.Run(transport+"/"+test.name, func(t *testing.T) {
				store := NewPluginState(DefaultConfig())
				account := reserveTestAccount(now, 20, 20)
				account.Annotation.FiveHourReservePercent = 12
				account.Annotation.WeeklyReservePercent = 2
				if test.limited {
					account.Annotation.QuotaReserveEnabled = false
				}
				store.UpsertQuota(account)
				if transport == "sse" {
					frame := fmt.Sprintf(`data: {"rate_limits":{"allowed":%t,"limit_reached":%t,"primary":{"used_percent":%g,"window_minutes":300,"reset_after_seconds":7200},"secondary":{"used_percent":%g,"window_minutes":10080,"reset_after_seconds":86400}}}`, !test.limited, test.limited, test.five, test.weekly)
					applyObservedRateLimits(store, account.AuthID, "", []byte(frame), now)
				} else {
					applyObservedCodexHeaders(store, pluginapi.UsageRecord{Provider: "codex", AuthID: account.AuthID,
						ResponseHeaders: map[string][]string{
							"X-Codex-Allowed": {fmt.Sprint(!test.limited)}, "X-Codex-Limit-Reached": {fmt.Sprint(test.limited)},
							"X-Codex-Primary-Used-Percent": {fmt.Sprint(test.five)}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"7200"},
							"X-Codex-Secondary-Used-Percent": {fmt.Sprint(test.weekly)}, "X-Codex-Secondary-Window-Minutes": {"10080"}, "X-Codex-Secondary-Reset-After-Seconds": {"86400"},
						}}, now)
				}
				account = accountByAuthID(t, store.Snapshot(now), account.AuthID)
				if account.Quota.FiveHour.Kind != WindowFiveHour || account.Quota.LongWindow.Kind != WindowWeekly {
					t.Errorf("window kinds lost during observation: five=%q weekly=%q", account.Quota.FiveHour.Kind, account.Quota.LongWindow.Kind)
				}
				if account.Quota.FiveHour.Exhausted != (test.five >= 100) || account.Quota.LongWindow.Exhausted != (test.weekly >= 100) {
					t.Errorf("exhaustion leaked between windows: five=%v weekly=%v", account.Quota.FiveHour.Exhausted, account.Quota.LongWindow.Exhausted)
				}
				view := accountViewFromState(account, store.Config(), now, nil)
				if reason := accountExclusionReason(view, now); reason != test.want {
					t.Errorf("exclusion reason=%q, want %q", reason, test.want)
				}
				if !test.limited && ClassifyAccount(view, now) != Excluded {
					t.Fatal("observed quota bypassed the configured reserve")
				}
				if !test.limited {
					snapshot := store.Snapshot(now)
					snapshot.CPAAdmission = CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{account.AuthID: {}}}
					req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: account.AuthID, Provider: "codex"}}}
					if decision := PickCodexAccount(req, snapshot, now); !decision.Reject || decision.AuthID != "" {
						t.Fatalf("observed reserve did not reject scheduler request: %#v", decision)
					}
					text := quotaReserveExplanation(accountReservationState(account), now)
					if test.want == "weekly_reserved" && (!strings.Contains(text, "剩余 2.00% ≤ 生效保留 2%") || strings.Contains(text, "没有周额度窗口")) {
						t.Fatalf("weekly reserve explanation lost: %s", text)
					}
				}
			})
		}
	}
}

func TestObservedWindowClassificationAndPartialMerge(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name, minutes string
		kind          WindowKind
		family        AccountFamily
	}{
		{"weekly", "10080", WindowWeekly, AccountFamilyWeekly},
		{"weekly_without_duration", "", WindowWeekly, AccountFamilyWeekly},
		{"weekly_nonstandard_duration", "20160", WindowWeekly, AccountFamilyWeekly},
		{"monthly_minimum", "40320", WindowMonthly, AccountFamilyMonthly},
		{"monthly", "43200", WindowMonthly, AccountFamilyMonthly},
		{"monthly_maximum", "44640", WindowMonthly, AccountFamilyMonthly},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewPluginState(DefaultConfig())
			original := reserveTestAccount(now, 20, 30)
			store.UpsertQuota(original)
			duration := ""
			if test.minutes != "" {
				duration = `,"window_minutes":` + test.minutes
			}
			frame := `data: {"rate_limits":{"secondary":{"used_percent":40,"reset_after_seconds":86400` + duration + `}}}`
			applyObservedRateLimits(store, original.AuthID, "", []byte(frame), now)
			account := accountByAuthID(t, store.Snapshot(now), original.AuthID)
			if account.Quota.LongWindow.Kind != test.kind || account.Family != test.family {
				t.Fatalf("SSE classification: kind=%q family=%q", account.Quota.LongWindow.Kind, account.Family)
			}
			if *account.Quota.FiveHour.UsedPercent != 20 || !account.Quota.FiveHour.ResetAt.Equal(original.Quota.FiveHour.ResetAt) {
				t.Fatal("secondary-only observation changed the 5-hour window")
			}
			headers := map[string][]string{"X-Codex-Secondary-Used-Percent": {"40"}, "X-Codex-Secondary-Reset-After-Seconds": {"86400"}}
			if test.minutes != "" {
				headers["X-Codex-Secondary-Window-Minutes"] = []string{test.minutes}
			}
			quota, ok := codexQuotaFromHeaders(headers, now)
			if !ok || quota.LongWindow.Kind != test.kind || quota.Family != test.family {
				t.Fatalf("header classification: %#v", quota)
			}
			applyObservedRateLimits(store, original.AuthID, "", []byte(`data: {"rate_limits":{"primary":{"used_percent":50,"window_minutes":300,"reset_after_seconds":3600}}}`), now.Add(time.Second))
			account = accountByAuthID(t, store.Snapshot(now.Add(time.Second)), original.AuthID)
			if account.Quota.LongWindow.Kind != test.kind || *account.Quota.LongWindow.UsedPercent != 40 || account.Family != test.family {
				t.Fatal("primary-only observation changed the long window")
			}
		})
	}
}

func TestStreamObserverFramesRateLimitsAcrossChunkSplits(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	var frames [][]byte
	observer := &streamQuotaObserver{}
	// Split the frame at every awkward boundary: before the marker, inside the
	// marker, and inside the JSON payload.
	splits := []string{"event: codex.", "rate_limits\ndata: ", `{"type":"cod`, `ex.rate_limits","rate_limits":{"allowed":true,"limit_rea`, `ched":false,"primary":{"used_percent":25,"window_minutes":300,"reset_after_seconds":10800},"secondary":{"used_percent":10,"window_minutes":10080,"reset_after_seconds":86400}}}`, "\n"}
	for _, split := range splits {
		observer.absorb([]byte(split), func(frame []byte) { frames = append(frames, append([]byte(nil), frame...)) })
	}
	observer.absorb([]byte("\n"), func(frame []byte) { frames = append(frames, append([]byte(nil), frame...)) })
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1 assembled frame (got %#v)", len(frames), frames)
	}
	if !frameLooksLikeRateLimits(frames[0]) {
		t.Fatalf("assembled frame failed prefilter: %s", frames[0])
	}

	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "team", AuthIndex: "idx-team", Provider: "codex", LastSuccessAt: now.Add(-time.Hour)})
	applyObservedRateLimits(store, "team", "idx-team", frames[0], now)
	account := accountByAuthID(t, store.Snapshot(now), "team")
	if account.Quota.FiveHour == nil || account.Quota.FiveHour.UsedPercent == nil || *account.Quota.FiveHour.UsedPercent != 25 {
		t.Fatalf("five-hour window not observed: %#v", account.Quota.FiveHour)
	}
	if account.Quota.LongWindow == nil || account.Quota.Family != AccountFamilyWeekly {
		t.Fatalf("long window/family not observed: %#v %s", account.Quota.LongWindow, account.Family)
	}
	if !account.LastObservedAt.Equal(now) {
		t.Fatalf("LastObservedAt = %s, want %s", account.LastObservedAt, now)
	}
	if !account.LastSuccessAt.Equal(now) {
		t.Fatalf("observation should refresh cache freshness, LastSuccessAt = %s", account.LastSuccessAt)
	}
}

func TestStreamObserverIgnoresOrdinaryFramesAndBouncesBuffer(t *testing.T) {
	observer := &streamQuotaObserver{}
	var handled int
	observer.absorb([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"), func([]byte) { handled++ })
	observer.absorb([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello world this is a long ordinary frame with no quota marker at all\"}\n\n"), func([]byte) { handled++ })
	if handled != 0 {
		t.Fatalf("ordinary frames handled: %d", handled)
	}
	if len(observer.buffer) > len(rateLimitsEventMarker) {
		t.Fatalf("buffer not trimmed for ordinary stream: %d bytes", len(observer.buffer))
	}
	// Marker straddling the next chunk boundary is still detected.
	saw := false
	observer.absorb([]byte("event: codex.rate_lim"), func([]byte) {})
	observer.absorb([]byte("its\ndata: {}\n\n"), func([]byte) { saw = true })
	if !saw {
		t.Fatal("straddled marker frame was not assembled")
	}
}

func TestStreamChunkInterceptIsReadOnlyAndAttributes(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	previous := globalState
	t.Cleanup(func() { globalState = previous })
	globalState = NewPluginState(DefaultConfig())
	globalState.UpsertQuota(AccountState{AuthID: "team", AuthIndex: "idx-team", Provider: "codex", LastSuccessAt: now.Add(-time.Hour)})

	payload, err := json.Marshal(pluginapi.StreamChunkInterceptRequest{
		RequestID: "req-1", ChunkIndex: 3,
		Body:     []byte("event: codex.rate_limits\ndata: {\"rate_limits\":{\"primary\":{\"used_percent\":40,\"window_minutes\":300,\"reset_after_seconds\":3600}}}\n\n"),
		Metadata: map[string]any{"selected_auth_id": "team"},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleMethod(pluginabi.MethodResponseInterceptStreamChunk, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("intercept response not ok: %s", raw)
	}
	// Read-only contract: the response result must not carry any body/headers.
	var envelope struct {
		Result pluginapi.StreamChunkInterceptResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Result.Body) > 0 || envelope.Result.DropChunk || len(envelope.Result.Headers) > 0 {
		t.Fatalf("interceptor attempted to modify the stream: %#v", envelope.Result)
	}
	if account := accountByAuthID(t, globalState.Snapshot(now), "team"); account.Quota.FiveHour == nil {
		t.Fatal("rate_limits frame was not applied to the attributed account")
	}
}

func TestUsageRecordHeadersObserveQuotaOnSuccessOnly(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "team", AuthIndex: "idx-team", Provider: "codex", LastSuccessAt: now.Add(-time.Hour)})
	store.MarkAccountTemporaryExhausted("team", now.Add(2*time.Hour), usageLimitReachedReason)

	// A failed record with a fresh-window header snapshot must NOT clear the
	// marker: the upstream just refused to serve the account.
	HandleUsageFeedback(store, pluginapi.UsageRecord{
		Provider: "codex", AuthID: "team", AuthIndex: "idx-team", Failed: true,
		Failure:         pluginapi.UsageFailure{StatusCode: 429, Body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":7200}}`},
		ResponseHeaders: map[string][]string{"X-Codex-Primary-Used-Percent": {"0"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"18000"}},
	}, now)
	if account := accountByAuthID(t, store.Snapshot(now), "team"); !account.TemporaryExhausted {
		t.Fatal("failed record incorrectly cleared the temporary marker")
	}

	// A successful record with headers observes the window and refreshes the cache.
	HandleUsageFeedback(store, pluginapi.UsageRecord{
		Provider: "codex", AuthID: "team", AuthIndex: "idx-team", Failed: false,
		ResponseHeaders: map[string][]string{"X-Codex-Primary-Used-Percent": {"0"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"18000"}},
	}, now)
	account := accountByAuthID(t, store.Snapshot(now), "team")
	if account.Quota.FiveHour == nil || account.LastObservedAt.IsZero() {
		t.Fatalf("usage headers not observed: %#v", account.Quota.FiveHour)
	}
	// Same-window full reading: the manual/identity-gated reconciliation does
	// not fire, but the real success itself clears the marker.
	if account.TemporaryExhausted {
		t.Fatal("successful request should have cleared the temporary marker")
	}
}

func TestQuotaSourceStatusProjection(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "polled", AuthIndex: "idx-1", Provider: "codex", LastSuccessAt: now})
	store.UpsertQuota(AccountState{AuthID: "observed", AuthIndex: "idx-2", Provider: "codex", LastSuccessAt: now, LastObservedAt: now})
	snapshot := store.Snapshot(now)
	payload := BuildStatusPayload(snapshot, BuildOrderedAccounts(syntheticStatusRequest(snapshot), snapshot, now))
	byID := map[string]string{}
	for _, account := range payload.Accounts {
		byID[account.AuthID] = account.QuotaSource
	}
	if byID["polled"] != "polled" {
		t.Fatalf("polled account source = %q", byID["polled"])
	}
	if byID["observed"] != "observed" {
		t.Fatalf("observed account source = %q", byID["observed"])
	}
}
