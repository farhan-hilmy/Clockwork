package tasks

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// MemoryRepository is an in-memory Repository for tests. It enforces the same
// uniqueness and existence rules as the Postgres schema.
type MemoryRepository struct {
	mu    sync.Mutex
	tasks map[string]Task
	apps  map[string]bool
}

// NewMemoryRepository returns an empty repository that knows the given application IDs
// (the default application is always present).
func NewMemoryRepository(appIDs ...string) *MemoryRepository {
	apps := map[string]bool{DefaultApplicationID: true}
	for _, id := range appIDs {
		apps[id] = true
	}
	return &MemoryRepository{tasks: map[string]Task{}, apps: apps}
}

func (m *MemoryRepository) nameTaken(t Task) bool {
	for _, o := range m.tasks {
		if o.ID != t.ID && o.ApplicationID == t.ApplicationID && o.Name == t.Name {
			return true
		}
	}
	return false
}

func (m *MemoryRepository) Create(_ context.Context, t Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.apps[t.ApplicationID] {
		return &ValidationError{Fields: map[string]string{"application_id": "unknown application"}}
	}
	if m.nameTaken(t) {
		return ErrConflict
	}
	m.tasks[t.ID] = t
	return nil
}

func (m *MemoryRepository) Get(_ context.Context, id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, ErrNotFound
	}
	return t, nil
}

func (m *MemoryRepository) List(_ context.Context, p ListParams) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Task
	for _, t := range m.tasks {
		if p.ApplicationID != "" && t.ApplicationID != p.ApplicationID {
			continue
		}
		if p.Enabled != nil && t.Enabled != *p.Enabled {
			continue
		}
		if p.Query != "" && !strings.Contains(strings.ToLower(t.Name), strings.ToLower(p.Query)) {
			continue
		}
		if p.Cursor != "" && t.ID >= p.Cursor {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

func (m *MemoryRepository) Update(_ context.Context, t Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[t.ID]; !ok {
		return ErrNotFound
	}
	if m.nameTaken(t) {
		return ErrConflict
	}
	m.tasks[t.ID] = t
	return nil
}

func (m *MemoryRepository) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[id]; !ok {
		return ErrNotFound
	}
	delete(m.tasks, id)
	return nil
}

func (m *MemoryRepository) ApplicationExists(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.apps[id], nil
}

// Raw returns the stored (unredacted) task, for asserting that secrets are kept.
func (m *MemoryRepository) Raw(id string) (Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	return t, ok
}
