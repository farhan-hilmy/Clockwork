package executions

import (
	"context"
	"sort"
	"sync"

	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

// TaskLookup resolves a task by ID, returning tasks.ErrNotFound when it does not exist.
type TaskLookup func(ctx context.Context, id string) (tasks.Task, error)

// MemoryRepository is an in-memory Repository for tests.
type MemoryRepository struct {
	mu     sync.Mutex
	execs  map[string]Execution
	lookup TaskLookup
}

// NewMemoryRepository returns an empty repository that resolves tasks with lookup.
func NewMemoryRepository(lookup TaskLookup) *MemoryRepository {
	return &MemoryRepository{execs: map[string]Execution{}, lookup: lookup}
}

func (m *MemoryRepository) Get(_ context.Context, id string) (Execution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.execs[id]
	if !ok {
		return Execution{}, ErrNotFound
	}
	return e, nil
}

func (m *MemoryRepository) List(_ context.Context, p ListParams) ([]Execution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Execution
	for _, e := range m.execs {
		if (p.TaskID != "" && e.TaskID != p.TaskID) || (p.Status != "" && e.Status != p.Status) ||
			(p.Cursor != "" && e.ID >= p.Cursor) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

func (m *MemoryRepository) Enqueue(ctx context.Context, e Execution) (Execution, error) {
	t, err := m.lookup(ctx, e.TaskID)
	if err != nil {
		return Execution{}, err
	}
	e.ApplicationID, e.TaskName = t.ApplicationID, t.Name
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execs[e.ID] = e
	return e, nil
}
