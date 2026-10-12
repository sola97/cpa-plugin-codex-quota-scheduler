# Per-Account Quota Reserves Implementation Plan

> **For agentic workers:** Execute this plan inline. Read-only independent review may be delegated; the main agent owns integration, validation, commit and push.

**Goal:** Sync upstream and support independent per-account 5-hour and weekly quota reserves, including 10% for each.

**Architecture:** Store two percentages in existing account annotations. Publish a reservation reason with immutable scheduler views, use it in selection and management status, and return an ABI error instead of delegating a protected request to the built-in scheduler.

**Tech Stack:** Go 1.26, CPA v7.3.8 plugin ABI, embedded HTML/JavaScript management UI.

**Spec:** User request in this task: sync upstream, allow independently configured 5h and weekly reserves.

**Additional requirements:** Every account can customize its base percentages and add/remove arbitrary time steps. Defaults: 5h last 20/10 minutes reserve 3/1%; weekly last 60/30/15/5 minutes reserve 10/5/3/1%. The closest matching time step replaces the base, evaluated at pick time. Nil step arrays use defaults; explicit empty arrays retain a fixed base.

## Global Constraints

- Preserve the fork's existing CI artifact retention change.
- Integers from 0 to 100; 0 disables one reserve, legacy annotations default to 0.
- Remaining <= reserve blocks new selection; either window can block independently.
- Weekly reserves apply to weekly windows, not monthly windows.
- Missing usage for an existing protected window blocks pending quota refresh.
- A successful quota read with no 5h window keeps upstream's no-5h-window behavior.
- Do not release an observed reserve block just because local time passes reset.
- Validate PATCH, PUT and import input before persistence; retain export/reload compatibility.
- Settings updates republish scheduling state before returning success.
- Protection concerns new scheduling based on reported quota; in-flight requests and external usage can cross the threshold.

## Task 1: Sync Upstream

- [x] Fetch `JefferyZhang2019/cpa-plugin-codex-quota-scheduler` main.
- [x] Merge `upstream/main` while retaining the fork-only CI commit.

## Task 2: Reservation Policy and Scheduler

**Files:** `models.go`, `quota_reserve.go`, `selection.go`, `scheduler.go`, `scheduler_snapshot.go`, `dispatch.go`.

**Interfaces:** `AccountAnnotation.FiveHourReservePercent`, `AccountAnnotation.WeeklyReservePercent` are ints; `FiveHourReserveSteps`, `WeeklyReserveSteps` are slices of `QuotaReserveStep{MinutesBeforeReset, ReservePercent}`; `accountReservationReason(AccountState, time.Time) string` returns an exclusion reason; `PickDecision.Reject bool` suppresses fallback.

- [x] Add regression tests for 89.9/90/90.1% used, independent windows, 0/100%, unknown usage, monthly accounts and reset recovery.
- [x] Add annotation fields and the shared policy. The numeric comparison is:

```go
if window.UsedPercent == nil {
    return "reserve_quota_unknown"
}
if *window.UsedPercent >= float64(100-reserve) {
    return reason
}
```

- [x] Exclude protected views in `ClassifyAccount` and account queue state.
- [x] Preserve normal selection of other accounts and lower priority tiers when enabled.
- [x] If no selection succeeds and any offered candidate has an active reserve configuration, return `PickDecision{Handled: true, Reject: true, Reason: "quota_reserve_protected"}`. Include saved annotations for not-yet-loaded accounts.
- [x] Encode rejection as `errorEnvelope("quota_reserve_protected", ...)`, supported by CPA v7.3.8 scheduler errors.
- [x] Verify nested model retries use the host scheduler; do not duplicate account selection in the retry executor.

## Task 3: Management and Persistence

**Files:** `annotations.go`, `management.go`, `quota_reserve_test.go`, `management_dom_test.go`.

**Interfaces:** PATCH accepts `five_hour_reserve_percent` and `weekly_reserve_percent`; status exposes the same fields.

- [x] Validate annotation percentages in PATCH, PUT and import before writing.
- [x] Republish scheduling state immediately after annotation changes and import.
- [x] Add numeric controls to the account editor with min=0, max=100, step=1; retain independent values when saving only one setting.
- [x] Add editable time-step rows with separate minutes and percentage inputs plus add/remove buttons for each window. Reject duplicate minutes and out-of-window thresholds (5h max 300, weekly max 10080 minutes).
- [x] Display configured reserves and localized reservation reasons on account cards.
- [x] Test status, PATCH preservation, invalid values, PUT/import validation, export/reload and immediate published selection.

## Task 4: Verification and Delivery

**Files:** `README.md`, `README.zh-CN.md`, this plan.

- [x] Document management setup, JSON example, reset and fallback semantics, and reported-quota limits.
- [x] Run `go test ./...`, focused `go test -race`, and `go vet ./...` with a compatible C compiler.
- [x] Render the real management template and check editor save/reload, validation, status reasons, desktop and mobile layout in a browser.
- [x] Build the shared plugin with `-buildmode=c-shared`.
- [x] Review `git diff --check` and the scoped diff.

## Verification Evidence

- WSL Ubuntu: `go test ./...` and `go vet ./...` passed.
- Focused race checks: `go test -race ./... -run "QuotaReserve|AccountAnnotationPatchResolvesInstanceIdentityFromAuthID|StreamObserver|SchedulerPickABIPath" -count=1` passed.
- Linux amd64 shared plugin: `go build -trimpath -buildmode=c-shared -o dist/codex-quota-scheduler.so .` passed; ELF shared object confirmed.
- Real management handlers and embedded UI were exercised with a local seeded-account test harness, including independent values, add/remove rules, empty arrays, invalid and duplicate input, save/reopen/reload, immediate status updates, and Chinese/English labels.
- Desktop 1440x1000 and mobile 390x844 screenshots inspected; no mobile horizontal overflow or JavaScript page errors.
- Independent read-only review found no remaining concrete defects in the reservation policy or management integration.
- No deployment to a live CPA instance or real-account quota consumption was performed. Multi-platform CI is checked separately after push.
