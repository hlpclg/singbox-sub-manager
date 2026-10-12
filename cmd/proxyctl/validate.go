package main

import "github.com/hlpclg/singbox-sub-manager/internal/subscriptionvalidate"

// validateClashYAML is the CLI's long-standing name for the YAML structural
// layer of design §5. The implementation lives in internal/subscriptionvalidate
// so `proxyctl validate` and the v0.10 publish transaction share one copy.
func validateClashYAML(doc string) error {
	return subscriptionvalidate.ClashYAML(doc)
}
