package main

import "time"

type ProbePurpose string

const (
	ProbePurposeLegacyReset      ProbePurpose = "legacy_reset"
	ProbePurposeWeeklyActivation ProbePurpose = "weekly_activation"
)

type ProbeWindowKind string

const (
	ProbeWindowFiveHour ProbeWindowKind = "five_hour"
	ProbeWindowLong     ProbeWindowKind = "long"
)

type ProbeAttemptPhase string

const (
	ProbeAttemptPrepared    ProbeAttemptPhase = "prepared"
	ProbeAttemptSending     ProbeAttemptPhase = "sending"
	ProbeAttemptSent        ProbeAttemptPhase = "sent"
	ProbeAttemptSentUnknown ProbeAttemptPhase = "sent_unknown"
)

// ProbeAttemptSeam is storage-only until S6 owns probe transitions.
type ProbeAttemptSeam struct {
	Instance        AuthInstanceID    `json:"instance"`
	AttemptID       string            `json:"attempt_id"`
	Purpose         ProbePurpose      `json:"purpose,omitempty"`
	Windows         []ProbeWindowKind `json:"windows"`
	Phase           ProbeAttemptPhase `json:"phase"`
	SendFenceSeq    uint64            `json:"send_fence_seq"`
	CreatedAt       time.Time         `json:"created_at"`
	SentAt          *time.Time        `json:"sent_at,omitempty"`
	UsageEvidence   bool              `json:"usage_evidence,omitempty"`
	VerifyNotBefore time.Time         `json:"verify_not_before"`
	SuppressUntil   time.Time         `json:"suppress_until"`
}

// ProbeAttempt is the S6 crash-safe schema. Alias preserves every S3
// SentUnknown record without a lossy migration.
type ProbeAttempt = ProbeAttemptSeam
