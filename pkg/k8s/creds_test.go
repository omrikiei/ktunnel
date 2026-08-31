package k8s

import (
	"strings"
	"testing"

	"github.com/omrikiei/ktunnel/pkg/creds"
	apiv1 "k8s.io/api/core/v1"
)

// argsContain reports whether the argv contains want as consecutive entries.
// Consecutive *entries*, not a substring of one: that distinction is the whole
// point of the test below.
func argsContain(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j, w := range want {
			if args[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// Test_newContainer_SplitsCertAndKeyIntoSeparateArgs is the regression test for
// the bug that made --tls impossible in-cluster: newContainer appended
// fmt.Sprintf("--cert %s", cert), a single argv entry containing a space. No
// flag parser splits that, so the server saw one unknown argument and never
// got a certificate -- which is why expose and inject rejected --tls outright.
func Test_newContainer_SplitsCertAndKeyIntoSeparateArgs(t *testing.T) {
	c := newContainer(28688, "img", []apiv1.ContainerPort{},
		PodCredentials{SecretName: "myapp-ktunnel"}, 250, 1000, 128, 512)

	if !argsContain(c.Args, "--cert", certMountPath+"/tls.crt") {
		t.Errorf("--cert and its value are not two separate args: %q", c.Args)
	}
	if !argsContain(c.Args, "--key", certMountPath+"/tls.key") {
		t.Errorf("--key and its value are not two separate args: %q", c.Args)
	}
	for _, a := range c.Args {
		if len(a) > 7 && a[:7] == "--cert " {
			t.Errorf("argv entry %q still contains a space; the server sees one unknown flag", a)
		}
	}
	if !argsContain(c.Args, "--tls") {
		t.Errorf("a container given a certificate does not enable TLS: %q", c.Args)
	}
}

// The token never goes in the args, because args are readable by anyone with
// `get pods` even when a Secret exists to hold the secret properly.
func Test_newContainer_TakesTheTokenFromTheSecretNotTheArgs(t *testing.T) {
	c := newContainer(28688, "img", []apiv1.ContainerPort{},
		PodCredentials{SecretName: "myapp-ktunnel"}, 250, 1000, 128, 512)

	var tokenEnv *apiv1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == creds.TokenEnvVar {
			tokenEnv = &c.Env[i]
		}
	}
	if tokenEnv == nil {
		t.Fatalf("no %s in the container env: the server has no token to check", creds.TokenEnvVar)
	}
	if tokenEnv.Value != "" {
		t.Errorf("token is a literal value %q in the pod spec despite a Secret existing", tokenEnv.Value)
	}
	if tokenEnv.ValueFrom == nil || tokenEnv.ValueFrom.SecretKeyRef == nil {
		t.Fatal("token is not a secretKeyRef")
	}
	if got := tokenEnv.ValueFrom.SecretKeyRef.Name; got != "myapp-ktunnel" {
		t.Errorf("secretKeyRef names %q, want myapp-ktunnel", got)
	}
}

func Test_newContainer_MountsTheCredentialsSecret(t *testing.T) {
	c := newContainer(28688, "img", []apiv1.ContainerPort{},
		PodCredentials{SecretName: "myapp-ktunnel"}, 250, 1000, 128, 512)

	found := false
	for _, m := range c.VolumeMounts {
		if m.MountPath == certMountPath {
			found = true
			if !m.ReadOnly {
				t.Error("the credentials mount is writable")
			}
		}
	}
	if !found {
		t.Errorf("nothing is mounted at %s, so --cert points at a path that does not exist", certMountPath)
	}
}

// The fallback: secrets are forbidden in this namespace, so the run keeps
// authentication and gives up encryption. A key in a pod spec would be the
// whole channel; a token there is revocable and lasts one run.
func Test_newContainer_FallbackIsAuthenticatedButNotEncrypted(t *testing.T) {
	c := newContainer(28688, "img", []apiv1.ContainerPort{},
		PodCredentials{Token: "literal-token"}, 250, 1000, 128, 512)

	if argsContain(c.Args, "--tls") {
		t.Errorf("the fallback enables TLS with no certificate mounted: %q", c.Args)
	}
	for _, a := range c.Args {
		if a == "--cert" || a == "--key" {
			t.Errorf("the fallback passes %s with no Secret to read it from: %q", a, c.Args)
		}
	}
	if len(c.VolumeMounts) != 0 {
		t.Errorf("the fallback mounts %d volumes, want none", len(c.VolumeMounts))
	}

	var tokenEnv *apiv1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == creds.TokenEnvVar {
			tokenEnv = &c.Env[i]
		}
	}
	if tokenEnv == nil || tokenEnv.Value != "literal-token" {
		t.Fatalf("the fallback did not pass the token as a literal env value: %+v", c.Env)
	}
}

// --insecure, and standalone use: no credentials at all, exactly v2.3.
func Test_newContainer_InsecureCarriesNoCredentials(t *testing.T) {
	c := newContainer(28688, "img", []apiv1.ContainerPort{}, PodCredentials{}, 250, 1000, 128, 512)

	if len(c.Env) != 0 {
		t.Errorf("an insecure container has env %+v, want none", c.Env)
	}
	if len(c.VolumeMounts) != 0 {
		t.Errorf("an insecure container mounts %d volumes, want none", len(c.VolumeMounts))
	}
	if argsContain(c.Args, "--tls") {
		t.Errorf("an insecure container enables TLS: %q", c.Args)
	}
}

func Test_newSecret_HoldsTheThreePiecesTheServerNeeds(t *testing.T) {
	bundle, err := creds.Generate("myapp", "dev")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := newSecret("dev", "myapp-ktunnel", bundle)

	if s.Namespace != "dev" || s.Name != "myapp-ktunnel" {
		t.Errorf("secret is %s/%s, want dev/myapp-ktunnel", s.Namespace, s.Name)
	}
	for _, key := range []string{"tls.crt", "tls.key", "token"} {
		if len(s.Data[key]) == 0 {
			t.Errorf("secret has no %s", key)
		}
	}
}

// The printed manifests and the created objects come from one code path, and
// that is the point of RenderManifests. If expose now creates a Secret and
// mounts it, the printed YAML has to contain it -- otherwise `--print-manifests
// | kubectl apply -f -` yields a tunnel that is unauthenticated and unencrypted
// while the command that printed it would not have been.
func TestRenderManifests_IncludesTheCredentialsSecret(t *testing.T) {
	bundle, err := creds.Generate("myapp", "dev")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out, err := RenderManifests(ManifestOptions{
		Namespace:  "dev",
		Name:       "myapp",
		TunnelPort: 28688,
		Scheme:     "tcp",
		RawPorts:   []string{"8000"},
		Image:      Image,
		Bundle:     bundle,
		Creds:      PodCredentials{SecretName: "myapp"},
	})
	if err != nil {
		t.Fatalf("RenderManifests: %v", err)
	}

	for _, want := range []string{"kind: Secret", "tls.crt", "tls.key", "token"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifests do not contain %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "kind: Deployment") || !strings.Contains(out, "kind: Service") {
		t.Error("rendered manifests lost the Deployment or the Service")
	}
}

func TestRenderManifests_WithoutABundleEmitsNoSecret(t *testing.T) {
	out, err := RenderManifests(ManifestOptions{
		Namespace:  "dev",
		Name:       "myapp",
		TunnelPort: 28688,
		Scheme:     "tcp",
		RawPorts:   []string{"8000"},
		Image:      Image,
	})
	if err != nil {
		t.Fatalf("RenderManifests: %v", err)
	}
	if strings.Contains(out, "kind: Secret") {
		t.Errorf("--insecure rendered a Secret:\n%s", out)
	}
}

// PodCredentialsFor is what --print-manifests uses: it reaches no cluster, so
// it describes the run that would happen if the Secret can be created -- which
// is the run the printed YAML actually produces when applied, since applying
// it creates that Secret.
func TestPodCredentialsFor(t *testing.T) {
	bundle, err := creds.Generate("myapp", "dev")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	got := PodCredentialsFor("myapp", bundle)
	if got.SecretName != "myapp" {
		t.Errorf("SecretName is %q, want myapp", got.SecretName)
	}
	if got.Token != "" {
		t.Error("the token is inlined even though a Secret carries it")
	}

	if insecure := PodCredentialsFor("myapp", nil); insecure != (PodCredentials{}) {
		t.Errorf("--insecure produced %+v, want no credentials", insecure)
	}
}
