package remote

import (
	"context"
	"strings"
	"fmt"
	"testing"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
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
