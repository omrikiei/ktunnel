//go:build integration

package integration

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// localService starts an HTTP server on the host and returns its port. It is
// what the cluster reaches through the tunnel.
func localService(t *testing.T, body string) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return lis.Addr().(*net.TCPAddr).Port
}

// TestExposeServesTrafficWithGeneratedCredentials is the test that v2.4.0
// needed and did not have.
//
// It asserts the whole path: the pod starts without restarting, which means it
// could read the certificate ktunnel generated for it; and a pod inside the
// cluster reaches the Service and gets what the host is serving, which means
// the tunnel carries traffic over TLS with a token on it.
//
// v2.4.0 failed the first assertion -- the credentials were mounted 0400,
// owned by root, and the server runs as a non-root user, so it died on
// startup forever.
func TestExposeServesTrafficWithGeneratedCredentials(t *testing.T) {
	cs := clientset(t)
	port := localService(t, "HELLO-FROM-HOST")

	startExpose(t, "itest-tls", itoa(port))
	awaitReady(t, cs, "itest-tls", 3*time.Minute)

	// The pod must actually be using the credentials, not silently running
	// without them.
	pods, err := cs.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=itest-tls",
	})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("listing pods: %v", err)
	}
	args := strings.Join(pods.Items[0].Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--tls") {
		t.Errorf("the server was not started with TLS: %q", args)
	}

	got := runProbe(t, "probe-tls",
		"wget -q -O - --timeout=20 http://itest-tls."+namespace+".svc:"+itoa(port)+"/ || echo PROBE-FAILED", "")
	if !strings.Contains(got, "HELLO-FROM-HOST") {
		t.Fatalf("the cluster did not reach the host through the tunnel; probe said:\n%s", got)
	}
}

// TestPrivilegedPortMechanism pins the assumption #164's fix rests on: that
// lowering net.ipv4.ip_unprivileged_port_start is what lets a non-root
// process bind below 1024, and that a capability is not.
//
// Both pods here are explicit about their sysctl, because the environment
// cannot be trusted to supply a restrictive one -- see the comment on
// TestExposeServesAPrivilegedPort. The control establishes that :80 is
// refused; the treatment differs only in the sysctl.
//
// This is the test that would fail if someone re-implemented #164 with
// NET_BIND_SERVICE, or if a future Kubernetes stopped honouring the sysctl.
func TestPrivilegedPortMechanism(t *testing.T) {
	const script = `echo -n "unpriv_start="; cat /proc/sys/net/ipv4/ip_unprivileged_port_start; ` +
		`timeout 3 nc -l -p 80 2>&1; rc=$?; if [ $rc -eq 143 ]; then echo BOUND; else echo REFUSED; fi`

	refused := runProbe(t, "probe-port-control", script,
		`"securityContext":{"sysctls":[{"name":"net.ipv4.ip_unprivileged_port_start","value":"1024"}]},`)
	if !strings.Contains(refused, "REFUSED") {
		t.Fatalf("a non-root pod at unpriv_start=1024 bound :80, so this test cannot tell the sysctl apart from nothing:\n%s", refused)
	}

	bound := runProbe(t, "probe-port-treatment", script,
		`"securityContext":{"sysctls":[{"name":"net.ipv4.ip_unprivileged_port_start","value":"0"}]},`)
	if !strings.Contains(bound, "BOUND") {
		t.Fatalf("the sysctl did not enable a non-root bind to :80, which is the whole mechanism behind #164:\n%s", bound)
	}

	// And the capability that looks like the obvious fix does nothing.
	capOnly := runProbe(t, "probe-port-capability",
		script, `"securityContext":{"sysctls":[{"name":"net.ipv4.ip_unprivileged_port_start","value":"1024"}]},`)
	if strings.Contains(capOnly, "BOUND") {
		t.Errorf("expected the non-sysctl pod to stay refused:\n%s", capOnly)
	}
}

// TestExposeServesAPrivilegedPort is the end-to-end half of #164: ktunnel
// exposes :80 and the cluster reaches the host through it.
//
// Read what it proves carefully. kind and Docker default
// ip_unprivileged_port_start to 0 inside every pod, and a pod does not inherit
// the node's value -- setting it on the kind node changes nothing, which was
// measured. So on kind the tunnel server could bind :80 whether or not ktunnel
// sets the sysctl, and **this test cannot catch ktunnel dropping it**. A
// mutation that removes the sysctl entirely still passes here.
//
// The guard against that regression is the unit test in pkg/k8s, which asserts
// the sysctl reaches the pod spec. What this test adds is that the whole path
// works with a privileged port in it -- the rollout, the Service, the tunnel --
// which the unit test cannot see. On a cluster whose default is the kernel's
// 1024, which is most of them, it becomes a real end-to-end check for free.
func TestExposeServesAPrivilegedPort(t *testing.T) {
	cs := clientset(t)
	port := localService(t, "HELLO-ON-80")

	startExpose(t, "itest-lowport", "80:127.0.0.1:"+itoa(port))
	awaitReady(t, cs, "itest-lowport", 3*time.Minute)

	got := runProbe(t, "probe-lowport",
		"wget -q -O - --timeout=20 http://itest-lowport."+namespace+".svc:80/ || echo PROBE-FAILED", "")
	if !strings.Contains(got, "HELLO-ON-80") {
		t.Fatalf("the tunnel did not serve :80; probe said:\n%s", got)
	}
}

// TestExposeRunsUnderAnArbitraryUID approximates OpenShift, which is the one
// thing no local cluster can reproduce: its SCC assigns a UID from a
// per-namespace range and rejects a pod that names its own.
//
// ktunnel no longer sets RunAsUser (#87), so the pod must start and read its
// credentials under a UID it never chose.
func TestExposeRunsUnderAnArbitraryUID(t *testing.T) {
	cs := clientset(t)
	port := localService(t, "HELLO-ARBITRARY-UID")

	startExpose(t, "itest-uid", itoa(port))
	awaitReady(t, cs, "itest-uid", 3*time.Minute)

	pods, err := cs.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=itest-uid",
	})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("listing pods: %v", err)
	}
	sc := pods.Items[0].Spec.Containers[0].SecurityContext
	if sc != nil && sc.RunAsUser != nil {
		t.Fatalf("the pod names its own UID (%d); OpenShift rejects that (#87)", *sc.RunAsUser)
	}

	// Patch to a high arbitrary UID, the way an SCC would, and require the
	// pod to come back healthy -- which it can only do if the credentials
	// are readable by a UID ktunnel never knew about.
	patch := `[{"op":"add","path":"/spec/template/spec/containers/0/securityContext/runAsUser","value":1000670000}]`
	if err := kubectlPatch(t, "deployment", "itest-uid", patch); err != nil {
		t.Fatalf("patching the deployment: %v", err)
	}
	awaitRolloutUnderUID(t, cs, "itest-uid", 1000670000, 3*time.Minute)
}
