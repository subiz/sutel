package sutel

import "time"

type transactionTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type transactionClock interface {
	Now() time.Time
	NewTimer(time.Duration) transactionTimer
}

type realTransactionClock struct{}

func (realTransactionClock) Now() time.Time { return time.Now() }

func (realTransactionClock) NewTimer(duration time.Duration) transactionTimer {
	return realTransactionTimer{Timer: time.NewTimer(duration)}
}

type realTransactionTimer struct {
	*time.Timer
}

func (timer realTransactionTimer) C() <-chan time.Time { return timer.Timer.C }

func timerChannel(timer transactionTimer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.C()
}

func stopTransactionTimer(timer transactionTimer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C():
	default:
	}
}
