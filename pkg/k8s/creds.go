package k8s

import (
	"github.com/omrikiei/ktunnel/pkg/creds"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// certMountPath is where the credentials Secret is mounted in the
	// tunnel server container.
	certMountPath = "/ktunnel/creds"
	// credsVolumeName is the pod volume backing that mount.
	credsVolumeName = "ktunnel-creds" // #nosec G101 -- a volume name, not a credential
)

// PodCredentials says how one run's credentials reach the tunnel server
// container. The three states are the three the design allows:
//
//   - SecretName set: the full story. TLS from the mounted certificate, token
//     from a secretKeyRef.
//   - Token set alone: the fallback, for a namespace where `secrets: create`
//     is forbidden. Authenticated, not encrypted -- a token in a pod spec is
//     revocable and lasts one run, where a private key there would be the
//     whole channel.
//   - Neither: `--insecure`, which is v2.3 behaviour exactly.
type PodCredentials struct {
	SecretName string
	Token      string
}

// mountsSecret reports whether this run has a Secret to mount, which is also
// the condition for TLS being available in the pod.
func (c PodCredentials) mountsSecret() bool { return c.SecretName != "" }

// Encrypted reports whether the tunnel server these credentials describe
// serves TLS. The client asks, because attempting a handshake against a
// server that has no certificate is a failed connection and a warning rather
// than a secure tunnel.
func (c PodCredentials) Encrypted() bool { return c.mountsSecret() }

// Authenticated reports whether the tunnel server will check a token.
func (c PodCredentials) Authenticated() bool { return c.SecretName != "" || c.Token != "" }

// args returns the server flags these credentials imply.
func (c PodCredentials) args() []string {
	if !c.mountsSecret() {
		return nil
	}
	// Three separate argv entries, not one string with spaces in it. The
	// old code built `--cert /path` as a single argument, which no flag
	// parser splits, and which is why in-cluster TLS never worked.
	return []string{"--tls", "--cert", certMountPath + "/tls.crt", "--key", certMountPath + "/tls.key"}
}

// env returns the token environment variable, sourced from the Secret when
// there is one and inlined when there is not.
func (c PodCredentials) env() []apiv1.EnvVar {
	switch {
	case c.mountsSecret():
		return []apiv1.EnvVar{{
			Name: creds.TokenEnvVar,
			ValueFrom: &apiv1.EnvVarSource{
				SecretKeyRef: &apiv1.SecretKeySelector{
					LocalObjectReference: apiv1.LocalObjectReference{Name: c.SecretName},
					Key:                  "token",
				},
			},
		}}
	case c.Token != "":
		return []apiv1.EnvVar{{Name: creds.TokenEnvVar, Value: c.Token}}
	default:
		return nil
	}
}

// volumeMounts returns the read-only mount for the credentials Secret.
func (c PodCredentials) volumeMounts() []apiv1.VolumeMount {
	if !c.mountsSecret() {
		return nil
	}
	return []apiv1.VolumeMount{{
		Name:      credsVolumeName,
		MountPath: certMountPath,
		ReadOnly:  true,
	}}
}

// volumes returns the pod volume backing that mount.
func (c PodCredentials) volumes() []apiv1.Volume {
	if !c.mountsSecret() {
		return nil
	}
	// 0400: the container runs as UID 1000 and only reads these.
	mode := int32(0400)
	return []apiv1.Volume{{
		Name: credsVolumeName,
		VolumeSource: apiv1.VolumeSource{
			Secret: &apiv1.SecretVolumeSource{
				SecretName:  c.SecretName,
				DefaultMode: &mode,
			},
		},
	}}
}

// newSecret builds the Secret holding one run's generated credentials.
func newSecret(namespace, name string, b *creds.Bundle) *apiv1.Secret {
	return &apiv1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{deploymentNameLabel: name},
		},
		Type: apiv1.SecretTypeOpaque,
		Data: map[string][]byte{
			"tls.crt": b.ServerCert,
			"tls.key": b.ServerKey,
			"token":   []byte(b.Token),
		},
	}
}

// PodCredentialsFor describes how a bundle would reach the pod, without
// reaching the cluster to find out.
//
// It is what --print-manifests renders from: printing assumes the Secret can
// be created, which is sound because applying the printed manifests is what
// creates it. ExposeAsService does not use this -- it has an API server in
// reach and can find out for certain, including the forbidden case that falls
// back to an inline token.
func PodCredentialsFor(name string, b *creds.Bundle) PodCredentials {
	if b == nil {
		return PodCredentials{}
	}
	return PodCredentials{SecretName: name}
}
