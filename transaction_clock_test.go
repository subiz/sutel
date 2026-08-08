package sutel

import (
	"sync"
	"testing"
	"time"
)

type fakeTransactionClock struct {
	mu        sync.Mutex
	now       time.Time
	timers    []*fakeTransactionTimer
	scheduled []time.Duration
	changed   chan struct{}
}

type fakeTransactionTimer struct {
	clock    *fakeTransactionClock
	deadline time.Time
	channel  chan time.Time
	stopped  bool
	fired    bool
}

func newFakeTransactionClock() *fakeTransactionClock {
	return &fakeTransactionClock{now: time.Unix(0, 0), changed: make(chan struct{})}
}

func (clock *fakeTransactionClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeTransactionClock) NewTimer(duration time.Duration) transactionTimer {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	timer := &fakeTransactionTimer{
		clock: clock, deadline: clock.now.Add(max(0, duration)), channel: make(chan time.Time, 1),
	}
	clock.timers = append(clock.timers, timer)
	clock.scheduled = append(clock.scheduled, duration)
	clock.signalLocked()
	return timer
}

func (timer *fakeTransactionTimer) C() <-chan time.Time { return timer.channel }

func (timer *fakeTransactionTimer) Stop() bool {
	timer.clock.mu.Lock()
	defer timer.clock.mu.Unlock()
	if timer.stopped || timer.fired {
		return false
	}
	timer.stopped = true
	timer.clock.signalLocked()
	return true
}

func (clock *fakeTransactionClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	now := clock.now
	for _, timer := range clock.timers {
		if timer.stopped || timer.fired || timer.deadline.After(now) {
			continue
		}
		timer.fired = true
		timer.channel <- now
	}
	clock.signalLocked()
	clock.mu.Unlock()
}

func (clock *fakeTransactionClock) waitForActiveTimer(t *testing.T, duration time.Duration) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		clock.mu.Lock()
		found := false
		for _, timer := range clock.timers {
			if !timer.stopped && !timer.fired && timer.deadline.Sub(clock.now) == duration {
				found = true
				break
			}
		}
		changed := clock.changed
		clock.mu.Unlock()
		if found {
			return
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("fake clock did not receive an active %s timer", duration)
		}
	}
}

func (clock *fakeTransactionClock) waitForNoActiveTimer(t *testing.T, duration time.Duration) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		clock.mu.Lock()
		found := false
		for _, timer := range clock.timers {
			if !timer.stopped && !timer.fired && timer.deadline.Sub(clock.now) == duration {
				found = true
				break
			}
		}
		changed := clock.changed
		clock.mu.Unlock()
		if !found {
			return
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("fake clock still has an active %s timer", duration)
		}
	}
}

func (clock *fakeTransactionClock) scheduledDurations() []time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return append([]time.Duration(nil), clock.scheduled...)
}

func (clock *fakeTransactionClock) signalLocked() {
	close(clock.changed)
	clock.changed = make(chan struct{})
}
