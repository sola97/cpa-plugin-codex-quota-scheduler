package main

import (
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func schedulerReasonText(reason string) string {
	switch reason {
	case "selected", "available":
		return "账号满足当前调度条件"
	case "handle_disabled":
		return "插件调度已关闭"
	case "provider_not_codex":
		return "不是 Codex 请求，不参与此插件调度"
	case "quota_reserve_protected":
		return "候选账号存在已开启的额度保留保护；没有可选账号，禁止内置调度回退"
	case "five_hour_reserved":
		return "5 小时剩余额度已达到保留阈值"
	case "weekly_reserved":
		return "周剩余额度已达到保留阈值"
	case "reserve_quota_unknown":
		return "保护已开启但额度无法核实，等待额度刷新"
	case "quota_not_ready":
		return "CPA 候选账号的额度尚未就绪，查询失败或等待超时；这不是已确认触及保留线"
	case "auth_failure":
		return "账号认证失败，等待恢复"
	case "circuit_open":
		return "账号熔断已开启，暂不可选"
	case "circuit_half_open":
		return "账号处于熔断恢复试用阶段"
	case "temporary_exhausted":
		return "账号暂时额度耗尽，尚未到恢复时间"
	case "trial_pending":
		return "账号已有恢复试用请求，等待验证结果"
	case "five_hour_exhausted":
		return "5 小时额度已耗尽，等待该窗口重置和新额度验证"
	case "weekly_exhausted":
		return "周额度已耗尽，等待周窗口重置和新额度验证；5 小时窗口仍独立判定"
	case "monthly_exhausted":
		return "月额度已耗尽，等待月窗口重置和新额度验证"
	case "window_exhausted", "long_window_exhausted":
		return "额度窗口已耗尽，等待重置和新额度验证"
	case "stale_quota":
		return "额度缓存已过期且没有可用证据"
	case "unknown_account", "unknown_family":
		return "账号或额度窗口尚未确认"
	case "no_candidates", "no ordered candidates":
		return "当前没有符合范围的候选账号"
	case "no_selectable_account", "fallback_fill_first":
		return "没有可选账号，按配置回退到内置调度"
	default:
		return "当前未满足账号选择条件（" + reason + "）"
	}
}

func schedulerReserveDetails(snapshot SchedulerSnapshot, req pluginapi.SchedulerPickRequest, now time.Time) []string {
	if !snapshot.HandleEnabled {
		return []string{"插件调度已关闭，账号保留规则不参与调度"}
	}
	var details []string
	for _, candidate := range req.Candidates {
		if candidate.Provider != "codex" || candidate.ID == "" {
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
			annotation := snapshot.ReserveAnnotations["auth:"+candidate.ID]
			message := "账号尚未加载，无法确认额度及规则归属"
			if hasQuotaReserve(annotation) {
				message += "；已保存的保留保护已开启，禁止通过内置调度绕过"
			}
			details = append(details, candidate.ID+"："+message)
			continue
		}
		state := account.Reservation
		text := quotaReserveExplanation(state, now)
		if _, admitted := snapshot.ActiveHighestTier[account.ID]; !admitted {
			text += "；不在当前允许的 CPA 优先级范围内"
		} else if reason := accountExclusionReason(*account, now); reason != "available" {
			text += "；未选择原因：" + schedulerReasonText(reason)
		}
		details = append(details, candidate.ID+"："+text)
	}
	return details
}

func quotaReserveExplanation(state QuotaReservation, now time.Time) string {
	annotation := state.Annotation
	if !annotation.QuotaReserveEnabled {
		return "保留保护未开启，已保存的比例和时间规则不生效"
	}
	if !hasQuotaReserve(annotation) {
		return "保留开关已开启，但两个窗口基础比例均为 0%，未启用窗口保护"
	}
	if state.Unknown {
		return "保留保护已开启，但额度尚未读取成功或已经过期；无法判定剩余比例，暂停选择并等待刷新"
	}
	fiveHour := quotaReserveWindowExplanation("5 小时", annotation.FiveHourReservePercent, annotation.FiveHourReserveSteps, WindowFiveHour, state.FiveHour, now)
	weekly := "周额度：当前账号没有周额度窗口，规则不适用"
	if annotation.WeeklyReservePercent == 0 {
		weekly = "周额度：基础比例为 0%，该窗口保护未启用"
	} else if state.Family == AccountFamilyUnknown || (state.Family == AccountFamilyWeekly && state.LongWindow == nil) {
		weekly = "周额度：窗口尚未确认，等待额度刷新"
	} else if state.LongWindow != nil && state.LongWindow.Kind == WindowWeekly {
		weekly = quotaReserveWindowExplanation("周额度", annotation.WeeklyReservePercent, annotation.WeeklyReserveSteps, WindowWeekly, state.LongWindow, now)
	}
	return fiveHour + "；" + weekly
}

func quotaReserveWindowExplanation(label string, base int, steps []QuotaReserveStep, kind WindowKind, window *QuotaWindow, now time.Time) string {
	if base == 0 {
		return label + "：基础比例为 0%，该窗口保护未启用"
	}
	if window == nil {
		return label + "：当前账号没有此额度窗口，规则不适用"
	}
	reserve := effectiveQuotaReserve(base, steps, kind, window, now)
	rule := fmt.Sprintf("基础保留 %d%%", base)
	if window.ResetAt.After(now) {
		until := window.ResetAt.Sub(now)
		if steps == nil {
			steps = defaultQuotaReserveSteps(kind)
		}
		closest := 0
		for _, step := range steps {
			if until <= time.Duration(step.MinutesBeforeReset)*time.Minute && (closest == 0 || step.MinutesBeforeReset < closest) {
				closest = step.MinutesBeforeReset
			}
		}
		if closest > 0 {
			rule = fmt.Sprintf("距重置 %.1f 分钟，命中最后 %d 分钟规则，保留 %d%%", until.Minutes(), closest, reserve)
		} else if len(steps) > 0 {
			rule += "，未进入任何临近重置时间规则"
		} else {
			rule += "，未配置时间规则"
		}
	} else if !window.ResetAt.IsZero() {
		rule += "，本地重置时间已到，等待新额度证据"
	} else {
		rule += "，重置时间未知，时间规则不适用"
	}
	reason := windowReservationReason(window, reserve, "reserved")
	switch reason {
	case "reserve_quota_unknown":
		return label + "：" + rule + "；剩余额度未知，暂停选择并等待刷新"
	case "reserved":
		if window.Exhausted {
			return label + "：" + rule + "；额度窗口已耗尽，暂停选择"
		}
		return fmt.Sprintf("%s：%s；剩余 %.2f%% ≤ 生效保留 %d%%，暂停选择", label, rule, 100-*window.UsedPercent, reserve)
	default:
		if reserve == 0 {
			return label + "：" + rule + "；当前规则保留 0%，不执行该窗口保护"
		}
		return fmt.Sprintf("%s：%s；剩余 %.2f%% > 生效保留 %d%%，未触发保留限制", label, rule, 100-*window.UsedPercent, reserve)
	}
}
