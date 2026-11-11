// Package apps manages applications (the integration contexts that own tasks)
// and their API keys.
//
// An application authenticates with an API key of the form "cwk_<random>". Only
// a SHA-256 hash of each key is stored: keys are 256-bit random values, so a
// fast hash is enough, and a database leak does not reveal usable keys.
package apps

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/farhan-hilmy/clockwork/internal/ids"
)

// DefaultID is the application for tasks created directly in Clockwork.
// It cannot be disabled or deleted, and it has no API keys.
const DefaultID = "app_clockwork"

// KeyPrefix starts every API key, so keys are recognisable in logs and secret scanners.
const KeyPrefix = "cwk_"

// Application is an integration context. Every task belongs to exactly one.
type Application struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	TaskCount   int       `json:"task_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Key describes an API key. The key itself is never returned after creation.
type Key struct {
	ID            string     `json:"id"`
	ApplicationID string     `json:"application_id"`
	Name          string     `json:"name"`
	Prefix        string     `json:"prefix"` // e.g. "cwk_4fj2k9", to tell keys apart
	CreatedAt     time.Time  `json:"created_at"`
	LastUsedAt    *time.Time `json:"last_used_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
}

// NewKey is returned once, when a key is created.
type NewKey struct {
	Key
	Secret string `json:"key"` // the full API key; store it now, it cannot be retrieved again
}

// Errors returned by the service.
var (
	ErrNotFound     = errors.New("application not found")
	ErrKeyNotFound  = errors.New("api key not found")
	ErrNameTaken    = errors.New("an application with this name already exists")
	ErrHasTasks     = errors.New("the application still has tasks; delete or move them first")
	ErrDefaultApp   = errors.New("the default clockwork application cannot be changed this way")
	ErrUnauthorized = errors.New("invalid or revoked api key")
	ErrAppDisabled  = errors.New("the application for this api key is disabled")
)

// ValidationError reports invalid fields, keyed like the task validation errors.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return fmt.Sprintf("validation failed: %v", e.Fields) }

// CreateInput registers an application.
type CreateInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// UpdateInput changes an application. Nil fields are left alone.
type UpdateInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Enabled     *bool   `json:"enabled"`
}

// Repository persists applications and keys.
type Repository interface {
	List(ctx context.Context) ([]Application, error)
	Get(ctx context.Context, id string) (Application, error)
	Create(ctx context.Context, a Application) error
	Update(ctx context.Context, a Application) error
	Delete(ctx context.Context, id string) error

	CreateKey(ctx context.Context, k Key, hash []byte) error
	ListKeys(ctx context.Context, appID string) ([]Key, error)
	RevokeKey(ctx context.Context, appID, keyID string, at time.Time) error
	// KeyByHash returns the key with this hash and its application.
	KeyByHash(ctx context.Context, hash []byte) (Key, Application, error)
	TouchKey(ctx context.Context, keyID string, at time.Time) error
}

// Service manages applications and authenticates API keys.
type Service struct {
	repo Repository
	now  func() time.Time
}

// NewService builds a Service. now may be nil, meaning time.Now.
func NewService(repo Repository, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, now: func() time.Time { return now().UTC().Truncate(time.Microsecond) }}
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func validateName(name string, fe map[string]string) {
	switch {
	case name == "":
		fe["name"] = "is required"
	case utf8.RuneCountInString(name) > 100:
		fe["name"] = "must be at most 100 characters"
	case !nameRE.MatchString(name):
		fe["name"] = "use lowercase letters, digits, '.', '_' and '-', starting with a letter or digit"
	}
}

func fieldErr(fe map[string]string) error {
	if len(fe) == 0 {
		return nil
	}
	return &ValidationError{Fields: fe}
}

// List returns all applications, the default one first.
func (s *Service) List(ctx context.Context) ([]Application, error) { return s.repo.List(ctx) }

// Get returns one application.
func (s *Service) Get(ctx context.Context, id string) (Application, error) {
	return s.repo.Get(ctx, id)
}

// Create registers an application. It starts enabled and without keys.
func (s *Service) Create(ctx context.Context, in CreateInput) (Application, error) {
	now := s.now()
	a := Application{
		ID:          ids.New("app"),
		Name:        strings.TrimSpace(in.Name),
		Description: strings.TrimSpace(in.Description),
		Enabled:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	fe := map[string]string{}
	validateName(a.Name, fe)
	if utf8.RuneCountInString(a.Description) > 2000 {
		fe["description"] = "must be at most 2000 characters"
	}
	if err := fieldErr(fe); err != nil {
		return Application{}, err
	}
	if err := s.repo.Create(ctx, a); err != nil {
		return Application{}, err
	}
	return a, nil
}

// Update renames, describes, enables or disables an application.
// Disabling stops its API keys from working and its tasks from being scheduled.
func (s *Service) Update(ctx context.Context, id string, in UpdateInput) (Application, error) {
	a, err := s.repo.Get(ctx, id)
	if err != nil {
		return Application{}, err
	}
	if id == DefaultID && (in.Name != nil || in.Enabled != nil) {
		return Application{}, ErrDefaultApp
	}
	fe := map[string]string{}
	if in.Name != nil {
		a.Name = strings.TrimSpace(*in.Name)
		validateName(a.Name, fe)
	}
	if in.Description != nil {
		a.Description = strings.TrimSpace(*in.Description)
		if utf8.RuneCountInString(a.Description) > 2000 {
			fe["description"] = "must be at most 2000 characters"
		}
	}
	if in.Enabled != nil {
		a.Enabled = *in.Enabled
	}
	if err := fieldErr(fe); err != nil {
		return Application{}, err
	}
	a.UpdatedAt = s.now()
	if err := s.repo.Update(ctx, a); err != nil {
		return Application{}, err
	}
	return a, nil
}

// Delete removes an application and its keys. It fails while the application owns tasks.
func (s *Service) Delete(ctx context.Context, id string) error {
	if id == DefaultID {
		return ErrDefaultApp
	}
	return s.repo.Delete(ctx, id)
}

// CreateKey issues a new API key for an application. The returned Secret is the
// only time the key is available.
func (s *Service) CreateKey(ctx context.Context, appID, name string) (NewKey, error) {
	if appID == DefaultID {
		return NewKey{}, ErrDefaultApp
	}
	if _, err := s.repo.Get(ctx, appID); err != nil {
		return NewKey{}, err
	}
	name = strings.TrimSpace(name)
	fe := map[string]string{}
	switch {
	case name == "":
		fe["name"] = "is required"
	case utf8.RuneCountInString(name) > 100:
		fe["name"] = "must be at most 100 characters"
	}
	if err := fieldErr(fe); err != nil {
		return NewKey{}, err
	}
	secret := generateKey()
	k := Key{
		ID:            ids.New("key"),
		ApplicationID: appID,
		Name:          name,
		Prefix:        secret[:len(KeyPrefix)+6],
		CreatedAt:     s.now(),
	}
	if err := s.repo.CreateKey(ctx, k, HashKey(secret)); err != nil {
		return NewKey{}, err
	}
	return NewKey{Key: k, Secret: secret}, nil
}

// ListKeys returns an application's keys, newest first, including revoked ones.
func (s *Service) ListKeys(ctx context.Context, appID string) ([]Key, error) {
	if _, err := s.repo.Get(ctx, appID); err != nil {
		return nil, err
	}
	return s.repo.ListKeys(ctx, appID)
}

// RevokeKey stops a key from working. Revoking is permanent.
func (s *Service) RevokeKey(ctx context.Context, appID, keyID string) error {
	return s.repo.RevokeKey(ctx, appID, keyID, s.now())
}

// Authenticate resolves an API key to its application.
func (s *Service) Authenticate(ctx context.Context, secret string) (Application, error) {
	if !strings.HasPrefix(secret, KeyPrefix) || len(secret) > 200 {
		return Application{}, ErrUnauthorized
	}
	k, a, err := s.repo.KeyByHash(ctx, HashKey(secret))
	if errors.Is(err, ErrKeyNotFound) || (err == nil && k.RevokedAt != nil) {
		return Application{}, ErrUnauthorized
	}
	if err != nil {
		return Application{}, err
	}
	if !a.Enabled {
		return Application{}, ErrAppDisabled
	}
	// Best effort: a failed last-used update must not fail the request.
	_ = s.repo.TouchKey(ctx, k.ID, s.now())
	return a, nil
}

// HashKey is how keys are stored and looked up.
func HashKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

var keyEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func generateKey() string {
	b := make([]byte, 32) // 256 bits
	_, _ = rand.Read(b)   // crypto/rand.Read never fails on supported platforms
	return KeyPrefix + keyEncoding.EncodeToString(b)
}
