package k8s

import (
	"os"
	"path/filepath"
	"testing"
)

// kubeconfigWithNamespaces writes a kubeconfig holding two contexts -- one
// that sets a namespace and one that does not -- and points KUBECONFIG at it.
func kubeconfigWithNamespaces(t *testing.T, currentContext string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	body := `apiVersion: v1
kind: Config
current-context: ` + currentContext + `
clusters:
- name: c
  cluster:
    server: https://example.invalid:6443
users:
- name: u
  user:
    token: t
contexts:
- name: with-ns
  context:
    cluster: c
    user: u
    namespace: team-a
- name: no-ns
  context:
    cluster: c
    user: u
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("failed writing kubeconfig: %v", err)
	}
	t.Setenv(kubeConfigEnvVar, path)
}

// TestResolveNamespace is the regression test for ktunnel ignoring the
// namespace its kubeconfig context selects.
//
// --namespace defaulted to the literal string "default", so the flag was
// always set and always won. Someone whose context points at team-a, who runs
// `ktunnel expose` the way they run every other kubectl command, got their
// objects in `default` -- silently, since nothing said which namespace was
// being used (#134).
func TestResolveNamespace(t *testing.T) {
	tests := []struct {
		name           string
		currentContext string
		kubeCtx        string
		flag           string
		wantNamespace  string
		wantSource     string
	}{
		{
			name:           "the flag wins when it is set",
			currentContext: "with-ns",
			flag:           "prod",
			wantNamespace:  "prod",
			wantSource:     "--namespace",
		},
		{
			name:           "the current context's namespace is used when the flag is not",
			currentContext: "with-ns",
			wantNamespace:  "team-a",
			wantSource:     `kubeconfig context "with-ns"`,
		},
		{
			name:           "--context selects which context's namespace is read",
			currentContext: "no-ns",
			kubeCtx:        "with-ns",
			wantNamespace:  "team-a",
			wantSource:     `kubeconfig context "with-ns"`,
		},
		{
			name:           "a context with no namespace falls back to default",
			currentContext: "no-ns",
			wantNamespace:  "default",
			wantSource:     sourceDefault,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kubeconfigWithNamespaces(t, tc.currentContext)

			namespace, source, err := ResolveNamespace(tc.kubeCtx, tc.flag)
			if err != nil {
				t.Fatalf("ResolveNamespace: %v", err)
			}
			if namespace != tc.wantNamespace {
				t.Errorf("namespace is %q, want %q; objects would be created in the wrong namespace", namespace, tc.wantNamespace)
			}
			if source != tc.wantSource {
				t.Errorf("source is %q, want %q; the point of reporting it is that the user can tell which one it came from", source, tc.wantSource)
			}
		})
	}
}

// TestResolveNamespace_UnreadableKubeconfig pins the fallback: a kubeconfig
// that cannot be read is not a reason to fail here. Whatever is wrong with it
// surfaces on the first API call with a much better message than this function
// could give.
func TestResolveNamespace_UnreadableKubeconfig(t *testing.T) {
	t.Setenv(kubeConfigEnvVar, filepath.Join(t.TempDir(), "does-not-exist"))

	namespace, _, err := ResolveNamespace("", "")
	if err == nil {
		t.Skip("reading a missing kubeconfig is not an error on this platform")
	}
	if namespace != "default" {
		t.Errorf("namespace is %q on an unreadable kubeconfig, want the %q fallback", namespace, "default")
	}
}
