# Weekly-Aligned Primary Quota Activation Probe Design

Status: Implemented

Date: 2026-08-03

## Summary

Add an opt-in account activation probe for Codex accounts whose five-hour
primary quota reports 100% remaining while the weekly quota reset time is
approximately seven days from the current time. The probe sends one minimal
Codex request containing `你好`, verifies the quota state, and records a
durable per-account/per-weekly-reset result so the same weekly quota cycle is
never probed more than once.

If a later automatic check finds that the weekly reset time no longer matches
the seven-day expectation, automatic probing stops for that account. A manual
quota refresh is required to re-arm the account.

This feature extends the existing crash-safe Probe execution path, but it does
not change the semantics of the existing lazy-reset probe. A separate setting
keeps the new quota-consuming behavior explicit and backward compatible.

## User-confirmed semantics

- “Quota is 100%” means the five-hour primary quota has `used_percent == 0`.
- Accounts without a five-hour primary window do not qualify.
- The weekly window is a validation signal, not the quota window used for the
  first predicate.
- A weekly reset is aligned when:

  ```text
  abs(weekly.reset_at - (now + 7 days)) <= 2 minutes
  ```

- A successful probe is sent at most once for one account and one weekly reset
  cycle.
- The existing ten-minute Probe send suppression remains in force for crashes,
  uncertain sends, and duplicate wakeups.
- A weekly-reset mismatch moves the account to a manual-refresh-required state;
  automatic refreshes may update displayed quota but may not send a probe.

## Goals

1. Activate a fresh primary quota window only when the account's weekly reset
   timestamp provides a strong seven-day consistency signal.
2. Send the smallest supported Codex request and verify its effect through a
   second quota read.
3. Persist enough state to prevent duplicate sends across refreshes,
   concurrent wakes, process restarts, and uncertain HTTP outcomes.
4. Keep the behavior opt-in, visible in the Management UI, and isolated from
   account selection, circuit-breaker state, and scheduler trials.
5. Stop automatic probing after a weekly-reset mismatch until an explicit
   manual refresh re-arms the account.

## Non-goals

- Do not make this behavior always-on.
- Do not repurpose or silently change the existing `enable_reset_probe`
  lazy-reset behavior.
- Do not probe accounts without a five-hour primary window.
- Do not send a probe on every quota refresh while `used_percent == 0`.
- Do not add arbitrary endpoint, model, or prompt configuration.
- Do not use the probe as a scheduler pick, circuit-breaker recovery request, or
  business usage event.
- Do not continuously poll dormant accounts solely to find candidates. The
  feature runs on the existing authorized quota-refresh/probe wake path.

## Alternatives considered

### A. Add the rule directly to the existing lazy-reset probe

This has the smallest apparent diff, but it changes the meaning of an existing
user-facing setting and removes the current generic lazy-reset behavior. It
would also make upgrades surprising for users who already enabled the setting.

Rejected because it is not backward compatible.

### B. Add a direct check inside the normal quota refresh function

This would compare the two windows and call the existing HTTP helper directly.
It is easy to prototype, but it would duplicate the Probe WAL, single-flight,
lease, verify-first recovery, and suppression rules. A refresh retry or process
restart could send duplicate messages.

Rejected because it creates a second unsafe send path.

### C. Add a separate activation-probe mode on the existing Probe runtime

This keeps the current lazy-reset mode intact while reusing the existing
coordinator, instance lease, WAL, send fence, propagation wait, verification
read, and durable recovery. The new mode owns a separate account-level gate
because its predicate combines the primary and weekly windows.

Recommended because it preserves existing behavior while sharing the hard parts
that protect real quota-consuming requests.

## Configuration and Management UI

Add a new setting:

```yaml
enable_weekly_activation_probe: false
```

The default is `false`. The existing `enable_reset_probe` setting remains
unchanged.

The Management UI adds a protected setting with Chinese copy equivalent to:

```text
自动激活七天额度窗口
当五小时额度剩余100%，且周额度重置时间约为7天后时，发送一次“你好”来激活额度窗口。每个账号每个周额度周期只发送一次，可能消耗少量额度。
```

The UI must also explain that a weekly-reset mismatch disables automatic
probing for that account until the user manually refreshes its quota.

Status payloads and account cards should expose, without credentials:

- activation probe state;
- recorded weekly reset timestamp;
- cooldown or next-check time when applicable;
- last probe time;
- manual-refresh-required status;
- sanitized last error.

## State model

The existing `ProbeWindow` state is per quota window. This feature combines two
windows, so it uses a separate persisted account-level activation state while
sharing the existing `ProbeAttempt`/WAL send machinery.

Conceptually, the state is:

```text
Idle
  ↓ feature enabled and account is eligible
ReadyForProbe
  ↓ successful send and verification
Confirmed(weekly_reset_at)
  ↓ weekly reset changes or becomes misaligned
ManualRefreshRequired
  ↓ successful explicit manual refresh
ReadyForProbe or Confirmed
```

An in-flight send is represented by the existing durable Probe attempt phases:

```text
Prepared → Sending → Sent → verify → Confirmed
                         ↘ SentUnknown → verify-first recovery
```

The account-level state must contain at least:

- `State`;
- `WeeklyResetAt` used as the cycle identity;
- `CooldownUntil`;
- `LastProbeAt`;
- `LastError`.

The persistent map is keyed by `AuthInstanceID`, not a mutable auth filename.
Credential binding epochs and the existing send fences continue to protect
against stale credentials and roster changes.

The state schema must initialize missing maps on load and preserve existing
state files without treating absent activation state as an error.

## Trigger and decision flow

For each eligible, authoritative account during an authorized probe wake:

1. Read the current credentials and primary/weekly quota through the existing
   held instance lease.
2. If the feature is disabled, return without changing activation state.
3. If the account has no five-hour window, or its `UsedPercent` is missing,
   return without sending.
4. If `UsedPercent != 0`, return without sending. Do not guess that a missing
   or nonzero value means “100% remaining”.
5. If the weekly window or `weekly.ResetAt` is missing, move to
   `ManualRefreshRequired` only when an existing activation record was already
   armed; otherwise leave the account idle and do not send.
6. Compute the seven-day alignment using the time of the completed quota read.
7. If the alignment is outside ±2 minutes, persist
   `ManualRefreshRequired` and return without a POST.
8. If the state is already `Confirmed` for the same weekly reset cycle, return
   without a POST.
9. If a nonterminal attempt exists, let recovery own it; never create a second
   send for the same instance.
10. Persist a prepared activation attempt, then execute the existing
    send-fenced Probe sequence.

The trigger decision is pure and separately testable. It must not perform I/O,
mutate scheduler selection state, or create goroutines.

## Probe request and verification

The send uses the existing Codex compact endpoint and credential headers. The
payload changes only the input text for this new activation mode:

```json
{"model":"gpt-5.4-mini","instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]}]}
```

The sequence is:

1. Precheck quota and capture the primary/weekly snapshots.
2. Persist the send fence and `Sending` WAL record before the POST.
3. Send the compact request under the held instance lease.
4. Persist `Sent` before waiting for propagation.
5. Wait for the existing short propagation interval.
6. Perform a barrier quota read after the send fence.
7. If the weekly reset remains aligned within ±2 minutes and the request
   produced positive usage evidence, mark the activation cycle `Confirmed`.
8. If the post-send weekly reset is no longer aligned, mark
   `ManualRefreshRequired`; do not send another request automatically.
9. Complete the WAL only after the activation state and verification result are
   durably persisted.

An HTTP 2xx response without positive usage evidence is not sufficient to mark
the activation successful. A sent-but-uncertain attempt is verified first after
restart and is never immediately resent.

## Cooldown, retry, and manual re-arm

- The existing ten-minute send suppression is retained for every activation
  attempt.
- A successfully verified activation becomes `Confirmed` and is not eligible
  for another send during that weekly reset cycle, even after cooldown expires.
- A pre-send failure uses the existing bounded retry delays and does not consume
  the once-per-cycle success marker.
- A post-send uncertainty enters the existing `SentUnknown` recovery path.
- An automatic quota refresh cannot clear `ManualRefreshRequired`.
- An explicit successful manual refresh re-evaluates the account. If the
  primary and weekly predicates are valid and the current weekly reset has not
  already been confirmed, the state returns to `ReadyForProbe`; otherwise it
  remains held without sending.
- A new weekly reset timestamp does not automatically re-arm an account that
  is held for manual refresh. This is intentional and follows the requested
  stop-until-manual-refresh behavior.

## Failure isolation and security

- Probe success does not change the account circuit breaker or scheduler trial
  registry.
- One account's probe failure must not prevent other eligible accounts from
  being evaluated during the same due pass.
- Existing roster admission, binding fingerprint, login epoch, and capability
  gates remain mandatory before any request.
- Error text is redacted with the existing credential redaction helpers before
  persistence or Management exposure.
- The setting and status remain behind the existing Management API key.

## Testing strategy

### Pure decision tests

- exact seven-day alignment;
- ±2 minute lower and upper boundaries;
- just outside both boundaries;
- primary `used_percent == 0` qualifies;
- primary nonzero, missing, or missing five-hour window does not qualify;
- missing weekly window/reset does not send;
- confirmed same `weekly.reset_at` does not send;
- mismatch moves an armed account to manual-refresh-required;
- automatic refresh does not clear the manual hold;
- successful manual refresh re-arms only when predicates are valid.

### Runtime and persistence tests

- quota GET → activation POST → quota GET request order;
- request body contains `你好` only for the new activation mode;
- one account sends once for one weekly reset;
- repeated due wakes and concurrent calls produce one POST;
- restart after `Sending`, `Sent`, and `SentUnknown` verifies first and never
  resends blindly;
- post-send mismatch stops future automatic sends;
- one account failure does not block a second eligible account;
- circuit/trial state remains unchanged;
- missing activation maps load cleanly from an older state file.

### Management tests

- setting defaults to disabled;
- setting round-trips through protected status and update APIs;
- Chinese warning and manual-refresh explanation are present;
- status exposes sanitized activation state without credentials.

## Verification and rollout

The implementation should first run the pure decision and focused runtime
tests, then the broader Probe/runtime suites because this feature crosses
quota parsing, persistence, concurrency, and user-visible management state.
The local CGO toolchain must be able to compile the package before interpreting
those tests as meaningful.

No production code is part of this design-review change.
