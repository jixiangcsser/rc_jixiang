package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rc_jixiang/internal/config"
	"rc_jixiang/internal/deliverer"
	"rc_jixiang/internal/dispatcher"
	"rc_jixiang/internal/goroutine"
	"rc_jixiang/internal/server"
	"rc_jixiang/internal/store"
)

func main() {
	cfg := config.Load()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	st, err := store.New(cfg.DBPath)
	if err != nil {
		logger.Error("open store", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	// 根 context，收到退出信号后 cancel，所有子 goroutine 通过它感知关闭
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 崩溃恢复：将上次进程异常退出时卡在 processing 的任务重置为 pending，
	// 确保 at-least-once 语义——任务不会因进程崩溃而永久丢失。
	if err := st.RecoverProcessing(ctx); err != nil {
		logger.Error("recover processing", "error", err)
		os.Exit(1)
	}

	dlv := deliverer.New(cfg.HTTPTimeout)
	disp := dispatcher.New(st, dlv, cfg.WorkerCount, cfg.PollInterval, logger)
	handler := server.New(st, logger)

	// Dispatcher 在独立 goroutine 中运行，持续轮询并投递任务
	goroutine.GoWithLogger(logger, func() { disp.Run(ctx) })

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: handler,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	goroutine.GoWithLogger(logger, func() {
		logger.Info("server starting", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	})

	// 阻塞直到收到 SIGINT / SIGTERM
	<-quit
	logger.Info("shutting down")

	// 先取消根 context，通知 Dispatcher 停止轮询
	cancel()

	// 再给 HTTP Server 10 秒处理存量请求，之后强制关闭
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown", "error", err)
	}

	logger.Info("server stopped")
}
