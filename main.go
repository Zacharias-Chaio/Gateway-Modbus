package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gateway/internal/config"
	"gateway/internal/logx"
	"gateway/internal/runtime"
	"gateway/internal/store"
	"gateway/internal/web"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	dbPath := flag.String("db", "data/config.db", "SQLite 配置数据库路径")
	flag.Parse()

	// 在数据库打开前先使用内置日志配置；随后改用数据库中的持久化设置。
	logx.Init(config.Default().LogOptions())
	logger := logx.Module("main")

	db, err := store.Open(*dbPath)
	if err != nil {
		logger.Error("打开数据库失败", "err", err)
		os.Exit(1)
	}
	// 监听中断/终止信号；每次软件重启均在该进程上下文中重建运行资源。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gatewayRuntime, err := runtime.New(ctx, db)
	if err != nil {
		logger.Error("初始化网关运行时失败", "err", err)
		os.Exit(1)
	}
	logger = logx.Module("main")

	srv := &http.Server{Addr: *addr, Handler: web.Router(db, gatewayRuntime)}

	go func() {
		logger.Info("网关微服务启动", "addr", *addr, "url", "http://localhost"+*addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("服务异常退出", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	stop() // 恢复默认信号处理：再次 Ctrl+C 可强制退出
	logger.Info("正在关闭服务…")

	gatewayRuntime.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("优雅关闭失败", "err", err)
		os.Exit(1)
	}
	logger.Info("服务已关闭")
}
