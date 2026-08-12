package remote

import (
	"context"
	"fmt"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"strings"
	"testing"
)

func TestCheckNode_Success(t *testing.T) {
	node := nodes.Node{Name: "test"}

	oldRun := runSingbox
	oldTest := testProxy
	oldGetPort := getFreePort
	defer func() {
		runSingbox = oldRun
		testProxy = oldTest
		getFreePort = oldGetPort
	}()

	runSingbox = func(ctx context.Context, configPath string) (func(), error) {
		return func() {}, nil
	}
	testProxy = func(ctx context.Context, port int) error {
		return nil
	}
	getFreePort = func() (int, error) {
		return 10800, nil
	}

	err := CheckNode(context.Background(), node)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckNode_ProxyFail(t *testing.T) {
	node := nodes.Node{Name: "test"}

	oldRun := runSingbox
	oldTest := testProxy
	oldGetPort := getFreePort
	defer func() {
		runSingbox = oldRun
		testProxy = oldTest
		getFreePort = oldGetPort
	}()

	runSingbox = func(ctx context.Context, configPath string) (func(), error) {
		return func() {}, nil
	}
	testProxy = func(ctx context.Context, port int) error {
		return fmt.Errorf("proxy test failed: %w", context.DeadlineExceeded)
	}
	getFreePort = func() (int, error) {
		return 10800, nil
	}

	err := CheckNode(context.Background(), node)
	if err == nil || !strings.Contains(err.Error(), "proxy test failed") {
		t.Fatalf("expected proxy test failed error, got %v", err)
	}
}

func TestDefaultTestProxy_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := defaultTestProxy(ctx, 12345)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("expected context canceled error, got %v", err)
	}
}

func TestDefaultRunSingbox_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Should fail immediately to start or context canceled
	_, err := defaultRunSingbox(ctx, "/tmp/nonexistent.json")
	if err == nil {
		t.Errorf("expected error, got nil")
	}
}
