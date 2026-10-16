package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func readinessFixture(t *testing.T, used int) (*fakeHostClient, *PluginState, *pickActivityPump, pluginapi.SchedulerPickRequest) {
	t.Helper()
	host := &fakeHostClient{
		authList:  []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex"}},
		authJSON:  map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access-a","account_id":"account-a"}`)},
		httpBody:  []byte(`{"rate_limit":{"primary_window":{"used_percent":` + strconv.Itoa(used) + `,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		doStarted: make(chan struct{}, 1), releaseDo: make(chan struct{}),
	}
	state := NewPluginState(DefaultConfig())
	state.SetAnnotations(AnnotationState{Accounts: map[string]AccountAnnotation{"auth:a": {QuotaReserveEnabled: true, FiveHourReservePercent: 10}}})
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	priority := 0
	c := NewRosterController(RosterControllerOptions{
		Host: &s7RosterHost{entries: []RosterEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: &priority}}},
		Publish: func(ctx context.Context, active ActiveRoster) (ActiveRoster, error) {
			if err := r.PublishAuthoritativeRoster(ctx, hostRosterSnapshotFromActive(active)); err != nil {
				return active, err
			}
			active.Generation = r.runtimeRoster().Generation
			return active, nil
		}, Observe: r.ObserveRosterLifecycle,
	})
	previousSnapshot := publishedSchedulerSnapshot.Load()
	previousPump := globalPickActivityPump.Load()
	p := newPickActivityPump(c, r, state)
	globalPickActivityPump.Store(p)
	publishSchedulerState(state, nil, time.Now())
	t.Cleanup(func() {
		select {
		case <-host.releaseDo:
		default:
			close(host.releaseDo)
		}
		p.stop()
		<-p.done
		<-p.observationDone
		r.Stop()
		globalPickActivityPump.Store(previousPump)
		publishedSchedulerSnapshot.Store(previousSnapshot)
	})
	return host, state, p, pluginapi.SchedulerPickRequest{Provider: "codex", Model: "client-original-model", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}}}
}

func TestSchedulerQuotaWaitsForUsageThenForwardsOriginalPick(t *testing.T) {
	host, _, _, req := readinessFixture(t, 20)
	raw, _ := json.Marshal(req)
	done := make(chan []byte, 1)
	go func() { response, _ := handleSchedulerPick(raw); done <- response }()
	select {
	case response := <-done:
		t.Fatalf("request returned before quota was read: %s", response)
	case <-host.doStarted:
	case <-time.After(time.Second):
		t.Fatal("missing quota never triggered a usage query")
	}
	select {
	case response := <-done:
		t.Fatalf("request did not wait for quota: %s", response)
	case <-time.After(20 * time.Millisecond):
	}
	close(host.releaseDo)
	var response envelope
	select {
	case raw := <-done:
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("original request did not resume after quota refresh")
	}
	var picked pluginapi.SchedulerPickResponse
	if !response.OK || json.Unmarshal(response.Result, &picked) != nil || picked.AuthID != "a" {
		t.Fatalf("pick response = %#v / %#v", response, picked)
	}
	for _, url := range host.requestedURLs() {
		if url != chatGPTQuotaEndpoint && url != resetCreditsEndpoint {
			t.Fatalf("unexpected model/activation call: %s", url)
		}
	}
}

func TestSchedulerQuotaActualReserveStillRejectsAfterRefresh(t *testing.T) {
	host, _, _, req := readinessFixture(t, 95)
	close(host.releaseDo)
	raw, _ := json.Marshal(req)
	response, err := handleSchedulerPick(raw)
	if err != nil {
		t.Fatal(err)
	}
	var result envelope
	if json.Unmarshal(response, &result) != nil || result.Error == nil || result.Error.Code != "quota_reserve_protected" {
		t.Fatalf("actual reserve must still reject: %s", response)
	}
}

func TestSchedulerQuotaDeadlineReturnsRetryable503(t *testing.T) {
	_, _, p, req := readinessFixture(t, 20)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := waitForSchedulerQuota(ctx, req, p); err == nil {
		t.Fatal("expected bounded readiness failure")
	}
	response, err := quotaNotReadyEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	var result pluginabi.Envelope
	if json.Unmarshal(response, &result) != nil || result.Error == nil || result.Error.HTTPStatus != http.StatusServiceUnavailable || !result.Error.Retryable || result.Error.Code != "quota_not_ready" {
		t.Fatalf("readiness failure envelope: %s", response)
	}
}

func TestSchedulerQuotaOneReadyAccountForwardsWhileOtherTwoRefresh(t *testing.T) {
	host, state, p, req := readinessFixture(t, 20)
	priority := 0
	rosterHost := p.roster.host.(*s7RosterHost)
	entries := append([]RosterEntry(nil), rosterHost.entries...)
	for _, id := range []string{"b", "c"} {
		host.authList = append(host.authList, pluginapi.HostAuthFileEntry{ID: id, AuthIndex: "idx-" + id, Provider: "codex"})
		host.authJSON["idx-"+id] = json.RawMessage(`{"access_token":"access-` + id + `","account_id":"account-` + id + `"}`)
		entries = append(entries, RosterEntry{ID: id, AuthIndex: "idx-" + id, Provider: "codex", Priority: &priority})
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex"})
	}
	rosterHost.result(entries, nil)
	if _, err := p.roster.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	used := 20.0
	binding, _ := p.refresher.bindings.Lookup("a")
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx-a", Provider: "codex", Instance: binding.Instance, LastSuccessAt: time.Now(), Quota: ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: time.Now().Add(time.Hour)}}})
	publishSchedulerState(state, nil, time.Now())
	raw, _ := json.Marshal(req)
	done := make(chan []byte, 1)
	go func() { response, _ := handleSchedulerPick(raw); done <- response }()
	select {
	case response := <-done:
		var result envelope
		var picked pluginapi.SchedulerPickResponse
		if json.Unmarshal(response, &result) != nil || !result.OK || json.Unmarshal(result.Result, &picked) != nil || picked.AuthID != "a" {
			t.Fatalf("ready account was not selected: %s", response)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ready account blocked on other accounts' quota queries")
	}
	select {
	case <-host.doStarted:
	case <-time.After(time.Second):
		t.Fatal("missing accounts were not refreshed asynchronously")
	}
	close(host.releaseDo)
	if !waitForCondition(t, time.Second, func() bool {
		return len(state.Snapshot(time.Now()).Accounts) == 3
	}) {
		t.Fatal("both missing accounts did not finish refreshing")
	}
	if got := readinessQuotaReadCount(host); got != 2 {
		t.Fatalf("refreshed %d accounts, want only the two missing accounts", got)
	}
}

func TestSchedulerQuotaConcurrentRequestsShareRefresh(t *testing.T) {
	host, _, p, req := readinessFixture(t, 20)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 24)
	for i := 0; i < cap(done); i++ {
		go func() { done <- waitForSchedulerQuota(ctx, req, p) }()
	}
	select {
	case <-host.doStarted:
	case <-ctx.Done():
		t.Fatal("no quota request started")
	}
	close(host.releaseDo)
	for i := 0; i < cap(done); i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := readinessQuotaReadCount(host); got != 1 {
		t.Fatalf("concurrent requests made %d auth refreshes, want 1", got)
	}
}

func TestSchedulerQuotaFailureRespectsBackoff(t *testing.T) {
	host, state, _, req := readinessFixture(t, 20)
	host.httpStatus = http.StatusServiceUnavailable
	close(host.releaseDo)
	raw, _ := json.Marshal(req)
	for i := 0; i < 3; i++ {
		response, err := handleSchedulerPick(raw)
		if err != nil {
			t.Fatal(err)
		}
		var result pluginabi.Envelope
		if json.Unmarshal(response, &result) != nil || result.Error == nil || result.Error.Code != "quota_not_ready" || result.Error.HTTPStatus != 503 {
			t.Fatalf("missing quota was misreported as a reserve block: %s", response)
		}
	}
	if got := readinessQuotaReadCount(host); got != 1 {
		t.Fatalf("failure backoff bypassed: %d refreshes", got)
	}
	if !waitForCondition(t, time.Second, func() bool {
		for _, log := range state.Snapshot(time.Now()).Logs {
			if log.Event == "scheduler.quota_not_ready" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("quota readiness failure not logged separately")
	}
}

func readinessQuotaReadCount(host *fakeHostClient) int {
	count := 0
	for _, url := range host.requestedURLs() {
		if url == chatGPTQuotaEndpoint {
			count++
		}
	}
	return count
}

func TestSchedulerQuotaUnconfirmedRosterMakesNoQuotaCalls(t *testing.T) {
	host, _, p, req := readinessFixture(t, 20)
	p.roster.host.(*s7RosterHost).result(nil, errors.New("host roster unavailable"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForSchedulerQuota(ctx, req, p); err == nil {
		t.Fatal("unconfirmed roster should not authorize quota reads")
	}
	if got := host.httpCallCount(); got != 0 {
		t.Fatalf("unconfirmed roster made %d external calls", got)
	}
}

func TestSchedulerQuotaShutdownReleasesWaiters(t *testing.T) {
	_, _, p, req := readinessFixture(t, 20)
	done := make(chan error, 1)
	go func() { done <- waitForSchedulerQuota(context.Background(), req, p) }()
	p.stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown should abort readiness waiting")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown left a request waiting")
	}
}
