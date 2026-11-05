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
	addr := flag.String("addr", ":8000", "HTTP 监听地址")
	dbPath := flag.String("db", "data/config.db", "SQLite 配置数据库路径")
	flag.Parse()

	// 在数据库打开前先使用内置日志配置；随后改用数据库中的持久化设置。
	logx.Init(config.Default().LogOptions())
	logger := logx.Module("main")

	db, err := store.Open(*dbPath)
	if err != nil {
		logger.Error("打开数据库失败", "err", err)
		// Open 可能已建表或跑过迁移才失败；失败路径同样要关库，
		// 让已完成的部分落盘，而不是留在 -wal 里。
		if closeErr := store.Close(db); closeErr != nil {
			logger.Warn("关闭数据库连接失败", "err", closeErr)
		}
		os.Exit(1)
	}
	// 监听中断/终止信号；每次软件重启均在该进程上下文中重建运行资源。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gatewayRuntime, err := runtime.New(ctx, db)
	if err != nil {
		logger.Error("初始化网关运行时失败", "err", err)
		// store.Open 已完成建表，schema 可能还在 -wal 里；退出前关库让它合并回主库。
		if closeErr := store.Close(db); closeErr != nil {
			logger.Warn("关闭数据库连接失败", "err", closeErr)
		}
		os.Exit(1)
	}
	logger = logx.Module("main")

	srv := &http.Server{Addr: *addr, Handler: web.Router(db, gatewayRuntime)}

	go func() {
		logger.Info("网关微服务启动", "addr", *addr, "url", "http://localhost"+*addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// 启动失败（如端口被占用）也要走关库流程：
			// 否则这次退出会把 schema 留在 -wal 里，主库看起来是空的。
			logger.Error("服务异常退出", "err", err)
			gatewayRuntime.Stop()
			if closeErr := store.Close(db); closeErr != nil {
				logger.Warn("关闭数据库连接失败", "err", closeErr)
			}
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	stop() // 恢复默认信号处理：再次 Ctrl+C 可强制退出
	logger.Info("正在关闭服务…")

	// 先停运行时：engine.Stop() 会等待全部链路 goroutine 退出，
	// 之后不再有采集侧的配置读取。
	gatewayRuntime.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("优雅关闭失败", "err", err)
		// 失败路径也要尝试关库：否则这次退出会留下未合并的 WAL。
		if closeErr := store.Close(db); closeErr != nil {
			logger.Warn("关闭数据库连接失败", "err", closeErr)
		}
		logx.Shutdown()
		os.Exit(1)
	}

	// HTTP 与采集都已停止，此时关闭数据库连接才是安全的。
	//
	// SQLite 只在最后一个连接正常关闭时执行检查点，把 WAL 合并回主库
	// 并删除 -wal / -shm。不关闭连接就退出，数据库会长期停留在
	// 「主库几乎是空的、数据全在 -wal 里」的状态——单独备份 config.db
	// 会得到一个空库（表数量为 0）。
	if err := store.Close(db); err != nil {
		logger.Warn("关闭数据库连接失败", "err", err)
	}

	logger.Info("服务已关闭")
	// 关闭日志写入器，确保文件缓冲落盘、句柄释放。
	logx.Shutdown()
}
