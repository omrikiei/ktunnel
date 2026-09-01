package k8s

import (
	"testing"

	apiv1 "k8s.io/api/core/v1"
)

// #87: OpenShift assigns a UID from a per-namespace range and rejects a pod
// that demands its own. ktunnel hardcoded RunAsUser: 1000, so `expose` did
// not work on OCP at all -- the reporter's deployment ran the moment they
// deleted the field by hand.
//
// The non-root property it was protecting moves to the image, which carries
// USER 1000. On a vanilla cluster that runs as 1000 exactly as before; on
// OpenShift the SCC overrides it, which is what OpenShift wants to do.
func Test_newContainer_DoesNotDemandItsOwnUID(t *testing.T) {
	c := newContainer(28688, "img", []apiv1.ContainerPort{}, PodCredentials{}, 250, 1000, 128, 512)

	if c.SecurityContext == nil {
		t.Fatal("no security context at all")
	}
	if c.SecurityContext.RunAsUser != nil {
		t.Errorf("RunAsUser is set to %d; OpenShift rejects a pod that picks its own UID (#87)",
			*c.SecurityContext.RunAsUser)
	}
	if c.SecurityContext.RunAsGroup != nil {
		t.Errorf("RunAsGroup is set to %d, which OpenShift rejects for the same reason",
			*c.SecurityContext.RunAsGroup)
	}
}

// Both are required by OpenShift's restricted-v2 SCC, and cost nothing on a
// vanilla cluster. Dropping every capability is also what makes an explicit
// grant meaningful, rather than one addition on top of a default set.
func Test_newContainer_DropsEveryCapability(t *testing.T) {
	c := newContainer(28688, "img", []apiv1.ContainerPort{}, PodCredentials{}, 250, 1000, 128, 512)

	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation is not false; restricted-v2 requires it and the tunnel server execs nothing")
	}
	if c.SecurityContext.Capabilities == nil {
		t.Fatal("no capabilities block")
	}
	var dropsAll bool
	for _, d := range c.SecurityContext.Capabilities.Drop {
		if d == "ALL" {
			dropsAll = true
		}
	}
	if !dropsAll {
		t.Errorf("capabilities are not dropped: %+v", c.SecurityContext.Capabilities)
	}

	// Measured, not assumed: capabilities.add is inert for a non-root
	// container -- the capability never reaches the permitted set, and a
	// bind to :80 still fails. Adding one would be a change that reads as a
	// fix and does nothing.
	if len(c.SecurityContext.Capabilities.Add) != 0 {
		t.Errorf("capabilities added to a non-root container, where they have no effect: %+v",
			c.SecurityContext.Capabilities.Add)
	}
}

// #164: the tunnel server binds the source ports inside the pod, so
// `expose myapp 80:8000` needs :80 there.
//
// The mechanism is a pod-level sysctl, not a capability. Kubernetes treats
// net.ipv4.ip_unprivileged_port_start as safe (1.22+), so it needs no cluster
// configuration, and setting it explicitly makes ktunnel behave the same on
// every cluster rather than inheriting whatever the container runtime chose --
// kind/Docker default it to 0, the kernel defaults it to 1024, which is why
// this reproduces for some people and not others.
func Test_podSysctls_OnlyWhenAPortNeedsIt(t *testing.T) {
	cases := []struct {
		name  string
		ports []int
		want  bool
	}{
		{"ordinary high ports", []int{8080, 28688}, false},
		{"http", []int{80}, true},
		{"https among high ports", []int{8080, 443, 9000}, true},
		{"the boundary is not privileged", []int{1024}, false},
		{"just below the boundary", []int{1023}, true},
		{"no ports at all", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := podSysctls(tc.ports)
			if tc.want && len(got) == 0 {
				t.Fatalf("ports %v include a privileged one, but no sysctl was set; the bind fails on any cluster whose default is 1024", tc.ports)
			}
			if !tc.want && len(got) != 0 {
				t.Fatalf("ports %v need no sysctl, but got %+v", tc.ports, got)
			}
			if tc.want {
				if got[0].Name != "net.ipv4.ip_unprivileged_port_start" {
					t.Errorf("sysctl is %q, want net.ipv4.ip_unprivileged_port_start", got[0].Name)
				}
				if got[0].Value != "0" {
					t.Errorf("sysctl value is %q, want 0", got[0].Value)
				}
			}
		})
	}
}

// The sysctl has to reach the pod spec, not merely be computable. It is a
// pod-level field: a container securityContext has nowhere to put it.
func Test_newDeployment_CarriesTheSysctlForAPrivilegedPort(t *testing.T) {
	privileged := []apiv1.ContainerPort{{ContainerPort: 80}}
	d := newDeployment("dev", "myapp", 28688, Image, privileged,
		map[string]string{}, map[string]string{}, map[string]string{}, nil,
		PodCredentials{}, 100, 500, 100, 1000)

	psc := d.Spec.Template.Spec.SecurityContext
	if psc == nil || len(psc.Sysctls) == 0 {
		t.Fatalf("a deployment exposing :80 carries no sysctl, so the server cannot bind it")
	}
	if psc.Sysctls[0].Name != "net.ipv4.ip_unprivileged_port_start" {
		t.Errorf("wrong sysctl: %+v", psc.Sysctls)
	}
}

func Test_newDeployment_HasNoSysctlForOrdinaryPorts(t *testing.T) {
	ordinary := []apiv1.ContainerPort{{ContainerPort: 8080}}
	d := newDeployment("dev", "myapp", 28688, Image, ordinary,
		map[string]string{}, map[string]string{}, map[string]string{}, nil,
		PodCredentials{}, 100, 500, 100, 1000)

	psc := d.Spec.Template.Spec.SecurityContext
	if psc != nil && len(psc.Sysctls) != 0 {
		t.Errorf("an ordinary deployment carries %+v; nothing needed it", psc.Sysctls)
	}
}
