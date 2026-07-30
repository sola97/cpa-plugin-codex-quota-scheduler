package main

import (
	"strings"
	"time"
)

func earliestAvailableResetCreditExpiry(quota ParsedQuota, now time.Time) time.Time {
	if quota.ResetCreditsAvailableCount == nil || *quota.ResetCreditsAvailableCount <= 0 {
		return time.Time{}
	}

	var earliest time.Time
	for _, credit := range quota.ResetCredits {
		if !credit.UsedAt.IsZero() || !credit.ExpiresAt.After(now) {
			continue
		}
		status := strings.TrimSpace(credit.Status)
		if status != "" && !strings.EqualFold(status, "available") {
			continue
		}
		if earliest.IsZero() || credit.ExpiresAt.Before(earliest) {
			earliest = credit.ExpiresAt
		}
	}
	return earliest
}

func accountConsumptionDeadline(naturalResetAt, resetCreditExpiry time.Time) (time.Time, bool) {
	if naturalResetAt.IsZero() || resetCreditExpiry.IsZero() || !resetCreditExpiry.Before(naturalResetAt) {
		return naturalResetAt, false
	}
	return resetCreditExpiry, true
}

func accountViewConsumptionDeadline(account AccountView) time.Time {
	if !account.ConsumeBy.IsZero() {
		return account.ConsumeBy
	}
	return account.Expiry
}
