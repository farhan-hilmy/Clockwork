// Package httpapi exposes Clockwork over HTTP: health probes, the versioned
// JSON API under /api/v1, and (optionally) the built web UI.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/farhan-hilmy/clockwork/internal/apps"
	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

// Pinger checks a dependency for the readiness probe.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the services the HTTP layer needs.
type Deps struct {
	Tasks      *tasks.Service
	Executions *executions.Service
	Apps       *apps.Service
	DB         Pinger
	AdminToken string
	WebDir     string // directory with the built UI; empty disables UI serving
	Logger     *slog.Logger
	Version    string
}

// NewRouter builds the HTTP handler.
func NewRouter(d Deps) http.Handler {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(d.Logger))
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		if err := d.DB.Ping(ctx); err != nil {
			d.Logger.Warn("readiness check failed", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "database": "unreachable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "database": "ok"})
	})

	h := &taskHandlers{svc: d.Tasks, log: d.Logger}
	eh := &executionHandlers{svc: d.Executions, tasks: h}
	ah := &applicationHandlers{svc: d.Apps, tasks: h}
	r.Route("/api/v1", func(api chi.Router) {
		// The API description is public: it holds no data, and tools fetch it without credentials.
		api.Get("/openapi.json", serveOpenAPI)

		api.Group(func(api chi.Router) {
			api.Use(authenticate(d.AdminToken, d.Apps, d.Logger))
			api.Get("/meta", func(w http.ResponseWriter, req *http.Request) {
				p := principal(req)
				shell := d.Tasks.ShellEnabled() && p.Admin // applications can never use shell
				who := map[string]any{"type": "admin"}
				if !p.Admin {
					who = map[string]any{"type": "application", "application_id": p.App.ID, "application_name": p.App.Name}
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"version":       d.Version,
					"shell_enabled": shell,
					"runners":       runnersAvailable(shell),
					"principal":     who,
				})
			})
			api.Get("/tasks", h.list)
			api.Post("/tasks", h.create)
			api.Get("/tasks/{id}", h.get)
			api.Patch("/tasks/{id}", h.update)
			api.Delete("/tasks/{id}", h.delete)
			api.Post("/tasks/{id}/enable", h.setEnabled(true))
			api.Post("/tasks/{id}/disable", h.setEnabled(false))
			api.Post("/tasks/{id}/run", eh.run)
			api.Get("/executions", eh.list)
			api.Get("/executions/{id}", eh.get)

			api.Group(func(api chi.Router) {
				api.Use(adminOnly)
				api.Get("/applications", ah.list)
				api.Post("/applications", ah.create)
				api.Get("/applications/{id}", ah.get)
				api.Patch("/applications/{id}", ah.update)
				api.Delete("/applications/{id}", ah.delete)
				api.Get("/applications/{id}/keys", ah.listKeys)
				api.Post("/applications/{id}/keys", ah.createKey)
				api.Delete("/applications/{id}/keys/{keyID}", ah.revokeKey)
			})
		})
		api.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint", nil)
		})
		api.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed on this endpoint", nil)
		})
	})

	if d.WebDir != "" {
		r.Handle("/*", spaHandler(d.WebDir))
	}
	return r
}

func runnersAvailable(shell bool) []string {
	if shell {
		return []string{"http", "shell"}
	}
	return []string{"http"}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				return
			}
			// The query string is not logged: it may carry secrets in future endpoints.
			log.Info("http request",
				"method", r.Method, "path", r.URL.Path, "status", ww.Status(),
				"bytes", ww.BytesWritten(), "duration_ms", time.Since(start).Milliseconds(),
				"request_id", middleware.GetReqID(r.Context()))
		})
	}
}

// spaHandler serves files from dir and falls back to index.html for client-side routes.
func spaHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(dir, filepath.FromSlash(filepath.Clean("/"+r.URL.Path)))
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			http.ServeFile(w, r, filepath.Join(dir, "index.html"))
			return
		}
		fs.ServeHTTP(w, r)
	})
}
