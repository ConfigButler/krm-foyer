package session

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Memory is a Store in this process's memory. It suits one replica: a second
// replica would not see its sessions, and a restart ends them all.
type Memory struct {
	now       func() time.Time
	mu        sync.Mutex
	sessions  map[Key]entry
	lastSweep time.Time
}

type entry struct {
	session Session
	expires time.Time
}

// sweepInterval is how often Create drops expired sessions.
const sweepInterval = time.Minute

// MaxMemorySessions bounds the sessions a Memory holds. Each holds an ID token, a
// few kilobytes at most, so this is tens of megabytes. Only a user the issuer signed
// in can start one, but one such user can start any number.
const MaxMemorySessions = 20000

// NewMemory returns an empty store. now is its clock; nil means time.Now.
func NewMemory(now func() time.Time) *Memory {
	if now == nil {
		now = time.Now
	}
	return &Memory{now: now, sessions: map[Key]entry{}}
}

// Create implements Store.
func (m *Memory) Create(_ context.Context, key Key, s Session, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if now.Sub(m.lastSweep) >= sweepInterval || len(m.sessions) >= MaxMemorySessions {
		for k, e := range m.sessions {
			if !now.Before(e.expires) {
				delete(m.sessions, k)
			}
		}
		m.lastSweep = now
	}
	if e, ok := m.sessions[key]; ok && now.Before(e.expires) {
		return errors.New("session key already in use")
	}
	if len(m.sessions) >= MaxMemorySessions {
		return ErrFull
	}
	m.sessions[key] = entry{session: s, expires: expires}
	return nil
}

// Get implements Store.
func (m *Memory) Get(_ context.Context, key Key) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live(key)
	if !ok {
		return Session{}, ErrNotFound
	}
	return e.session, nil
}

// Touch implements Store.
func (m *Memory) Touch(_ context.Context, key Key, lastSeen, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live(key)
	if !ok {
		return ErrNotFound
	}
	e.session.LastSeen, e.expires = lastSeen, expires
	m.sessions[key] = e
	return nil
}

// Delete implements Store.
func (m *Memory) Delete(_ context.Context, key Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, key)
	return nil
}

// live returns the entry under key if it has not expired. The caller holds mu.
func (m *Memory) live(key Key) (entry, bool) {
	e, ok := m.sessions[key]
	if !ok || !m.now().Before(e.expires) {
		return entry{}, false
	}
	return e, true
}

func (m *Memory) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}
