package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"time"

	"gateway/internal/api"
	"gateway/internal/logx"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"gorm.io/gorm"
)

//go:embed static
var staticFS embed.FS

// requestLogger 把 HTTP 请求日志接入 logx（mod=http 分支）。
func requestLogger(next http.Handler) http.Handler {
	log := logx.Module("http")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		defer func() {
			log.Info("HTTP",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"elapsed", time.Since(start).String(),
			)
		}()
		next.ServeHTTP(ww, r)
	})
}

// Router 组装静态页面与 REST API。
// runtime 管理可重启的链路引擎和 NATS 客户端。
func Router(db *gorm.DB, runtime api.RuntimeFacade) http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)

	s := api.New(db)
	if runtime != nil {
		s.Engine = runtime
	}
	r.Route("/api", func(r chi.Router) {
		r.Get("/models", s.ListModels)
		r.Post("/models", s.SaveModel)
		r.Delete("/models/{id}", s.DeleteModel)

		r.Get("/channels", s.ListChannels)
		r.Post("/channels", s.SaveChannel)
		r.Delete("/channels/{id}", s.DeleteChannel)

		r.Get("/realtime", s.Realtime)
		r.Post("/set", s.SetValue)
		r.Get("/comm-monitor", s.CommunicationMonitor)

		r.Get("/hardware", s.GetHardware)
		r.Get("/settings", s.GetSettings)
		r.Post("/settings", s.SaveSettings)
		r.Get("/system-info", s.GetSystemInfo)
		r.Post("/restart", s.Restart)

		// 链路引擎运行状态。
		r.Get("/engine/status", engineStatusHandler(runtime))

		// 系统日志出口：快照拉取 + SSE 实时推送。
		r.Get("/syslog", logx.SyslogHandler())
		r.Get("/syslog/stream", logx.SyslogStreamHandler())
	})

	sub, _ := fs.Sub(staticFS, "static")
	r.Handle("/*", http.FileServer(http.FS(sub)))
	return r
}

// engineStatusHandler 返回 GET /api/engine/status：各链路运行状态快照。
func engineStatusHandler(runtime api.RuntimeFacade) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var data any = []any{}
		if runtime != nil {
			data = runtime.Status()
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}
}
