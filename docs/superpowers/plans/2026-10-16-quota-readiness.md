# Request Quota Readiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Hold the original Codex request while missing quota is fetched, then select from the refreshed snapshot without bypassing account reserves.

**Architecture:** Keep snapshot selection free of host IO. At the ABI response boundary, wake the existing coalescing activity pump and wait at most 15 seconds for quota readiness. Reuse authoritative roster authorization, background refresh concurrency and failure backoff. Return a retryable HTTP 503 if quota cannot be established.

**Tech Stack:** Go, CPA plugin ABI, existing roster and quota controllers.

**Spec:** User request in this conversation: enabled CPA accounts without quota must automatically query quota before forwarding the same request. Quota reads must not invoke a model.

## Global Constraints

- Never construct roster authority from scheduler candidates.
- Never bypass reserve thresholds, disabled accounts, priority admission or refresh failure backoff.
- Existing selectable accounts retain the nonblocking pick path.
- Query the usage endpoint only; do not enable activation probes or change account settings.
- Preserve unrelated changes; commit and push the completed fix.

### Task 1: Readiness And Background Activity

**Files:** Add `scheduler_readiness.go`, `scheduler_readiness_test.go`; modify `dispatch.go`, `pick_activity.go`, `quota_reserve_logs.go`.

**Interfaces:** The ABI handler calls `waitForSchedulerQuota(ctx, req, pump)` before snapshot selection. The activity pump records activity and reads the current admission version after roster synchronization. Existing refresher ownership remains unchanged.

- [x] Add regression tests for the same original request waiting for a gated usage response, selecting only after successful refresh, preserving actual reserve rejection, and sharing concurrent refresh work.
- [x] Run focused tests. Initial execution was blocked by a 32-bit GCC on PATH; use the existing 64-bit w64devkit compiler. Focused production-runtime tests subsequently passed five consecutive runs.
- [x] Implement bounded waiting on immutable snapshots, release on pump shutdown or context deadline, distinguish unavailable quota from actual reserve rejection, and encode retryable 503 using the SDK envelope.
- [x] Verify failure backoff prevents repeated request-triggered queries; verify one valid account does not wait for two other accounts' missing quota; verify roster failure performs no quota IO.
- [x] Run focused tests, existing async bounded-pick tests and `go test -race ./...`. All passed.

### Task 2: Documentation And Delivery

**Files:** Modify `README.zh-CN.md` and this plan.

- [x] Document the 15-second wait, read-only usage lookup, unchanged original model, separate activation probes, and 503 failure behavior.
- [x] Run `go test ./...`, `go vet ./...` and a Windows dynamic-library build. All passed; the final complete race suite also passed after moving the new tests onto production runtime bindings.
- [x] Inspect the final diff, commit only task files with `fix(scheduler): wait for quota readiness before rejecting requests`, and push `origin/main` (9f3e11c).
- [x] Live deployment is outside this change: no NUC/Bandwagon settings or installed libraries were changed. Host HTTP end-to-end status propagation and upstream connectivity still need live verification after deployment.

### Task 3: Downstream Error Contract And Dual-Host Deployment

**Goal:** User's final follow-up prefers semantic HTTP 429 (superseding 402) with `usage_limit_reached` in both type and code. Use `You've hit your 5-hour usage limit.` for 5h reserve rejection and matching weekly/mixed text for other windows. Preserve the separate retryable 503 readiness error.

**Files:** Modify `dispatch.go`, `scheduler_readiness.go`, `quota_reserve_test.go`, `scheduler_readiness_test.go`, `README.zh-CN.md`; deployment helpers live outside the repository under the current task's `work` directory.

- [x] Update ABI contract tests and demonstrate failure against the prior response.
- [x] Return a structured OpenAI error as the ABI error message so CPA preserves the public type/code, plus the SDK HTTP status field.
- [x] Fix startup empty-roster caching and failure checks racing the activity pump. Add a gated authoritative-roster regression; preserve safe refusal when roster confirmation truly fails and 503 when the refresh service is unavailable.
- [x] Run focused and full Linux tests, race checks and vet; build a versioned Linux shared library using the existing WSL Go 1.26 compiler. All passed after the final startup fix.
- [x] Verify the actual HTTP error contract in a disposable CPA v8.0.31 container with synthetic account data, without modifying live account reserves. Cold first requests returned 429/5-hour (0.534s), 429/weekly (0.526s), and 503/quota_not_ready (0.264s). Synthetic host HTTP responses never reached a model or real account.
- [x] Back up each installed library, verify configuration/rule digests, atomically install on NUC and restart CPA with its TUIC sidecar. Use the existing five-minute single-direction synchronizer to deploy the exact same library to Bandwagon, preserving target connection settings.
- [x] Read both hosts' plugin registration, account state and request logs; verify cold-start recovery and persistence without printing credentials. Source committed/pushed as b4f2f8b. Deployment evidence is recorded in `docs/deployments/2026-10-17-quota-readiness.md`.
