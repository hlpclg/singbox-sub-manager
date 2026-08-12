package health

import (
	"context"
	"fmt"
	"net"
)

// ---------------------------------------------------------------------------
// DNS resolve check (#14)
// ---------------------------------------------------------------------------

// dnsCheckImpl resolves cfg.Domain via the injected or default resolver and
// passes only when at least one address is returned.
//
// The injected LookupHost seam must honour ctx; the default net.Resolver does
// so on supported platforms. The DNS timeout from cfg.Timeouts.DNS is applied
// as a child context deadline.
type dnsCheckImpl struct{}

func dnsCheck() Check { return dnsCheckImpl{} }

func (dnsCheckImpl) ID() string   { return "dns.resolve" }
func (dnsCheckImpl) Name() string { return "DNS" }

func (c dnsCheckImpl) Run(ctx context.Context, cfg Config) Result {
	if ctx.Err() != nil {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "cancelled"}
	}

	domain := cfg.Domain
	if domain == "" {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "no domain configured"}
	}

	timeout := cfg.Timeouts.DNS
	if timeout <= 0 {
		timeout = DefaultTimeouts().DNS
	}

	dnsCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lookup := cfg.LookupHost
	if lookup == nil {
		r := &net.Resolver{}
		lookup = r.LookupHost
	}

	addrs, err := lookup(dnsCtx, domain)
	if err != nil {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: dnsErrMessage(err)}
	}
	if len(addrs) == 0 {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "no addresses"}
	}

	return Result{ID: c.ID(), Name: c.Name(), Status: StatusPass, Message: "resolves"}
}

// dnsErrMessage returns a stable, non-sensitive error description.
func dnsErrMessage(err error) string {
	if dnsErr, ok := err.(*net.DNSError); ok {
		if dnsErr.IsTimeout {
			return "timeout"
		}
		return fmt.Sprintf("lookup failed: %s", dnsErr.Name)
	}
	if err == context.DeadlineExceeded {
		return "timeout"
	}
	if err == context.Canceled {
		return "cancelled"
	}
	return "lookup failed"
}
