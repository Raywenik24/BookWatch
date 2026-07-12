// Package scheduler runs the LN volume check and the author/release tracker
// on their own independent cron schedules (plus on demand), with a
// single-flight lock so no two runs — scheduled or manual, either phase —
// ever overlap. A run that finds the lock held is simply skipped; it fires
// again on its own next tick (#80).
package scheduler

import (
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"bookwatch/internal/service"
)

// RunFunc performs one check run and returns its summary. progress is called
// before each book so the scheduler can publish live check progress.
type RunFunc func(progress func(i, total int, title string)) (service.CheckSummary, error)

// ValidateSpec reports whether spec is a valid cron expression for AddFunc.
// An empty spec (cron disabled) is always valid.
func ValidateSpec(spec string) error {
	if spec == "" {
		return nil
	}
	_, err := cron.ParseStandard(spec)
	return err
}

type Scheduler struct {
	run        RunFunc // combined LN+tracker run, used by the manual "Run check"
	runLN      RunFunc // LN volume check only
	runTracker RunFunc // author/release tracking only

	c *cron.Cron

	mu           sync.Mutex
	lnEntry      cron.EntryID
	trackerEntry cron.EntryID
	lnSpec       string
	trackerSpec  string
	running      bool
	lastRun      time.Time

	cur, total int // live progress of the in-flight run
	curTitle   string

	// observer, if set, is called (off-lock) after every state change — run
	// start, each progress tick, and run end — so an interested party can
	// publish live status without polling. Set once at wiring time via
	// OnChange; never read under the mutex.
	observer func()
}

// New builds a Scheduler. run is the combined LN+tracker run used by the
// manual "Run check" action; runLN and runTracker are the two phases that
// get their own independent cron schedules.
func New(run, runLN, runTracker RunFunc) *Scheduler {
	return &Scheduler{run: run, runLN: runLN, runTracker: runTracker, c: cron.New()}
}

// OnChange registers a callback fired after every run-state change (start,
// each progress tick, end). It runs off the scheduler lock, so the callback is
// free to call Busy/Progress. Meant to be set once during wiring.
func (s *Scheduler) OnChange(fn func()) { s.observer = fn }

// notify fires the observer if one is registered. Always called off-lock.
func (s *Scheduler) notify() {
	if s.observer != nil {
		s.observer()
	}
}

// Start registers the LN and tracker cron jobs (either spec may be empty to
// leave that phase cron-disabled) and starts the ticker.
func (s *Scheduler) Start(lnSpec, trackerSpec string) error {
	if err := s.RescheduleLN(lnSpec); err != nil {
		return err
	}
	if err := s.RescheduleTracker(trackerSpec); err != nil {
		return err
	}
	s.c.Start()
	return nil
}

func (s *Scheduler) Stop() { s.c.Stop() }

// RescheduleLN swaps the LN-check cron entry for a new spec (empty disables
// it). Safe to call at any time, including while the ticker is running — this
// is what lets a Settings edit take effect without a restart.
func (s *Scheduler) RescheduleLN(spec string) error {
	return s.reschedule(spec, &s.lnSpec, &s.lnEntry, func() { s.trigger("cron-ln", s.runLN) })
}

// RescheduleTracker is RescheduleLN for the tracker-poll cron entry.
func (s *Scheduler) RescheduleTracker(spec string) error {
	return s.reschedule(spec, &s.trackerSpec, &s.trackerEntry, func() { s.trigger("cron-tracker", s.runTracker) })
}

func (s *Scheduler) reschedule(spec string, curSpec *string, curEntry *cron.EntryID, fn func()) error {
	if err := ValidateSpec(spec); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if spec == *curSpec {
		return nil
	}
	if *curEntry != 0 {
		s.c.Remove(*curEntry)
		*curEntry = 0
	}
	if spec != "" {
		id, err := s.c.AddFunc(spec, fn)
		if err != nil {
			return err
		}
		*curEntry = id
	}
	*curSpec = spec
	return nil
}

// Busy reports whether a run is currently in progress.
func (s *Scheduler) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Progress returns the in-flight run's position (current, total, current title).
// All zero/empty when idle.
func (s *Scheduler) Progress() (cur, total int, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur, s.total, s.curTitle
}

// Trigger starts the combined LN+tracker run in the background — this is
// what the manual "Run check" action uses. Returns false if a run (scheduled
// or manual, either phase) is already in progress (single-flight).
func (s *Scheduler) Trigger(source string) bool {
	return s.trigger(source, s.run)
}

// trigger starts run in the background under the shared single-flight lock.
// source is just for logging. Returns false if a run is already in progress.
func (s *Scheduler) trigger(source string, run RunFunc) bool {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		log.Printf("check skipped (%s): a run is already in progress", source)
		return false
	}
	s.running = true
	s.cur, s.total, s.curTitle = 0, 0, ""
	s.mu.Unlock()
	s.notify() // run started

	progress := func(i, total int, title string) {
		s.mu.Lock()
		s.cur, s.total, s.curTitle = i, total, title
		s.mu.Unlock()
		s.notify()
	}

	go func() {
		defer func() {
			s.mu.Lock()
			s.running = false
			s.cur, s.total, s.curTitle = 0, 0, ""
			s.lastRun = time.Now()
			s.mu.Unlock()
			s.notify() // run finished
		}()
		log.Printf("check started (%s)", source)
		sum, err := run(progress)
		if err != nil {
			log.Printf("check error (%s): %v", source, err)
			return
		}
		log.Printf("check done (%s): %d checked, %d updated, %d errors",
			source, sum.Checked, sum.Updated, sum.Errors)
	}()
	return true
}
