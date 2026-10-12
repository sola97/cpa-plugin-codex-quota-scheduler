# Quota Reserve Toggle and Chinese Decision Logs

**Goal:** Require an explicit independent account switch before its quota reserves affect selection, and explain selection/protection decisions in Chinese.

**Architecture:** AccountAnnotation.QuotaReserveEnabled defaults to false and is exposed as quota_reserve_enabled in the account editor and annotation API. Gate immutable scheduler views, queue status, and fallback protection consistently. Capture readable diagnostics from the same pick-time snapshot before asynchronous logging.

**Spec:** User requested an explicit opt-in switch and Chinese logs containing effective or unsatisfied reasons. NUC deployment must persist; the user will configure account rules themselves.

## Tasks

- [x] Add account annotation switch, immutable-view gates and default-off regression tests.
- [x] Add management editor toggle, annotation roundtrip/persistence coverage, and disabled account-card state.
- [x] Add Chinese reason_text and quota_reserve_details without replacing stable machine reason codes.
- [x] Explain unknown quota, missing/nonweekly windows, base disabled, unmatched time rules, selected thresholds and blocked/allowed comparisons.
- [x] Capture diagnostics at pick time, retaining async log correctness through concurrent config updates.
- [x] Run full Go tests, focused race, vet, build, and rendered UI checks.
- [x] Commit feat(quota): add account protection switches and Chinese decision logs, then push main.
- [x] Back up and deploy to NUC, recreating main CPA together with its TUIC client sidecar, never pulling replacement images.
- [x] Verify default-off state, successful quota refresh, saved settings across recreation, and absence of synthetic test records.

## Verification Evidence

- Full `go test ./...`, `go vet ./...`, and focused `go test -race` passed on Debian with Go 1.26.
- Linux amd64 c-shared build passed.
- Real management handlers exercised in a temporary local browser harness: account A on/off and custom bases survive page reload; accounts B and C remain unchanged.
- Desktop 1440x1000 and mobile 390x844 rendered without horizontal overflow or account text overlap; mobile editor stays inside the viewport and save remains reachable.
- Disabled cards do not imply a live 0% reserve; enabled cards separately show each window's current and base percentages. Zero bases, unknown quota and monthly/nonweekly windows have explicit labels.
- Chinese saved-account and protection-reason logs render correctly; browser console has no errors or warnings.
- Temporary browser harness removed before committing.

## Deployment Evidence

- Feature commit `92872d8` pushed to origin/main and loaded on NUC as `0.3.2-sola97.92872d8`.
- Installed Linux amd64 library SHA256: `082e46fd5683b750743da03bc26a9f21e4be3c490aabd4b10bf33599adf3ba06`.
- Backup: `/home/ubuntu/docker-cpa/backups/quota-toggle-20261012T115720Z`, including previous library, config, compose, and persistent plugin state.
- Main CPA and its TUIC client recreated together with `--pull never`; network namespace linkage checked. No proxy endpoint, image, config or compose changes.
- A nonexistent-account marker with an enabled switch and custom window rules survived another paired container recreation; marker removed afterward. Real annotation and settings digests matched their pre-upgrade baselines.
- Three real accounts loaded successfully and three quota refresh success events observed. Two accounts available; one remains unavailable because its 5-hour quota is exhausted, matching its pre-upgrade state.
- Real account rules preserved: switches off, base reserves unchanged at 0%. No real account protection enabled by the deployment.
- All three existing plugins registered; main CPA and TUIC client running with restart policy `always`.
