// Package sessionauth manages short-lived, scoped browser session grants.
package sessionauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

var (
	ErrConflict     = errors.New("device already has an authorized session")
	ErrUnauthorized = errors.New("session authorization is invalid or expired")
	ErrInvalid      = errors.New("invalid session grant")
)

const MaxTTL = 24 * time.Hour

type Grant struct {
	ID           string    `json:"session_id"`
	DeviceSerial string    `json:"device_serial"`
	ServiceID    string    `json:"service_id"`
	InstanceID   string    `json:"instance_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type entry struct {
	grant Grant
	hash  [32]byte
	done  chan struct{}
}

type Store struct {
	mu      sync.Mutex
	grants  map[string]*entry
	devices map[string]string
	now     func() time.Time
}

func NewStore() *Store {
	return &Store{grants: make(map[string]*entry), devices: make(map[string]string), now: time.Now}
}

func randomSecret(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// Issue reserves a device. A conflicting reservation is replaced only when preempt is true.
func (s *Store) Issue(serial, service, instance string, ttl time.Duration, preempt bool) (Grant, string, error) {
	if serial == "" || service == "" || instance == "" || ttl <= 0 || ttl > MaxTTL {
		return Grant{}, "", ErrInvalid
	}
	id, err := randomSecret(18)
	if err != nil {
		return Grant{}, "", err
	}
	token, err := randomSecret(32)
	if err != nil {
		return Grant{}, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if old := s.devices[serial]; old != "" {
		if !preempt {
			return Grant{}, "", ErrConflict
		}
		s.revokeLocked(old)
	}
	grant := Grant{ID: id, DeviceSerial: serial, ServiceID: service, InstanceID: instance, ExpiresAt: s.now().Add(ttl)}
	s.grants[id] = &entry{grant: grant, hash: sha256.Sum256([]byte(token)), done: make(chan struct{})}
	s.devices[serial] = id
	return grant, token, nil
}

// Validate requires both the secret and the complete intended scope.
func (s *Store) Validate(id, token, service, instance string) (Grant, <-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.grants[id]
	if e == nil {
		return Grant{}, nil, ErrUnauthorized
	}
	if !s.now().Before(e.grant.ExpiresAt) {
		s.revokeLocked(id)
		return Grant{}, nil, ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(token))
	if token == "" || subtle.ConstantTimeCompare(hash[:], e.hash[:]) != 1 || service != e.grant.ServiceID || instance != e.grant.InstanceID {
		return Grant{}, nil, ErrUnauthorized
	}
	return e.grant, e.done, nil
}

func (s *Store) Revoke(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokeLocked(id)
}

func (s *Store) RevokeDevice(serial string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokeLocked(s.devices[serial])
}

func (s *Store) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.grants {
		s.revokeLocked(id)
	}
}

func (s *Store) expireLocked() {
	now := s.now()
	for id, e := range s.grants {
		if !now.Before(e.grant.ExpiresAt) {
			s.revokeLocked(id)
		}
	}
}

func (s *Store) revokeLocked(id string) bool {
	e := s.grants[id]
	if e == nil {
		return false
	}
	delete(s.grants, id)
	if s.devices[e.grant.DeviceSerial] == id {
		delete(s.devices, e.grant.DeviceSerial)
	}
	close(e.done)
	return true
}
