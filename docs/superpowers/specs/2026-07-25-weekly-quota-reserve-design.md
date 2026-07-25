# Account-Scoped Weekly Quota Reserve Policy

Status: approved by the user on 2026-07-25.

## Goal

Allow the user to manually reserve weekly quota for selected Codex accounts.
The policy is account-scoped, not global: existing and newly discovered
accounts do not reserve quota until the user enables the policy in that
account's `编辑账号` dialog.

An enabled account defaults to reserving the last `20%` of its weekly quota and
releases that reserve when the next weekly reset is less than `5h` away. Both
values are configurable per account.

## Non-goals

- Do not enable reserve quota for every account through global scheduler
  settings.
- Do not modify CPA's source authentication JSON files or persisted disabled
  state.
- Do not apply this policy to monthly accounts.
- Do not change the existing account ordering or expiry-time scheduling rules.
- Do not add a second account-policy store alongside the existing account
  annotation store.

## Account configuration

Extend the existing account annotation model with an account-scoped weekly
quota reserve policy. The effective defaults are:

- enabled: `false`;
- reserve percent: `20`;
- unlock window: `5h`.

The switch controls whether the policy participates in scheduling. Disabling
the switch preserves the configured percent and unlock window so that enabling
it again restores the user's previous values.

The Management API accepts and returns the policy as part of the account
annotation payload. The UI uses hours for the unlock-window input, while the
backend converts the value to `time.Duration` before storing it in runtime
state. Persisted account annotations, normal plugin export, and plugin import
all include the policy.

Validation rules:

- reserve percent must be greater than `0` and no greater than `100`;
- unlock window must be greater than `0`;
- invalid account policy input returns a clear client error;
- an imported state containing an invalid enabled account policy is rejected
  instead of being silently normalized to another value.

An older account annotation without reserve fields is interpreted as disabled,
with `20%` and `5h` supplied as the edit-form defaults. No user-data schema
migration may turn existing accounts on automatically.

## Management UI

Remove the weekly quota reserve switch, percentage, and unlock window from the
global `调度设置` panel.

For a weekly account, add a `储备额度` section to the existing `编辑账号`
dialog:

- `启用储备额度` checkbox;
- `保留周额度` numeric percentage input, default `20`;
- `重置前解封` numeric hours input, default `5`.

The two numeric fields remain visible but disabled when the switch is off. Their
values are preserved. Monthly accounts do not expose an active reserve editor;
the UI explains that the setting applies only to weekly accounts.

An enabled account card shows a compact `储备 20%`-style marker. When the
account is actively held in reserve, the card uses the dedicated
`weekly_quota_reserve` unavailable reason and shows the current remaining
percentage, configured reserve percentage, and calculated unlock time. Inside
the unlock window, the card reports that the reserve is released and shows the
next weekly reset.

An exhausted account continues to show `weekly_exhausted` and its real reset
time. It must never promise an earlier reserve unlock that cannot make the
account usable.

## Eligibility rule

The policy can hold an account only when all of the following are true:

1. The account's reserve switch is enabled.
2. The account is classified as weekly and has a weekly `LongWindow`.
3. The quota record is fresh enough for normal scheduling decisions.
4. The weekly window has valid usage and reset data and is not exhausted.
5. The calculated remaining percentage is strictly below the account's
   configured reserve percentage.
6. `ResetCreditsAvailableCount` is present and equals zero.
7. The time remaining until the next weekly reset is greater than or equal to
   the account's configured unlock window.

The percentage comparison is strict. Remaining quota equal to the configured
reserve remains usable.

Missing or failed reset-credit data is unknown, not zero, and does not activate
the reserve. A positive reset-credit count also keeps the account eligible.

The unlock comparison is also strict. At exactly `5h` with a `5h` window the
account remains reserved; it is released only after the reset becomes less
than `5h` away.

The rule is dynamic. A refreshed quota record that raises the remaining quota
to the threshold, reports a positive reset-credit count, or changes other
eligibility data makes the account usable again without changing CPA's account
state. The account also becomes eligible automatically as time enters the
unlock window, without waiting for another quota refresh.

## Selection and status consistency

Production selection, fallback prevention, Management queue projection, and
account-card status use one shared reserve-state interpretation. The immutable
scheduler snapshot stores the policy facts needed for time-based release, and
all hot-path checks use the same inclusive blocked boundary:

```text
blocked while now <= unlock_at
released when now > unlock_at
```

This shared interpretation prevents the Management page from reporting an
account as reserved while production selection treats it as released.

Normal ordering remains unchanged. If another account is selectable, the
plugin selects it through the existing scheduling algorithm.

If no account can be selected and an active candidate is blocked by this
account-scoped reserve policy, the plugin must not delegate to CPA's built-in
`fill-first` fallback. The host does not know the plugin-local policy and could
choose the reserved account again. The scheduler request is rejected as
unavailable instead.

The fallback exception changes the frozen all-Excluded contract and must be
recorded in `docs/deviations.md`, including the host error semantics and a real
ABI regression test. Existing fill-first behavior remains unchanged when the
reserve policy is not the blocking reason.

## Persistence and immediate publication

The account policy uses the existing account annotation persistence path and
identity key. It does not introduce a separate map or modify CPA auth files.

A successful account PATCH follows this order:

1. parse and validate the complete account annotation patch;
2. persist the updated user-data state;
3. replace the in-memory annotation state;
4. rebuild and publish the immutable scheduler snapshot;
5. return success to the UI.

If persistence or validation fails, neither the in-memory annotation nor the
published scheduler snapshot changes.

Bulk annotation updates and imported plugin state also republish the scheduler
snapshot after their persisted state has been accepted. This makes account
priority and reserve changes effective on the next scheduler request rather
than after an unrelated quota refresh.

## Verification

Add focused tests for:

- old and new accounts default to reserve disabled while the edit form supplies
  `20%` and `5h`;
- only manually enabled accounts can enter reserve;
- per-account percentage and unlock-window overrides;
- remaining quota below, equal to, and above the configured percentage;
- zero, positive, and unknown reset-credit counts;
- exact unlock boundary remains blocked and the first time inside the window is
  released;
- monthly-account exclusion;
- full weekly exhaustion retains `weekly_exhausted` status and the true reset
  time;
- automatic re-admission after refreshed quota or reset-credit data changes;
- account PATCH validation, persistence, and restart loading;
- successful account PATCH immediately changes `schedulerPickPublished` and
  `handleSchedulerPick` behavior;
- import and export preserve valid account policies and reject invalid enabled
  policies;
- Management status reason, unlock text, edit-form defaults, and account marker;
- real ABI prevention of built-in fallback when reserve is the blocking reason;
- unchanged fill-first behavior when reserve is not the blocking reason.
