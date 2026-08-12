package remote

import (
	"context"
	"fmt"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sync/atomic"
	"time"
)

var runSingbox = defaultRunSingbox
var testProxy = defaultTestProxy

var getFreePort = defaultGetFreePort

var nextPort int32 = 40000

func defaultGetFreePort() (int, error) {
	for i := 0; i < 1000; i++ {
		p := int(atomic.AddInt32(&nextPort, 1))
		if p > 50000 {
			atomic.StoreInt32(&nextPort, 40000)
			p = 40001
		}
		addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		l, err := net.ListenTCP("tcp", addr)
		if err == nil {
			l.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free ports")
}

func defaultRunSingbox(ctx context.Context, configPath string) (func(), error) {
	cmd := exec.CommandContext(ctx, "sing-box", "run", "-c", configPath)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start sing-box: %w", err)
	}
	cleanup := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}
	return cleanup, nil
}

func defaultTestProxy(ctx context.Context, port int) error {
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	// Retry loop for the port to open and proxy to be ready
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}

	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, "GET", "http://cp.cloudflare.com/generate_204", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == 204 {
				resp.Body.Close()
				return nil
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("proxy test failed: %w", lastErr)
}

func CheckNode(ctx context.Context, n nodes.Node) error {
	port, err := getFreePort()
	if err != nil {
		return fmt.Errorf("get free port: %w", err)
	}

	cfgData, err := GenerateConfig(n, port)
	if err != nil {
		return fmt.Errorf("generate config: %w", err)
	}

	tmpFile, err := os.CreateTemp("/tmp", "singbox-*.json")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(cfgData); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	tmpFile.Close()

	cleanup, err := runSingbox(ctx, tmpFile.Name())
	if err != nil {
		return err
	}
	defer cleanup()

	return testProxy(ctx, port)
}
