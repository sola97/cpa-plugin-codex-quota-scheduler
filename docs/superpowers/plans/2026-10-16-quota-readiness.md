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
- [ ] Inspect the final diff, commit only task files with `fix(scheduler): wait for quota readiness before rejecting requests`, and push `origin/main`.
- [x] Live deployment is outside this change: no NUC/Bandwagon settings or installed libraries were changed. Host HTTP end-to-end status propagation and upstream connectivity still need live verification after deployment.
