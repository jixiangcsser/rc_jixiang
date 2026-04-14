package model

import "time"

type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusDone       Status = "done"
	StatusFailed     Status = "failed"
)

type Notification struct {
	ID          string
	VendorID    string
	URL         string
	Method      string
	Headers     map[string]string
	Body        string
	Status      Status
	Attempts    int
	MaxAttempts int
	NextRetryAt time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastError   string
}

type DeadLetter struct {
	ID             string
	NotificationID string
	VendorID       string
	URL            string
	LastError      string
	Attempts       int
	CreatedAt      time.Time
}
