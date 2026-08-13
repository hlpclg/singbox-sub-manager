package monitor

import (
	"path/filepath"
	"testing"
)

func TestProcessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	lock1 := NewFileLock(path)
	if err := lock1.TryLock(); err != nil {
		t.Fatalf("first lock failed: %v", err)
	}
	
	lock2 := NewFileLock(path)
	if err := lock2.TryLock(); err == nil {
		t.Fatal("expected second lock to fail")
	}
	
	lock1.Unlock()
	if err := lock2.TryLock(); err != nil {
		t.Fatalf("second lock failed after unlock: %v", err)
	}
	lock2.Unlock()
}
