package health

// AllChecks returns the 15 checks in the exact display order defined by the
// design spec. The slice is freshly allocated on each call.
func AllChecks() []Check {
	return []Check{
		singboxServiceCheck(),
		caddyServiceCheck(),
		udp443Check(),
		tcp443Check(),
		tcp80Check(),
		singboxConfigCheck(),
		caddyConfigCheck(),
		tokenCheck(),
		clashCheck(),
		srCheck(),
		httpsSubscriptionCheck(),
		tlsCertificateCheck(),
		tlsExpiryCheck(),
		dnsCheck(),
		diskCheck(),
	}
}

// ConcurrentIDs returns the check IDs that may run concurrently.
// Per the design: DNS, HTTPS, TLS certificate, and TLS expiry.
func ConcurrentIDs() map[string]bool {
	return map[string]bool{
		"dns.resolve":       true,
		"http.subscription": true,
		"tls.certificate":   true,
		"tls.expiry":        true,
	}
}
