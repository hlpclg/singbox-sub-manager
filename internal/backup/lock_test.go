package backup

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLocker records how a lock was used so tests can prove that every exit
// path releases it exactly as often as it was taken.
type fakeLocker struct {
	tryCalls    int
	unlockCalls int
	tryErr      error
	unlockErr   error
	onTry       func(attempt int)
}

func (f *fakeLocker) TryLock() error {
	f.tryCalls++
	if f.onTry != nil {
		f.onTry(f.tryCalls)
	}
	return f.tryErr
}

func (f *fakeLocker) Unlock() error {
	f.unlockCalls++
	return f.unlockErr
}

func TestAcquireWithRetry_Success(t *testing.T) {
	l := &fakeLocker{}
	lease, err := AcquireWithRetry(context.Background(), l, time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("AcquireWithRetry: %v", err)
	}
	if l.tryCalls != 1 {
		t.Errorf("TryLock called %d times, want 1", l.tryCalls)
	}
	if err := lease.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := lease.Unlock(); err != nil {
		t.Fatalf("second Unlock: %v", err)
	}
	if l.unlockCalls != 1 {
		t.Errorf("Unlock reached the lock %d times, want exactly 1", l.unlockCalls)
	}
}

func TestAcquireWithRetry_RetriesUntilFree(t *testing.T) {
	l := &fakeLocker{tryErr: errors.New("busy")}
	l.onTry = func(attempt int) {
		if attempt == 3 {
			l.tryErr = nil
		}
	}
	if _, err := AcquireWithRetry(context.Background(), l, time.Millisecond, time.Second); err != nil {
		t.Fatalf("AcquireWithRetry: %v", err)
	}
	if l.tryCalls != 3 {
		t.Errorf("TryLock called %d times, want 3", l.tryCalls)
	}
}

func TestAcquireWithRetry_Busy(t *testing.T) {
	l := &fakeLocker{tryErr: errors.New("held by monitor")}
	start := time.Now()
	lease, err := AcquireWithRetry(context.Background(), l, time.Millisecond, 20*time.Millisecond)
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("err = %v, want ErrLockBusy", err)
	}
	if lease != nil {
		t.Error("a lease was returned for a busy lock")
	}
	if l.tryCalls < 2 {
		t.Errorf("TryLock called %d times, want the bounded wait to retry", l.tryCalls)
	}
	if l.unlockCalls != 0 {
		t.Errorf("Unlock called %d times after failing to acquire", l.unlockCalls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("bounded wait took %s, want it to give up quickly", elapsed)
	}
}

func TestAcquireWithRetry_ZeroMaxWaitTriesOnce(t *testing.T) {
	l := &fakeLocker{tryErr: errors.New("busy")}
	if _, err := AcquireWithRetry(context.Background(), l, time.Millisecond, 0); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("err = %v, want ErrLockBusy", err)
	}
	if l.tryCalls != 1 {
		t.Errorf("TryLock called %d times, want exactly 1", l.tryCalls)
	}
}

func TestAcquireWithRetry_CancelWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := &fakeLocker{tryErr: errors.New("busy")}
	l.onTry = func(attempt int) {
		if attempt == 2 {
			cancel()
		}
	}

	_, err := AcquireWithRetry(ctx, l, time.Millisecond, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if l.tryCalls != 2 {
		t.Errorf("TryLock called %d times, want the wait to stop at cancellation", l.tryCalls)
	}
	if l.unlockCalls != 0 {
		t.Errorf("Unlock called %d times without holding the lock", l.unlockCalls)
	}
}

func TestAcquireWithRetry_AlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l := &fakeLocker{}
	if _, err := AcquireWithRetry(ctx, l, time.Millisecond, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if l.tryCalls != 0 {
		t.Errorf("TryLock called %d times on an already cancelled context", l.tryCalls)
	}
}

func TestAcquireWithRetry_RequiresLocker(t *testing.T) {
	if _, err := AcquireWithRetry(context.Background(), nil, time.Millisecond, time.Millisecond); err == nil {
		t.Fatal("AcquireWithRetry accepted a nil locker")
	}
}
