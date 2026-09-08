package mobileauth

import (
	"testing"
	"time"
)

func TestResumePreservesAbsoluteExpiryAndRejectsDeadline(t *testing.T) {
	t.Parallel()
	clock := newMutableClock(time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
	manager := mustSessionManager(t, clock, nil)
	token, _, created, err := manager.Exchange(testIdentity("resume-expiry"), "client-1")
	if err != nil {
		t.Fatal(err)
	}
	for step := 1; step < 24; step++ {
		clock.Advance(20 * time.Minute)
		_, resumed, err := manager.Resume(token, "client-1")
		if err != nil || !resumed.ExpiresAt.Equal(created.ExpiresAt) {
			t.Fatalf("resume extended expiry or failed: step=%d session=%+v err=%v", step, resumed, err)
		}
	}
	clock.Advance(20 * time.Minute)
	if _, _, err := manager.Resume(token, "client-1"); SessionReasonOf(err) != SessionReasonExpired {
		t.Fatalf("absolute deadline reason=%q err=%v", SessionReasonOf(err), err)
	}
}

func TestResumeRejectsIdleDeadline(t *testing.T) {
	t.Parallel()
	clock := newMutableClock(time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
	manager := mustSessionManager(t, clock, nil)
	token, _, _, err := manager.Exchange(testIdentity("resume-idle"), "client-1")
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Minute)
	if _, _, err := manager.Resume(token, "client-1"); SessionReasonOf(err) != SessionReasonExpired {
		t.Fatalf("idle deadline reason=%q err=%v", SessionReasonOf(err), err)
	}
}
