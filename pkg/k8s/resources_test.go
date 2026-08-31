package k8s

import (
	"testing"

	apiv1 "k8s.io/api/core/v1"
)

// TestNewContainer_ResourcesReadAsKubernetesWritesThem is the second half of
// #118, the half the flags did not answer.
//
// The reporter pasted what `kubectl describe pod` showed them:
//
//	Limits:
//	  cpu:     1
//	  memory:  1e9
//	Requests:
//	  cpu:        500e-3
//	  memory:     100e6
//
// and called the values non-int. They are correct -- 500e-3 cores is 500m --
// but nothing else in a cluster writes them that way, so they read as a bug
// and cannot be compared by eye against a LimitRange, a quota, or the
// deployment next to them.
//
// The cause is the zero-valued resource.Quantity the container was built from:
// its Format is the empty string, which serialises as DecimalExponent. The
// quantities are now constructed with the format Kubernetes itself uses.
func TestNewContainer_ResourcesReadAsKubernetesWritesThem(t *testing.T) {
	c := newContainer(28688, "img", []apiv1.ContainerPort{}, "", "", 250, 1000, 128, 512)

	tests := []struct {
		what string
		got  string
		want string
	}{
		{"cpu request", c.Resources.Requests.Cpu().String(), "250m"},
		{"cpu limit", c.Resources.Limits.Cpu().String(), "1"},
		{"memory request", c.Resources.Requests.Memory().String(), "128M"},
		{"memory limit", c.Resources.Limits.Memory().String(), "512M"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s renders as %q, want %q: nothing else in a cluster writes quantities that way",
				tc.what, tc.got, tc.want)
		}
	}
}

// TestNewContainer_ResourcesKeepTheirValues pins that the formatting change is
// only formatting: the numbers a user passes on the flags are the numbers the
// cluster is asked for.
func TestNewContainer_ResourcesKeepTheirValues(t *testing.T) {
	c := newContainer(28688, "img", []apiv1.ContainerPort{}, "", "", 250, 1000, 128, 512)

	if got := c.Resources.Requests.Cpu().MilliValue(); got != 250 {
		t.Errorf("cpu request is %dm, want 250m", got)
	}
	if got := c.Resources.Limits.Cpu().MilliValue(); got != 1000 {
		t.Errorf("cpu limit is %dm, want 1000m", got)
	}
	if got := c.Resources.Requests.Memory().Value(); got != 128*1000*1000 {
		t.Errorf("memory request is %d bytes, want %d", got, 128*1000*1000)
	}
	if got := c.Resources.Limits.Memory().Value(); got != 512*1000*1000 {
		t.Errorf("memory limit is %d bytes, want %d", got, 512*1000*1000)
	}
}
