package k8s

import (
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// apiError says which object a failed API call was about, and -- for the
// classes of failure that have one -- what to do about it.
//
// client-go's own errors are written for someone who already knows which
// request was made: `deployments.apps "api" not found` names neither the
// namespace it looked in nor the context it looked with, and between those two
// lies the answer most of the time. #134 is a user listing everything
// confusing about ktunnel at once, and a good half of it is errors that name
// no object, or name one in the API server's vocabulary rather than in the
// flags that produced it.
//
// verb is what ktunnel was doing in plain words ("read", "update"), kind is
// the object kind lowercased ("deployment"), and the cause is always wrapped,
// so callers can still match on apierrors and the original detail survives.
func apiError(verb, kind, namespace, name string, err error) error {
	object := fmt.Sprintf("%s %s/%s", kind, namespace, name)

	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%s not found; check the name, the --namespace flag and the --context flag "+
			"(kubectl get %s -n %s): %w", object, kind, namespace, err)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("not allowed to %s %s; ktunnel needs get, list, watch, create, update and delete "+
			"on deployments and services, and get, list and watch on pods, in that namespace "+
			"-- the permissions are listed in docs/security.md: %w", verb, object, err)
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("not authenticated to %s %s; the credentials in your kubeconfig context were "+
			"rejected -- they may have expired, or the context may point at another cluster: %w", verb, object, err)
	case apierrors.IsConflict(err):
		return fmt.Errorf("%s changed while ktunnel was updating it; something else is writing to it "+
			"(a controller, a CD system, another ktunnel) -- re-run the command: %w", object, err)
	default:
		return fmt.Errorf("failed to %s %s: %w", verb, object, err)
	}
}

// forwardError names the local port as well as the pod, because the failure a
// user actually meets on this path is on their own machine: a local port
// something else is already listening on. The cluster is not the place to look
// for it, and the fix is a flag.
func forwardError(namespace, podName, localPort string, err error) error {
	if strings.Contains(err.Error(), "address already in use") || strings.Contains(err.Error(), "unable to listen") {
		return fmt.Errorf("could not bind local port %s for the forward to pod %s/%s; something else on this "+
			"machine is listening on it -- stop it, or choose another port with --port: %w",
			localPort, namespace, podName, err)
	}
	return fmt.Errorf("the port forward from local port %s to pod %s/%s failed: %w", localPort, namespace, podName, err)
}
