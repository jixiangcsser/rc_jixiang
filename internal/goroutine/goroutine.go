// Package goroutine 提供带 panic 兜底的 goroutine 启动工具。
// 直接使用 go func() 时，goroutine 内的 panic 会让整个进程崩溃，且没有任何日志。
// 使用 Go / GoWithLogger 替代裸 go，可以在 panic 时记录日志并安全退出该 goroutine，
// 而不会影响其他 goroutine 和主进程。
package goroutine

import "log/slog"

// Go 启动一个带 recover 的 goroutine。
// panic 发生时通过 slog 默认 logger 记录错误，goroutine 正常退出。
func Go(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("goroutine panic", "panic", r)
			}
		}()
		fn()
	}()
}

// GoWithLogger 与 Go 相同，但使用调用方传入的 logger 记录 panic 信息，
// 便于在有结构化日志上下文（如 request-id、component 字段）的场景中使用。
func GoWithLogger(logger *slog.Logger, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("goroutine panic", "panic", r)
			}
		}()
		fn()
	}()
}
