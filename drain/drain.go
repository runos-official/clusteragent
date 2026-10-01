// Package drain counts the work that a cluster agent restart would destroy
// (builds and one-shot jobs) and lets the process shut down only after that
// work ends, or after a timeout.
//
// Every code path that starts such work calls Begin first and calls the
// returned end function when the work is over. On SIGTERM main calls Drain:
// from then on Begin refuses new work, and Drain waits for the running work.
package drain

import (
	"errors"
	"sync"
	"time"
)

// ErrDraining is what Begin returns once Drain has started. The caller reports
// it to its own caller as a refusal to start new work.
var ErrDraining = errors.New("cluster agent is draining for a restart and takes no new builds or jobs")

// ErrStarting is what Begin returns while the tracker is held. The agent holds
// work back at startup until it has closed the build rows of the previous
// process, so that cleanup cannot fail a row that a new build just created.
var ErrStarting = errors.New("cluster agent is starting and takes no new builds or jobs yet")

// Tracker counts in-flight work. The zero value is not usable: call New.
type Tracker struct {
	mu       sync.Mutex
	inFlight int
	draining bool
	held     bool
	idle     chan struct{} // closed when inFlight reaches zero during a drain
}

// New returns an empty Tracker.
func New() *Tracker { return &Tracker{} }

// Begin registers one unit of work. It returns ErrDraining after Drain has
// started. The returned end function is safe to call more than once; only the
// first call counts.
func (t *Tracker) Begin() (end func(), err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return nil, ErrDraining
	}
	if t.held {
		return nil, ErrStarting
	}
	t.inFlight++
	var once sync.Once
	return func() { once.Do(t.finish) }, nil
}

func (t *Tracker) finish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inFlight--
	if t.inFlight == 0 && t.idle != nil {
		close(t.idle)
		t.idle = nil
	}
}

// Drain stops new work and waits until the running work ends or the timeout
// passes. It returns how many units of work still ran at that point: zero means
// a clean drain.
func (t *Tracker) Drain(timeout time.Duration) int {
	t.mu.Lock()
	t.draining = true
	if t.inFlight == 0 {
		t.mu.Unlock()
		return 0
	}
	if t.idle == nil {
		t.idle = make(chan struct{})
	}
	idle := t.idle
	t.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return 0
	case <-timer.C:
		return t.InFlight()
	}
}

// Hold makes Begin refuse work with ErrStarting until Release is called.
func (t *Tracker) Hold() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.held = true
}

// Release lets Begin accept work again. It never undoes a drain: a Drain that
// started while the tracker was held keeps refusing work.
func (t *Tracker) Release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.held = false
}

// Draining reports whether Drain has started.
func (t *Tracker) Draining() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.draining
}

// InFlight reports how many units of work are running.
func (t *Tracker) InFlight() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inFlight
}

// std is the process-wide tracker that the build and one-shot paths share.
var std = New()

// Begin registers work on the process-wide tracker. See Tracker.Begin.
func Begin() (end func(), err error) { return std.Begin() }

// Hold holds the process-wide tracker. See Tracker.Hold.
func Hold() { std.Hold() }

// Release releases the process-wide tracker. See Tracker.Release.
func Release() { std.Release() }

// Drain drains the process-wide tracker. See Tracker.Drain.
func Drain(timeout time.Duration) int { return std.Drain(timeout) }

// Draining reports whether the process-wide tracker is draining.
func Draining() bool { return std.Draining() }

// InFlight reports the work on the process-wide tracker.
func InFlight() int { return std.InFlight() }
