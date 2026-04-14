package store

import (
	"context"
	"testing"
	"time"

	"rc_jixiang/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newNotification(id string) *model.Notification {
	now := time.Now().UTC().Truncate(time.Second)
	return &model.Notification{
		ID:          id,
		VendorID:    "vendor1",
		URL:         "https://example.com/hook",
		Method:      "POST",
		Headers:     map[string]string{"Content-Type": "application/json"},
		Body:        `{"key":"value"}`,
		Status:      model.StatusPending,
		Attempts:    0,
		MaxAttempts: 3,
		NextRetryAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func TestInsertAndGetByID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n := newNotification("id-001")
	if err := s.Insert(ctx, n); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.GetByID(ctx, "id-001")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected notification, got nil")
	}
	if got.ID != n.ID {
		t.Errorf("id: want %s, got %s", n.ID, got.ID)
	}
	if got.VendorID != n.VendorID {
		t.Errorf("vendor_id: want %s, got %s", n.VendorID, got.VendorID)
	}
	if got.Headers["Content-Type"] != "application/json" {
		t.Errorf("headers not preserved, got %v", got.Headers)
	}
}

func TestGetByID_NotFound(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetByID(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestClaimPending(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"id-a", "id-b", "id-c"} {
		if err := s.Insert(ctx, newNotification(id)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	claimed, err := s.ClaimPending(ctx, 2)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("want 2 claimed, got %d", len(claimed))
	}
	for _, n := range claimed {
		if n.Status != model.StatusProcessing {
			t.Errorf("want processing, got %s", n.Status)
		}
	}

	// claiming again should return the remaining one
	claimed2, err := s.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("claim2: %v", err)
	}
	if len(claimed2) != 1 {
		t.Fatalf("want 1 remaining, got %d", len(claimed2))
	}
}

func TestClaimPending_FutureRetrySkipped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n := newNotification("future")
	n.NextRetryAt = time.Now().Add(1 * time.Hour)
	if err := s.Insert(ctx, n); err != nil {
		t.Fatalf("insert: %v", err)
	}

	claimed, err := s.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("want 0 claimed (future retry), got %d", len(claimed))
	}
}

func TestMarkDone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n := newNotification("id-done")
	if err := s.Insert(ctx, n); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.MarkDone(ctx, "id-done"); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	got, err := s.GetByID(ctx, "id-done")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.StatusDone {
		t.Errorf("want done, got %s", got.Status)
	}
}

func TestMarkFailed_Retry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n := newNotification("id-retry")
	if err := s.Insert(ctx, n); err != nil {
		t.Fatalf("insert: %v", err)
	}

	nextRetry := time.Now().Add(4 * time.Second)
	if err := s.MarkFailed(ctx, "id-retry", "timeout", nextRetry, false); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	got, err := s.GetByID(ctx, "id-retry")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.StatusPending {
		t.Errorf("want pending (retry), got %s", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("want attempts=1, got %d", got.Attempts)
	}
	if got.LastError != "timeout" {
		t.Errorf("want last_error=timeout, got %s", got.LastError)
	}
}

func TestMarkFailed_DeadLetter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n := newNotification("id-dead")
	if err := s.Insert(ctx, n); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.MarkFailed(ctx, "id-dead", "permanent error", time.Time{}, true); err != nil {
		t.Fatalf("mark failed dead letter: %v", err)
	}

	got, err := s.GetByID(ctx, "id-dead")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.StatusFailed {
		t.Errorf("want failed, got %s", got.Status)
	}

	dls, err := s.ListDeadLetters(ctx)
	if err != nil {
		t.Fatalf("list dead letters: %v", err)
	}
	if len(dls) != 1 {
		t.Fatalf("want 1 dead letter, got %d", len(dls))
	}
	if dls[0].NotificationID != "id-dead" {
		t.Errorf("dead letter notification_id: want id-dead, got %s", dls[0].NotificationID)
	}
}

func TestRecoverProcessing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n := newNotification("id-processing")
	if err := s.Insert(ctx, n); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Simulate a crash: manually claim it so it becomes 'processing'
	claimed, err := s.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Status != model.StatusProcessing {
		t.Fatal("expected one processing notification")
	}

	// Recover should reset it to pending
	if err := s.RecoverProcessing(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}

	got, err := s.GetByID(ctx, "id-processing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.StatusPending {
		t.Errorf("want pending after recovery, got %s", got.Status)
	}
}

func TestListByStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"a", "b"} {
		if err := s.Insert(ctx, newNotification(id)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	if err := s.MarkDone(ctx, "b"); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	pending, err := s.ListByStatus(ctx, "pending")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "a" {
		t.Errorf("expected 1 pending, got %d", len(pending))
	}

	done, err := s.ListByStatus(ctx, "done")
	if err != nil {
		t.Fatalf("list done: %v", err)
	}
	if len(done) != 1 || done[0].ID != "b" {
		t.Errorf("expected 1 done, got %d", len(done))
	}
}
