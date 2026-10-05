package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The memory store keeps the Store contract on its own, whatever the Manager does.
func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	m := NewMemory(c.Now)
	key, other := Key{1}, Key{2}
	until := c.Now().Add(time.Hour)

	if _, err := m.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of nothing: %v", err)
	}
	// Touch never creates: a deleted session stays deleted.
	if err := m.Touch(ctx, key, c.Now(), until); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Touch of nothing: %v", err)
	}
	if _, err := m.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatal("Touch created a session")
	}

	if err := m.Create(ctx, key, Session{IDToken: "a"}, until); err != nil {
		t.Fatal(err)
	}
	if err := m.Create(ctx, key, Session{IDToken: "b"}, until); err == nil {
		t.Fatal("Create overwrote an existing session")
	}
	if s, err := m.Get(ctx, key); err != nil || s.IDToken != "a" {
		t.Fatalf("Get = %+v, %v", s, err)
	}

	// Touch updates activity and the deadline, nothing else.
	later := c.Now().Add(time.Minute)
	if err := m.Touch(ctx, key, later, until.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s, _ := m.Get(ctx, key); !s.LastSeen.Equal(later) || s.IDToken != "a" {
		t.Fatalf("after Touch: %+v", s)
	}

	// Past its deadline a session is not returned, and not touched back to life.
	if err := m.Create(ctx, other, Session{IDToken: "o"}, c.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if _, err := m.Get(ctx, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get at the deadline: %v", err)
	}
	if err := m.Touch(ctx, other, c.Now(), c.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Touch revived an expired session: %v", err)
	}

	if err := m.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatal("Delete left the session")
	}
	if err := m.Delete(ctx, key); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

// Sessions nobody comes back for do not pile up: creating one sweeps out the
// expired ones.
func TestMemoryStoreForgetsExpiredSessions(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	m := NewMemory(c.Now)
	for i := range 100 {
		if err := m.Create(ctx, Key{byte(i)}, Session{}, c.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	c.Advance(2 * sweepInterval)
	if err := m.Create(ctx, Key{200}, Session{}, c.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := m.len(); n != 1 {
		t.Fatalf("%d sessions held, want 1", n)
	}
}

// The store holds at most MaxMemorySessions live sessions; expired ones make room.
func TestMemoryStoreIsBounded(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	m := NewMemory(c.Now)
	until := c.Now().Add(time.Hour)
	for i := range MaxMemorySessions {
		if err := m.Create(ctx, Key{byte(i), byte(i >> 8), byte(i >> 16)}, Session{}, until); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if err := m.Create(ctx, Key{0xff, 0xff, 0xff}, Session{}, until); !errors.Is(err, ErrFull) {
		t.Fatalf("Create past the bound: %v", err)
	}
	c.Advance(time.Hour)
	if err := m.Create(ctx, Key{0xff, 0xff, 0xff}, Session{}, c.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Create after the others expired: %v", err)
	}
	if n := m.len(); n != 1 {
		t.Errorf("%d sessions held, want 1", n)
	}
}
