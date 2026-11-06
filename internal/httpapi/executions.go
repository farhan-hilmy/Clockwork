package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/farhan-hilmy/clockwork/internal/executions"
)

type executionHandlers struct {
	svc   *executions.Service
	tasks *taskHandlers // shared error mapping
}

func (h *executionHandlers) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := executions.ListParams{TaskID: q.Get("task_id"), Status: q.Get("status"), Cursor: q.Get("cursor")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "invalid_parameter", "limit must be an integer between 1 and 200", nil)
			return
		}
		p.Limit = n
	}
	if p.Status != "" && !executions.ValidStatus(p.Status) {
		writeError(w, http.StatusBadRequest, "invalid_parameter", "status is not a valid execution status", nil)
		return
	}
	page, err := h.svc.List(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *executionHandlers) get(w http.ResponseWriter, r *http.Request) {
	e, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// run queues a manual run of a task. 202: the run happens asynchronously.
func (h *executionHandlers) run(w http.ResponseWriter, r *http.Request) {
	e, err := h.svc.RunNow(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/executions/"+e.ID)
	writeJSON(w, http.StatusAccepted, e)
}

func (h *executionHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, executions.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error(), nil)
		return
	}
	h.tasks.fail(w, r, err) // maps tasks.ErrNotFound and internal errors
}
