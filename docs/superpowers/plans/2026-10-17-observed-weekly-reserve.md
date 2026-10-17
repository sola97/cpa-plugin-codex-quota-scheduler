# Observed Weekly Quota Reserve Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve independently typed 5-hour and weekly windows in response observations so weekly reserves remain enforced before actual exhaustion.

**Architecture:** Keep the current SSE/header observer and state merge. Reuse `classifyWindow` in both observation paths. Derive each observed window's exhaustion from its own usage, not the shared envelope. Report the actual exhausted window from the existing snapshot evidence.

**Tech Stack:** Go 1.26, CPA plugin ABI, Docker/SSH, existing NUC-to-Bandwagon synchronizer.

**Spec:** User's 2026-10-17 NUC log at 13:32:24: 5h remaining 79%, weekly incorrectly reported absent while the weekly window was actually exhausted. User reset weekly quota manually at 15:48; do not interpret that as automatic recovery.

## Global Constraints

- 5-hour and weekly limits, reserves and reset times are independent; either exhausted window still excludes the account.
- Do not change account settings or trigger paid activation/reset actions.
- Do not bypass authoritative roster or modify the prior 429/503 contract.
- Preserve non-observed windows when an observation only reports one window.
- Commit and push completed changes; deployment must back up libraries and preserve settings.

### Task 1: Observation And Regression

**Files:** `stream_observer.go`, `stream_observer_test.go`, `scheduler_snapshot.go`, `quota_reserve_logs.go`.

- [x] Add failing tests for SSE and normalized headers: weekly reserve boundary with ample 5h, 5h reserve boundary with ample weekly, independent exhaustion under global limit flags, window typing with/without duration, monthly typing and partial observations. Old code reproduced weekly remaining 2% being selectable with 5h remaining 79% in both observation paths.
- [x] Reuse existing window classification in both observers and remove cross-window propagation of shared exhaustion flags when the window has its own percentage. Preserve the polled parser's positional fallback for nonstandard durations.
- [x] Report weekly/5h/monthly exhaustion distinctly in scheduler details and verify polled and observed paths.
- [x] Run focused tests, full Go tests, race and vet; build versioned Linux amd64 library. All passed. Version `0.3.2-sola97.weeklywindow`, SHA256 `8942ec7bfab9b05c4b194df6c7fc16709317e8aec9184fdeae9fa595232d0fc1`.

### Task 2: Delivery

**Files:** `README.zh-CN.md`, this plan and a deployment record.

- [x] Document why the pre-reset refusal was correct but weekly reserve handling was not.
- [ ] Commit and push the fix.
- [ ] Deploy the exact tested library to NUC with backup and shared-network sidecar restart, then use the existing synchronizer for Bandwagon.
- [ ] Verify both hosts' window kinds remain correct after real traffic, settings unchanged and weekly-exhausted accounts still excluded; record results and push docs.
