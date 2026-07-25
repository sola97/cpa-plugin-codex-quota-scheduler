# Weekly Quota Reserve Policy

## Goal

Add three global scheduler settings that reserve part of every weekly Codex
account's quota: `enable_weekly_quota_reserve` (default `true`),
`weekly_quota_reserve_percent` (default `20`), and
`weekly_quota_reserve_unlock_window` (default `5h`). The switch disables the
policy without changing the configured values. A weekly account whose
remaining weekly quota is below the configured percentage and whose reset-credit
endpoint explicitly reports zero available reset credits is held as reserve
while its next weekly reset is farther away than the unlock window.

## Configuration

Add the three reserve settings to the persisted plugin configuration and the
Management UI settings form.

- Defaults: enabled, `20%`, and `5h`.
- Valid range: `0` through `100`.
- The switch disables the policy. A percentage of `0` also results in no
  account being held in reserve.
- The unlock window must be a positive duration. When the next weekly reset is
  less than this window away, the account is released and returns to normal
  scheduling.
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
state. The account also becomes eligible automatically when the reset enters
the unlock window, even before another refresh completes.

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

Place an enable switch, percentage field, and duration field in the existing
`调度设置` panel. The account card uses the existing unavailable reason
presentation with a dedicated reason key for accounts held by the policy and
shows the calculated unlock time. When the account is inside the unlock window,
the card reports that the reserve has been released and shows the next reset.

## Verification

Add focused tests for:

- default and boundary configuration validation;
- weekly remaining quota below, equal to, and above the threshold;
- zero, positive, and unknown reset-credit counts;
- unlock-window release before the next refresh;
- monthly-account exclusion;
- automatic re-admission after refreshed quota data changes;
- Management status reason and UI setting wiring;
- prevention of built-in fallback when the policy is the blocking reason.
