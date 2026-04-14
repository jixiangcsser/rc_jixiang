package dispatcher

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"rc_jixiang/internal/deliverer"
	"rc_jixiang/internal/model"
	"rc_jixiang/internal/store"
)

// ---- backoff 单元测试 -------------------------------------------------------

func TestBackoff(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{3, 16 * time.Second},
		{4, 32 * time.Second},
		{100, maxDelay}, // 超大值应被截断
	}
	for _, tc := range cases {
		got := backoff(tc.attempts)
		if got != tc.want {
			t.Errorf("backoff(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

func TestBackoff_NeverExceedsMaxDelay(t *testing.T) {
	for attempts := 0; attempts <= 200; attempts++ {
		if got := backoff(attempts); got > maxDelay {
			t.Errorf("backoff(%d) = %v, exceeds maxDelay %v", attempts, got, maxDelay)
		}
	}
}

// ---- poll 集成测试（真实 store + httptest.Server 模拟供应商） ----------------

func newTestDispatcher(t *testing.T, vendorURL string, maxAttempts int) (*Dispatcher, *store.Store, string) {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	dlv := deliverer.New(5 * time.Second)
	disp := New(s, dlv, 10, 1*time.Hour, logger) // pollInterval 设超长，测试中手动调用 poll

	now := time.Now().UTC()
	n := &model.Notification{
		ID:          "test-id",
		VendorID:    "vendor",
		URL:         vendorURL,
		Method:      "POST",
		Headers:     map[string]string{},
		Body:        `{"k":"v"}`,
		Status:      model.StatusPending,
		MaxAttempts: maxAttempts,
		NextRetryAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	ctx := context.Background()
	if err := s.Insert(ctx, n); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return disp, s, n.ID
}

// TC: 供应商返回 2xx → 任务标记为 done
func TestPoll_DeliverySuccess(t *testing.T) {
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer vendor.Close()

	disp, s, id := newTestDispatcher(t, vendor.URL, 3)
	ctx := context.Background()

	disp.poll(ctx)
	// poll 内部用 goroutine 投递，等待完成
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := s.GetByID(ctx, id)
		if n != nil && n.Status == model.StatusDone {
			return // 通过
		}
		time.Sleep(50 * time.Millisecond)
	}
	n, _ := s.GetByID(ctx, id)
	t.Errorf("want status=done, got %s", n.Status)
}

// TC: 供应商返回 5xx → 任务重置为 pending，attempts+1，next_retry_at 推迟
func TestPoll_DeliveryFailure_Retry(t *testing.T) {
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer vendor.Close()

	disp, s, id := newTestDispatcher(t, vendor.URL, 5)
	ctx := context.Background()

	before := time.Now()
	disp.poll(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := s.GetByID(ctx, id)
		if n != nil && n.Status == model.StatusPending && n.Attempts == 1 {
			// next_retry_at 应被推迟到未来（退避生效）
			if !n.NextRetryAt.After(before) {
				t.Errorf("next_retry_at not pushed forward: %v", n.NextRetryAt)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	n, _ := s.GetByID(ctx, id)
	t.Errorf("want status=pending attempts=1, got status=%s attempts=%d", n.Status, n.Attempts)
}

// TC: 达到 max_attempts → 任务标记为 failed，写入 dead_letters
func TestPoll_DeliveryFailure_DeadLetter(t *testing.T) {
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer vendor.Close()

	// max_attempts=1：第一次失败就应进死信
	disp, s, id := newTestDispatcher(t, vendor.URL, 1)
	ctx := context.Background()

	disp.poll(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := s.GetByID(ctx, id)
		if n != nil && n.Status == model.StatusFailed {
			dls, err := s.ListDeadLetters(ctx)
			if err != nil {
				t.Fatalf("list dead letters: %v", err)
			}
			if len(dls) != 1 {
				t.Errorf("want 1 dead letter, got %d", len(dls))
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	n, _ := s.GetByID(ctx, id)
	t.Errorf("want status=failed, got %s", n.Status)
}

// TC: processing 状态的任务不会被再次认领（防止重复投递）
func TestPoll_ProcessingNotReclaimed(t *testing.T) {
	callCount := 0
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer vendor.Close()

	disp, _, _ := newTestDispatcher(t, vendor.URL, 3)
	ctx := context.Background()

	// 第一次 poll 认领并开始投递
	disp.poll(ctx)
	// 立即再次 poll，此时任务已是 processing，不应被再次认领
	disp.poll(ctx)

	time.Sleep(300 * time.Millisecond)
	if callCount > 1 {
		t.Errorf("task delivered %d times, want exactly 1", callCount)
	}
}
