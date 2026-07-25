# Weekly Quota Reserve Policy

## Goal

Add a global scheduler setting that reserves part of every weekly Codex
account's quota. The default reserve is `0%`, which preserves the current
behavior. When configured to `20%`, a weekly account whose remaining weekly
quota is below `20%` and whose reset-credit endpoint explicitly reports zero
available reset credits must not be selected by the plugin scheduler.

## Configuration

Add `weekly_quota_reserve_percent` to the persisted plugin configuration and
the Management UI settings form.

- Default: `0`.
- Valid range: `0` through `100`.
- `0` disables the policy.
- The comparison is strict: remaining quota equal to the configured value is
  still usable; only a lower value is blocked.
- The setting is global and applies to all accounts classified as weekly.

The value is included in settings read/write, import, export, and the normal
YAML configuration path.

## Eligibility rule

The policy blocks an account only when all of the following are true:

1. The account family is weekly and it has a weekly `LongWindow`.
2. `LongWindow.UsedPercent` is present and the calculated remaining percentage
   is below the configured reserve.
3. `ResetCreditsAvailableCount` is present and equals zero.

Missing or failed reset-credit data is treated as unknown, not as zero. This
avoids disabling an account merely because the auxiliary endpoint failed.
Monthly accounts and accounts with the policy disabled are unaffected.

The rule is dynamic. A later successful refresh that raises the remaining
quota to the threshold or reports a positive reset-credit count makes the
account eligible again without changing CPA's persisted account-disabled
state.

## Scheduler and fallback behavior

The eligibility helper is shared by production selection and Management queue
projection so the account's displayed availability and actual selection cannot
disagree.

If another account is selectable, normal plugin selection continues. If no
account can be selected and any candidate is blocked by this policy, the
plugin must not delegate to CPA's built-in `fill-first` fallback, because the
host does not understand this plugin-local policy and could select the blocked
account again. The scheduler request is rejected as unavailable instead.

Existing `fill-first` fallback behavior remains unchanged when this policy is
not the reason all candidates are unavailable.

## UI

Place a numeric field in the existing `调度设置` panel with a concise Chinese
description explaining that it is the weekly quota reserve percentage and
that `0` disables the rule. The account card uses the existing unavailable
reason presentation with a dedicated reason key for accounts blocked by the
policy.

## Verification

Add focused tests for:

- default and boundary configuration validation;
- weekly remaining quota below, equal to, and above the threshold;
- zero, positive, and unknown reset-credit counts;
- monthly-account exclusion;
- automatic re-admission after refreshed quota data changes;
- Management status reason and UI setting wiring;
- prevention of built-in fallback when the policy is the blocking reason.
