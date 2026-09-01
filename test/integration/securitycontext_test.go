//go:build integration

// Package integration holds the tests that need a real kubelet.
//
// Everything here exists because the fake client cannot see it. A fake client
// answers questions about the shape of a manifest: whether a volume is
// declared, whether a field is set. It cannot answer whether the process on
// the other end can open the file, or whether a bind succeeds -- and both of
// those shipped as bugs in v2.4.0, past a full suite of green shape
// assertions.
//
// Run with: make test-integration
package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	namespace = "default"
	// ktunnelBin and image are what the CI job builds and loads into the
	// cluster before this runs.
	ktunnelBin = "KTUNNEL_BIN"
	imageEnv   = "KTUNNEL_IMAGE"
	// contextEnv names the kube context to run against, and is required.
	//
	// Not optional, and deliberately not defaulted to the current context:
	// these tests create Deployments, Services and Secrets and then delete
	// them. Inheriting whichever cluster the developer happens to be
	// pointed at is how a test suite ends up running against production.
	contextEnv = "KTUNNEL_TEST_CONTEXT"
)

// testContext returns the kube context these tests may touch, and fails if
// nobody named one.
func testContext(t *testing.T) string {
	t.Helper()
	ctx := os.Getenv(contextEnv)
	if ctx == "" {
		t.Fatalf("%s is not set. These tests create and delete cluster resources, so they refuse to "+
			"guess at a cluster: set it to the kind context, e.g. %s=kind-ktunnel-itest", contextEnv, contextEnv)
	}
	return ctx
}

func clientset(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: testContext(t)}).ClientConfig()
	if err != nil {
		t.Fatalf("no cluster reachable: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("building clientset: %v", err)
	}
	return cs
}

func binary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv(ktunnelBin)
	if bin == "" {
		t.Fatalf("%s is not set; the CI job builds the binary and points this at it", ktunnelBin)
	}
	return bin
}

func image(t *testing.T) string {
	t.Helper()
	img := os.Getenv(imageEnv)
	if img == "" {
		t.Fatalf("%s is not set; the CI job builds the image and loads it into the cluster", imageEnv)
	}
	return img
}

// startExpose runs `ktunnel expose` in the background and stops it on cleanup,
// which is also how the teardown path gets exercised.
func startExpose(t *testing.T, name string, args ...string) {
	t.Helper()
	full := append([]string{"expose", name}, args...)
	full = append(full, "-n", namespace, "--server-image", image(t), "--context", testContext(t))
	cmd := exec.Command(binary(t), full...)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting ktunnel: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
		}
		if t.Failed() {
			t.Logf("ktunnel output:\n%s", out.String())
		}
	})
}

// awaitReady waits for the deployment's pod to be ready, and fails loudly on a
// restart. The restart count is the assertion that matters: v2.4.0's pod could
// not read its own certificate, so it came up, died, and came up again
// forever, and nothing in the unit suite noticed.
func awaitReady(t *testing.T, cs *kubernetes.Clientset, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pods, err := cs.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/instance=" + name,
		})
		if err == nil && len(pods.Items) > 0 {
			for _, p := range pods.Items {
				for _, cs := range p.Status.ContainerStatuses {
					if cs.RestartCount > 0 {
						logs := podLogs(t, p.Name)
						t.Fatalf("pod %s restarted %d times before becoming ready; it cannot run as configured.\nlogs:\n%s",
							p.Name, cs.RestartCount, logs)
					}
					if cs.Ready {
						return
					}
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("deployment %s never became ready within %s", name, timeout)
}

func podLogs(t *testing.T, pod string) string {
	t.Helper()
	out, _ := exec.Command("kubectl", "--context", testContext(t), "logs", pod, "-n", namespace, "--tail=20").CombinedOutput()
	return string(out)
}

// runProbe runs a throwaway pod to completion and returns what it printed.
// It is how a test asks a question from inside the cluster.
//
// Create, wait, read logs, delete -- rather than `kubectl run --attach`,
// which interleaves its own warnings with the container's output and drops it
// entirely when the pod finishes quickly.
func runProbe(t *testing.T, name, script string, podSpecExtra string) string {
	t.Helper()
	ctx := testContext(t)
	_ = exec.Command("kubectl", "--context", ctx, "delete", "pod", name, "-n", namespace, "--ignore-not-found").Run()

	// The probe container mirrors the tunnel server's own security context:
	// non-root, every capability dropped. That matters for the privileged
	// port control probe above all -- root binds :80 whatever
	// ip_unprivileged_port_start says, so a root probe cannot establish that
	// the port is refused, and the test it guards would pass for the wrong
	// reason.
	manifest := fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":%q},`+
		`"spec":{"restartPolicy":"Never",%s"containers":[{"name":"p","image":"busybox:1.36",`+
		`"securityContext":{"runAsUser":1000,"allowPrivilegeEscalation":false,`+
		`"capabilities":{"drop":["ALL"]}},`+
		`"command":["sh","-c"],"args":[%q]}]}}`, name, podSpecExtra, script)

	apply := exec.Command("kubectl", "--context", ctx, "apply", "-n", namespace, "-f", "-")
	apply.Stdin = strings.NewReader(manifest)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("creating probe %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", ctx, "delete", "pod", name, "-n", namespace, "--ignore-not-found").Run()
	})

	// Succeeded or Failed: either way the container has said what it has to
	// say, and its logs are the answer.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		phase, _ := exec.Command("kubectl", "--context", ctx, "get", "pod", name,
			"-n", namespace, "-o", "jsonpath={.status.phase}").Output()
		switch string(phase) {
		case "Succeeded", "Failed":
			out, _ := exec.Command("kubectl", "--context", ctx, "logs", name, "-n", namespace).CombinedOutput()
			return string(out)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("probe %s never finished", name)
	return ""
}

func itoa(i int) string { return strconv.Itoa(i) }

// kubectlPatch applies a JSON patch, which is how a test simulates an
// admission controller mutating the object after ktunnel wrote it.
func kubectlPatch(t *testing.T, kind, name, patch string) error {
	t.Helper()
	out, err := exec.Command("kubectl", "--context", testContext(t), "patch", kind, name,
		"-n", namespace, "--type=json", "-p="+patch).CombinedOutput()
	if err != nil {
		t.Logf("kubectl patch: %s", out)
	}
	return err
}

// awaitRolloutUnderUID waits for a pod running as the given UID to be ready.
func awaitRolloutUnderUID(t *testing.T, cs *kubernetes.Clientset, name string, uid int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pods, err := cs.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/instance=" + name,
		})
		if err == nil {
			for _, p := range pods.Items {
				sc := p.Spec.Containers[0].SecurityContext
				if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != uid {
					continue
				}
				for _, st := range p.Status.ContainerStatuses {
					if st.RestartCount > 0 {
						t.Fatalf("pod %s running as UID %d restarted %d times: it cannot read its credentials under a UID it did not choose.\nlogs:\n%s",
							p.Name, uid, st.RestartCount, podLogs(t, p.Name))
					}
					if st.Ready {
						return
					}
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no pod running as UID %d became ready within %s", uid, timeout)
}
