package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/farhan-hilmy/clockwork/internal/apps"
)

// Principal is who a request acts as: the operator (admin token) or one application (API key).
type Principal struct {
	Admin bool
	App   apps.Application // set when Admin is false
}

// AppID is the application the principal is confined to, or "" for the admin.
func (p Principal) AppID() string {
	if p.Admin {
		return ""
	}
	return p.App.ID
}

// owns reports whether the principal may see a resource belonging to appID.
func (p Principal) owns(appID string) bool { return p.Admin || p.App.ID == appID }

type principalKey struct{}

func principal(r *http.Request) Principal {
	p, _ := r.Context().Value(principalKey{}).(Principal)
	return p
}

// authenticate accepts either the admin token or an application's API key as a
// bearer token, and stores the resulting Principal in the request context.
func authenticate(adminToken string, keys *apps.Service, log *slog.Logger) func(http.Handler) http.Handler {
	want := []byte(adminToken)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			var p Principal
			switch {
			case !ok || got == "":
				unauthorized(w)
				return
			case subtle.ConstantTimeCompare([]byte(got), want) == 1:
				p = Principal{Admin: true}
			case strings.HasPrefix(got, apps.KeyPrefix):
				app, err := keys.Authenticate(r.Context(), got)
				switch {
				case errors.Is(err, apps.ErrUnauthorized):
					unauthorized(w)
					return
				case errors.Is(err, apps.ErrAppDisabled):
					writeError(w, http.StatusForbidden, "application_disabled", err.Error(), nil)
					return
				case err != nil:
					log.Error("api key lookup failed", "err", err, "request_id", middleware.GetReqID(r.Context()))
					writeError(w, http.StatusInternalServerError, "internal", "internal server error", nil)
					return
				}
				p = Principal{App: app}
			default:
				unauthorized(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
		})
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="clockwork"`)
	writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required: the admin token or an application API key", nil)
}

// adminOnly rejects application API keys.
func adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !principal(r).Admin {
			writeError(w, http.StatusForbidden, "forbidden", "this endpoint requires the admin token", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
