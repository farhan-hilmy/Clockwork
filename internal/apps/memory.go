package apps

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryRepository is an in-memory Repository for tests. TaskCount, if set, reports
// how many tasks an application owns (used for counts and for the delete check).
type MemoryRepository struct {
	mu        sync.Mutex
	apps      map[string]Application
	keys      map[string]Key
	hashes    map[string][]byte
	TaskCount func(appID string) int
}

// NewMemoryRepository returns a repository holding only the default application.
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		apps:   map[string]Application{DefaultID: {ID: DefaultID, Name: "clockwork", Description: "Default Clockwork application context", Enabled: true}},
		keys:   map[string]Key{},
		hashes: map[string][]byte{},
	}
}

func (m *MemoryRepository) withCount(a Application) Application {
	if m.TaskCount != nil {
		a.TaskCount = m.TaskCount(a.ID)
	}
	return a
}

func (m *MemoryRepository) List(context.Context) ([]Application, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Application, 0, len(m.apps))
	for _, a := range m.apps {
		out = append(out, m.withCount(a))
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].ID == DefaultID) != (out[j].ID == DefaultID) {
			return out[i].ID == DefaultID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (m *MemoryRepository) Get(_ context.Context, id string) (Application, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[id]
	if !ok {
		return Application{}, ErrNotFound
	}
	return m.withCount(a), nil
}

func (m *MemoryRepository) nameTaken(a Application) bool {
	for _, o := range m.apps {
		if o.ID != a.ID && o.Name == a.Name {
			return true
		}
	}
	return false
}

func (m *MemoryRepository) Create(_ context.Context, a Application) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nameTaken(a) {
		return ErrNameTaken
	}
	m.apps[a.ID] = a
	return nil
}

func (m *MemoryRepository) Update(_ context.Context, a Application) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.apps[a.ID]; !ok {
		return ErrNotFound
	}
	if m.nameTaken(a) {
		return ErrNameTaken
	}
	m.apps[a.ID] = a
	return nil
}

func (m *MemoryRepository) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.apps[id]; !ok {
		return ErrNotFound
	}
	if m.TaskCount != nil && m.TaskCount(id) > 0 {
		return ErrHasTasks
	}
	delete(m.apps, id)
	for kid, k := range m.keys {
		if k.ApplicationID == id {
			delete(m.keys, kid)
			delete(m.hashes, kid)
		}
	}
	return nil
}

func (m *MemoryRepository) CreateKey(_ context.Context, k Key, hash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[k.ID] = k
	m.hashes[k.ID] = hash
	return nil
}

func (m *MemoryRepository) ListKeys(_ context.Context, appID string) ([]Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Key{}
	for _, k := range m.keys {
		if k.ApplicationID == appID {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (m *MemoryRepository) RevokeKey(_ context.Context, appID, keyID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[keyID]
	if !ok || k.ApplicationID != appID {
		return ErrKeyNotFound
	}
	if k.RevokedAt == nil {
		k.RevokedAt = &at
		m.keys[keyID] = k
	}
	return nil
}

func (m *MemoryRepository) KeyByHash(_ context.Context, hash []byte) (Key, Application, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, h := range m.hashes {
		if bytes.Equal(h, hash) {
			k := m.keys[id]
			return k, m.apps[k.ApplicationID], nil
		}
	}
	return Key{}, Application{}, ErrKeyNotFound
}

func (m *MemoryRepository) TouchKey(_ context.Context, keyID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.keys[keyID]; ok {
		k.LastUsedAt = &at
		m.keys[keyID] = k
	}
	return nil
}
