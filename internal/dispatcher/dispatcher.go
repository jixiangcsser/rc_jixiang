package dispatcher

import (
	"context"
	"log/slog"
	"math"
	"rc_jixiang/internal/deliverer"
	"rc_jixiang/internal/goroutine"
	"rc_jixiang/internal/store"
	"time"
)

// maxDelay 是指数退避的上限，防止失败任务等待时间无限增长
const maxDelay = 10 * time.Minute

type Dispatcher struct {
	store        *store.Store
	deliverer    *deliverer.Deliverer
	workerCount  int
	pollInterval time.Duration
	logger       *slog.Logger
}

func New(s *store.Store, d *deliverer.Deliverer, workerCount int, pollInterval time.Duration, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:        s,
		deliverer:    d,
		workerCount:  workerCount,
		pollInterval: pollInterval,
		logger:       logger,
	}
}

// Run 是 Dispatcher 的主循环，阻塞运行直到 ctx 被取消。
// 每隔 pollInterval 触发一次 poll，从数据库认领待投递任务并分发给 Worker goroutine。
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.poll(ctx)
		}
	}
}

// poll 是单次轮询逻辑：认领 pending 任务，为每个任务启动一个 goroutine 执行投递。
//
// 并发模型：每个任务独占一个 goroutine，数量由 workerCount 限制（ClaimPending 的 limit 参数）。
// 设计取舍：用 goroutine-per-task 而非固定 worker pool，实现简单且够用；
// 若需精确控制并发上限，可改用 semaphore 或 buffered channel。
func (d *Dispatcher) poll(ctx context.Context) {
	notifications, err := d.store.ClaimPending(ctx, d.workerCount)
	if err != nil {
		d.logger.Error("dispatcher poll claim", "error", err)
		return
	}

	for _, n := range notifications {
		n := n // 捕获循环变量，避免所有 goroutine 共享同一个指针
		// 使用 GoWithLogger 替代裸 go，panic 时记录日志而不崩溃进程。
		// 任务保持 processing 状态，服务重启后由 RecoverProcessing 重置为 pending 并重试。
		goroutine.GoWithLogger(d.logger, func() {
			err := d.deliverer.Deliver(ctx, n)
			if err == nil {
				if markErr := d.store.MarkDone(ctx, n.ID); markErr != nil {
					d.logger.Error("dispatcher mark done", "id", n.ID, "error", markErr)
				}
				d.logger.Info("notification delivered", "id", n.ID)
				return
			}

			d.logger.Warn("notification delivery failed", "id", n.ID, "attempts", n.Attempts, "error", err)

			// n.Attempts 是本次投递前的值，newAttempts 是投递后应记录的次数
			newAttempts := n.Attempts + 1
			// 达到或超过 max_attempts 时不再重试，移入死信表
			moveToDeadLetter := newAttempts >= n.MaxAttempts

			// 退避延迟基于本次投递前的 attempts 计算，使第 1 次失败等 2s、第 2 次等 4s……
			delay := backoff(n.Attempts)
			nextRetry := time.Now().Add(delay)

			if markErr := d.store.MarkFailed(ctx, n.ID, err.Error(), nextRetry, moveToDeadLetter); markErr != nil {
				d.logger.Error("dispatcher mark failed", "id", n.ID, "error", markErr)
			}

			if moveToDeadLetter {
				d.logger.Warn("notification moved to dead letter", "id", n.ID, "attempts", newAttempts)
			}
		})
	}
}

// backoff 计算第 attempts 次失败后的等待时长，公式：min(2^attempts × 2s, maxDelay)
//
// attempts=0 → 2s，attempts=1 → 4s，attempts=2 → 8s，……，超过上限后恒为 10min。
//
// 注意：在浮点域完成上限判断后再转换为 time.Duration，
// 避免 math.Pow(2, 大数) 溢出 int64 导致结果变为负值或零。
func backoff(attempts int) time.Duration {
	seconds := math.Pow(2, float64(attempts)) * 2
	if seconds >= float64(maxDelay/time.Second) {
		return maxDelay
	}
	return time.Duration(seconds) * time.Second
}
