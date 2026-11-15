package sessionauth

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGrantScopeExpiryAndRevocation(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	grant, token, err := s.Issue("device", "service", "instance", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	_, done, err := s.Validate(grant.ID, token, "service", "instance")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][3]string{{"bad", "service", "instance"}, {token, "other", "instance"}, {token, "service", "other"}} {
		if _, _, err := s.Validate(grant.ID, scope[0], scope[1], scope[2]); !errors.Is(err, ErrUnauthorized) {
			t.Fatal("scope bypass")
		}
	}
	if _, _, err := s.Issue("device", "service", "instance", time.Minute, false); !errors.Is(err, ErrConflict) {
		t.Fatal("conflict accepted")
	}
	next, secret, err := s.Issue("device", "service", "instance", time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("preemption did not revoke previous grant")
	}
	if _, _, err := s.Validate(grant.ID, token, "service", "instance"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old token accepted")
	}
	now = now.Add(time.Minute)
	if _, _, err := s.Validate(next.ID, secret, "service", "instance"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired token accepted")
	}
}

func TestGrantConcurrentReservation(t *testing.T) {
	s := NewStore()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			grant, token, err := s.Issue("device", "service", "instance", time.Minute, false)
			if err == nil {
				accepted.Add(1)
				if _, _, err := s.Validate(grant.ID, token, "service", "instance"); err != nil {
					t.Error(err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d", accepted.Load())
	}
	s.RevokeDevice("device")
	grant, token, err := s.Issue("device", "service", "instance", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	_, done, _ := s.Validate(grant.ID, token, "service", "instance")
	s.RevokeAll()
	select {
	case <-done:
	default:
		t.Fatal("revoke all failed")
	}
	if s.Revoke(grant.ID) {
		t.Fatal("repeated revoke not idempotent")
	}
}
