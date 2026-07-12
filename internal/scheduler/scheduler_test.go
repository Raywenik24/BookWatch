package scheduler

import (
	"sync/atomic"
	"testing"
	"time"

	"bookwatch/internal/service"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition not met in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestTrigger_singleFlightAndProgress(t *testing.T) {
	release := make(chan struct{})
	var runs int32
	run := func(progress func(i, total int, title string)) (service.CheckSummary, error) {
		atomic.AddInt32(&runs, 1)
		progress(1, 2, "x")
		<-release
		return service.CheckSummary{}, nil
	}
	s := New(run, nil, nil)

	if !s.Trigger("a") {
		t.Fatal("first trigger should start")
	}
	waitFor(t, func() bool { c, _, _ := s.Progress(); return s.Busy() && c == 1 })

	// A second trigger while running is rejected (single-flight).
	if s.Trigger("b") {
		t.Error("second trigger should be rejected while running")
	}
	if _, total, title := s.Progress(); total != 2 || title != "x" {
		t.Errorf("progress: total=%d title=%q", total, title)
	}

	close(release)
	waitFor(t, func() bool { return !s.Busy() })

	// Progress resets to zero when idle.
	if c, tot, ti := s.Progress(); c != 0 || tot != 0 || ti != "" {
		t.Errorf("progress not reset when idle: %d/%d %q", c, tot, ti)
	}
	if n := atomic.LoadInt32(&runs); n != 1 {
		t.Errorf("run executed %d times, want 1", n)
	}
}

func TestTrigger_acceptsAfterCompletion(t *testing.T) {
	var runs int32
	run := func(func(i, total int, title string)) (service.CheckSummary, error) {
		atomic.AddInt32(&runs, 1)
		return service.CheckSummary{}, nil
	}
	s := New(run, nil, nil)
	if !s.Trigger("1") {
		t.Fatal("first trigger should start")
	}
	waitFor(t, func() bool { return !s.Busy() })
	if !s.Trigger("2") {
		t.Fatal("a trigger after completion should be accepted")
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&runs) == 2 })
}

func TestReschedule_validatesAndSwapsWithoutRestart(t *testing.T) {
	noop := func(func(i, total int, title string)) (service.CheckSummary, error) {
		return service.CheckSummary{}, nil
	}
	s := New(noop, noop, noop)

	if err := s.Start("@every 1h", "@every 1h"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if err := s.RescheduleLN("not a cron expr"); err == nil {
		t.Error("bad LN spec should be rejected")
	}
	if err := s.RescheduleTracker("not a cron expr"); err == nil {
		t.Error("bad tracker spec should be rejected")
	}
	if err := s.RescheduleLN("@every 2h"); err != nil {
		t.Errorf("valid reschedule should succeed: %v", err)
	}
	if s.lnSpec != "@every 2h" {
		t.Errorf("lnSpec = %q, want @every 2h", s.lnSpec)
	}
	if err := s.RescheduleTracker(""); err != nil {
		t.Errorf("empty spec (disable) should succeed: %v", err)
	}
	if s.trackerSpec != "" || s.trackerEntry != 0 {
		t.Errorf("tracker cron not disabled: spec=%q entry=%v", s.trackerSpec, s.trackerEntry)
	}
}
