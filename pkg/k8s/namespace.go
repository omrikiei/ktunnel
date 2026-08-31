package k8s

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// defaultNamespace is where an object goes when neither the flag nor the
// kubeconfig context says otherwise -- the same fallback kubectl uses.
const defaultNamespace = "default"

// sourceDefault is what the report says when nothing selected a namespace.
const sourceDefault = "no namespace set in the kubeconfig context"

// ResolveNamespace decides which namespace a command works in, and says where
// that decision came from.
//
// The order is the one every other kubectl-shaped tool uses: an explicit
// --namespace wins, then the namespace of the kubeconfig context in play,
// then "default". Passing an empty flagNamespace means the flag was not given.
//
// This exists because --namespace used to default to the literal string
// "default", which made the flag look explicit on every run and hid the
// context's own namespace entirely. Someone whose context points at their team
// namespace, running ktunnel the way they run kubectl, got their deployment in
// `default` and nothing said so (#134).
//
// An unreadable kubeconfig is not fatal here: it returns the "default"
// fallback along with the error, so the caller can say so and carry on to the
// first API call, which reports what is actually wrong with far more detail
// than this can.
func ResolveNamespace(kubeCtx, flagNamespace string) (string, string, error) {
	if flagNamespace != "" {
		return flagNamespace, "--namespace", nil
	}

	raw, err := kubeClientConfig(kubeCtx).RawConfig()
	if err != nil {
		return defaultNamespace, sourceDefault, err
	}

	contextName := kubeCtx
	if contextName == "" {
		contextName = raw.CurrentContext
	}
	if kubeContext, ok := raw.Contexts[contextName]; ok && kubeContext.Namespace != "" {
		return kubeContext.Namespace, fmt.Sprintf("kubeconfig context %q", contextName), nil
	}
	return defaultNamespace, sourceDefault, nil
}

// kubeConfigPath returns the kubeconfig to load explicitly, which is
// ~/.kube/config when KUBECONFIG says nothing. An empty result leaves the
// default loading rules -- KUBECONFIG's own precedence list -- in charge.
func kubeConfigPath() string {
	if os.Getenv(kubeConfigEnvVar) != "" {
		return ""
	}
	if home := homedir.HomeDir(); home != "" {
		return filepath.Join(home, ".kube", "config")
	}
	return ""
}

// kubeClientConfig builds the client config both the namespace and the REST
// config are read from, so the two cannot disagree about which kubeconfig or
// which context is in play.
func kubeClientConfig(kubeCtx string) clientcmd.ClientConfig {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kConfig := kubeConfigPath(); kConfig != "" {
		loadingRules.ExplicitPath = kConfig
	}

	var configOverrides *clientcmd.ConfigOverrides
	if kubeCtx != "" {
		configOverrides = &clientcmd.ConfigOverrides{CurrentContext: kubeCtx}
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
}
