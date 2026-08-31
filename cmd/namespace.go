// Package cmd implements the command line interface for ktunnel
package cmd

import (
	"fmt"

	"github.com/omrikiei/ktunnel/pkg/k8s"
)

// namespaceFlagUsage states the precedence on the flag itself, since the flag
// no longer shows a default and "" would otherwise read as "no namespace".
const namespaceFlagUsage = "Namespace (default: the kubeconfig context's namespace, or \"default\")"

// resolveNamespace settles which namespace this run works in, once, and says
// which of the three possible sources decided it.
//
// --namespace used to default to the literal string "default", so the flag was
// always set and the kubeconfig context's own namespace was never read.
// Someone whose context points at their team namespace, running ktunnel the
// way they run kubectl, got their objects in `default` and nothing said so
// (#134). The flag now defaults to empty, which is what makes "not given"
// distinguishable from "given as default".
func resolveNamespace() string {
	namespace, source := resolveNamespaceQuietly()
	logger.Info(namespaceLine(namespace, source))
	return namespace
}

// resolveNamespaceQuietly is resolveNamespace for output that has to stay
// machine-readable. The logger writes to stdout, and --print-manifests writes
// YAML there, so where that one line goes is the caller's decision.
func resolveNamespaceQuietly() (namespace, source string) {
	namespace, source, err := k8s.ResolveNamespace(KubeContext, Namespace)
	if err != nil {
		// Not fatal: whatever is wrong with the kubeconfig surfaces on the
		// first API call, with a far better message than this could give.
		logger.Warnf("Could not read the namespace from the kubeconfig (%v); using namespace %s", err, namespace)
	}
	return namespace, source
}

func namespaceLine(namespace, source string) string {
	return fmt.Sprintf("Using namespace %s (%s)", namespace, source)
}
