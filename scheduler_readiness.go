package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const schedulerQuotaWaitTimeout = 15 * time.Second

// Wait only when quota evidence could make an account selectable. Host IO stays
// in the coalescing activity pump; no state or ABI lock is held while waiting.
func waitForSchedulerQuota(ctx context.Context, req pluginapi.SchedulerPickRequest, pump *pickActivityPump) error {
	snapshot := publishedSchedulerSnapshot.Load()
	if !schedulerQuotaPending(snapshot, req, time.Now()) {
		return nil
	}
	if pump == nil || pump.refresher == nil || pump.state == nil {
		return fmt.Errorf("额度刷新服务不可用")
	}
	pump.state.RecordLog("info", "scheduler.quota_wait", "账号额度尚未就绪，正在查询额度；原请求等待刷新后继续调度", map[string]any{"model": req.Model, "candidate_count": codexCandidateCount(req)}, time.Now())
	pump.enqueue(req, snapshot.AdmissionVersion, time.Now())
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		current := publishedSchedulerSnapshot.Load()
		if !schedulerQuotaPending(current, req, time.Now()) {
			return nil
		}
		if err := schedulerQuotaRefreshFailure(pump, req, current.AdmissionVersion); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待账号额度刷新超时：%w", ctx.Err())
		case <-pump.ctx.Done():
			return fmt.Errorf("额度刷新服务已停止")
		case <-ticker.C:
		}
	}
}

func schedulerQuotaPending(snapshot *SchedulerSnapshot, req pluginapi.SchedulerPickRequest, now time.Time) bool {
	if snapshot == nil || !snapshot.HandleEnabled || !requestIncludesCodex(req) {
		return false
	}
	pending := false
	for _, candidate := range req.Candidates {
		if candidate.ID == "" || candidate.Provider != "codex" {
			continue
		}
		if _, admitted := snapshot.ActiveHighestTier[candidate.ID]; !admitted && len(snapshot.ActiveHighestTier) != 0 {
			continue
		}
		var account *AccountView
		for i := range snapshot.Accounts {
			if snapshot.Accounts[i].ID == candidate.ID {
				account = &snapshot.Accounts[i]
				break
			}
		}
		if account == nil {
			pending = true
			continue
		}
		if account.Cache != CacheUnknown && ClassifyAccount(*account, now) != Excluded {
			return false
		}
		if account.Cache == CacheUnknown || account.Cache == CacheStale || reservationReason(account.Reservation, now) == "reserve_quota_unknown" {
			pending = true
		}
	}
	return pending
}

func schedulerQuotaRefreshFailure(pump *pickActivityPump, req pluginapi.SchedulerPickRequest, version uint64) error {
	if pump.roster != nil {
		roster := pump.roster.Snapshot()
		if !roster.LastSyncAt.IsZero() && (!roster.Confirmed || !roster.BackgroundAllowed) {
			return fmt.Errorf("CPA 启用账号列表尚未确认，不能安全查询额度")
		}
	}
	now := time.Now()
	state := pump.state.Snapshot(now)
	if state.CPAAdmissionVersion != version {
		return nil
	}
	var lastError string
	for _, candidate := range req.Candidates {
		if candidate.ID == "" || candidate.Provider != "codex" {
			continue
		}
		if _, admitted := state.CPAAdmission.AuthIDs[candidate.ID]; !admitted {
			continue
		}
		found := false
		for _, account := range state.Accounts {
			if account.AuthID != candidate.ID {
				continue
			}
			found = true
			if !account.LastSuccessAt.IsZero() && !account.Stale && now.Sub(account.LastSuccessAt) <= state.Config.StaleAfter && accountReservationReason(account, now) != "reserve_quota_unknown" {
				break
			}
			if due, _ := accountRefreshDue(account, state.Config, now); due {
				return nil
			}
			lastError = account.AuthID + "：" + redactSecrets(account.LastError)
			break
		}
		if !found {
			return nil
		}
	}
	if lastError != "" {
		return fmt.Errorf("额度查询失败，等待重试：%s", lastError)
	}
	return nil
}

func quotaNotReadyEnvelope() ([]byte, error) {
	return json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{
		Code: "quota_not_ready", Message: "账号额度尚未就绪，额度查询失败或等待超时，请稍后重试", Retryable: true, HTTPStatus: http.StatusServiceUnavailable,
	}})
}
