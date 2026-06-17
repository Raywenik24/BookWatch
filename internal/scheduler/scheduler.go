// Package scheduler runs the check on a cron schedule and on demand, with a
// single-flight lock so a scheduled run and a manual run never overlap.
package scheduler

import (
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"pagewatch/internal/service"
)

// RunFunc performs one check run and returns its summary.
type RunFunc func() (service.CheckSummary, error)

type Scheduler struct {
	run RunFunc
	c   *cron.Cron

	mu      sync.Mutex
	running bool
	lastRun time.Time
}

func New(run RunFunc) *Scheduler {
	return &Scheduler{run: run, c: cron.New()}
}

// Start schedules the cron job (if spec is non-empty) and starts the ticker.
func (s *Scheduler) Start(spec string) error {
	if spec != "" {
		if _, err := s.c.AddFunc(spec, func() { s.Trigger("cron") }); err != nil {
			return err
		}
	}
	s.c.Start()
	return nil
}

func (s *Scheduler) Stop() { s.c.Stop() }

// Busy reports whether a run is currently in progress.
func (s *Scheduler) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Trigger starts a run in the background. Returns false if one is already
// running (single-flight). source is just for logging.
func (s *Scheduler) Trigger(source string) bool {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false
	}
	s.running = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.running = false
			s.lastRun = time.Now()
			s.mu.Unlock()
		}()
		log.Printf("check started (%s)", source)
		sum, err := s.run()
		if err != nil {
			log.Printf("check error (%s): %v", source, err)
			return
		}
		log.Printf("check done (%s): %d checked, %d updated, %d errors",
			source, sum.Checked, sum.Updated, sum.Errors)
	}()
	return true
}
