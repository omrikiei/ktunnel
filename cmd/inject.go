// Package cmd implements the command line interface for ktunnel
package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/omrikiei/ktunnel/pkg/k8s"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var Namespace string
var KubeContext string
var ServerImage string
var eject bool

var injectCmd = &cobra.Command{
	Use:   "inject",
	Short: "Inject server sidecar to the cluster and run the ktunnel client to establish a connection",
	Long: `This command accepts a pod/deployment and injects the tunnel sidecar to that artifact,
                        it then establishes a reverse tunnel`,
}

// injectLong is the help both subcommands share, with the workload kind read
// into it. The sentences are the same because the behaviour is: a StatefulSet
// is not a special case of injecting, it is the same injection into a template
// a different controller owns.
func injectLong(kind k8s.WorkloadKind) string {
	return fmt.Sprintf(`Adds the tunnel server to a %s's pod template as a sidecar, waits for the
rollout, and establishes a reverse tunnel from those pods to your machine.

The sidecar's listeners are pod-local: containers in an injected pod reach your
machine at localhost:PORT, and nothing outside the pod is routed through the
tunnel. Every replica is injected, so every replica is tunnelled -- a %s
with three replicas takes three local ports, counting up from --port, and opens
three streams to your machine. Replicas added while the tunnel is up are picked
up the next time it is rebuilt.

The tunnel is not authenticated. Anything in the cluster that can reach an
injected pod on a tunnelled port reaches whatever is behind it on your machine.
See docs/security.md.`, kind, kind)
}

var injectDeploymentCmd = &cobra.Command{
	Use:   "deployment [flags] DEPLOYMENT_NAME [ports]",
	Short: "Inject server sidecar to a deployment and run the ktunnel client to establish a connection",
	Long:  injectLong(k8s.KindDeployment),
	Args:  cobra.MinimumNArgs(2),
	Example: `
# Inject a back tunnel from a running deployment to local mysql and redis
ktunnel inject deployment mydeployment 3306 6379
`,
	Run: func(cmd *cobra.Command, args []string) {
		runInject(k8s.KindDeployment, args)
	},
}

// injectStatefulSetCmd is #91: an application that exists only as a
// StatefulSet -- a PHP app with xdebug, in the report -- had no way in at all,
// because `inject deployment` looks up a Deployment by that name and there
// isn't one. The error said `deployments.apps "owl-app" not found`, which is
// true, and leaves the user with nothing to try next.
//
// Local ports are assigned in ordinal order, which is the one thing this
// subcommand does differently from its sibling and the reason it is worth
// saying in the help: a StatefulSet's pods are identities, not replicas.
var injectStatefulSetCmd = &cobra.Command{
	Use:     "statefulset [flags] STATEFULSET_NAME [ports]",
	Aliases: []string{"sts"},
	Short:   "Inject server sidecar to a statefulset and run the ktunnel client to establish a connection",
	Long: injectLong(k8s.KindStatefulSet) + `

Local ports are assigned in ordinal order: with --port 28688 and three pods,
28688 reaches NAME-0, 28689 reaches NAME-1 and 28690 reaches NAME-2, and stays
that way across reconnects.

A statefulset whose updateStrategy is OnDelete does not restart its pods when
its template changes; ktunnel says so before it patches anything, and waits for
you to delete them.`,
	Args: cobra.MinimumNArgs(2),
	Example: `
# Inject a back tunnel from a running statefulset to a local xdebug listener
ktunnel inject statefulset owl-app 9003
`,
	Run: func(cmd *cobra.Command, args []string) {
		runInject(k8s.KindStatefulSet, args)
	},
}

// runInject is the whole of `inject`, for whichever workload kind was asked
// for. Nothing below branches on the kind: it names the object in the log
// lines and it selects the client that reads and writes it, and that is the
// entire difference between the two subcommands.
func runInject(kind k8s.WorkloadKind, args []string) {
	ctx, cancel := context.WithCancel(context.Background())
	if verbose {
		logger.SetLevel(log.DebugLevel)
		k8s.SetLogLevel(log.DebugLevel)
	}
	Namespace = resolveNamespace()

	// Inject
	objectName := args[0]
	readyChan := make(chan bool, 1)
	// Kube Service
	svc, err := k8s.NewKubeService(KubeContext, Namespace)
	if err != nil {
		log.Fatalf("failed creating kube service: %v", err)
	}
	// Said before the rollout starts: injecting modifies an object the user
	// owns and restarts every one of its pods.
	plan, err := svc.PlanInject(Namespace, objectName, kind, ServerImage, port)
	if err != nil {
		log.Fatalf("%v", err)
	}
	for _, line := range plan.Describe(eject) {
		logger.Info(line)
	}

	// Token-only, and deliberately no volume. inject patches a workload
	// ktunnel does not own, and eject has to be a clean reverse of it: one
	// container in, one container out. Adding a volume and a volumeMount
	// spreads the patch across two more parts of the spec, where a partial
	// eject leaves debris in someone else's object -- a worse outcome than
	// the encryption it would buy, given the sidecar's listeners are
	// pod-local to begin with.
	bundle, err := generateCredentials(objectName, Namespace)
	if err != nil {
		log.Fatalf("%v", err)
	}
	podCreds := k8s.PodCredentials{}
	if bundle != nil {
		podCreds.Token = bundle.Token
	}
	tunnelCreds = sessionCredentials{bundle: bundle, encrypted: false}

	_, err = svc.InjectSidecar(&Namespace, &objectName, kind, &port, ServerImage, podCreds, readyChan, &KubeContext)
	if err != nil {
		log.Fatalf("failed injecting sidecar: %v", err)
	}

	// Ejecting the sidecar runs exactly once, whether the command ends on
	// Ctrl+C, on a failed rollout or because the supervisor gave up. It
	// runs after the supervisor has returned, so the object is only patched
	// back once nothing is still forwarding to it.
	sess := newTunnelSession(ctx, cancel, "Stopping streams", func() {
		if !eject {
			return
		}
		ejectReady := make(chan bool, 1)
		ok, err := svc.RemoveSidecar(&Namespace, &objectName, kind, ServerImage, ejectReady, &KubeContext)
		if !ok {
			logger.Errorf("Failed removing the ktunnel container from %s %s/%s: %v", kind, Namespace, objectName, err)
			logger.Errorf("The container is still in the %s; remove it with `kubectl edit %s %s -n %s`, or re-run `ktunnel inject %s %s` and stop it again",
				kind, kind, objectName, Namespace, kind, objectName)
			return
		}
		<-ejectReady
		logger.Info("Finished, exiting")
	})
	defer sess.finish()

	log.Infof("Waiting for %s to be ready", kind)
	success, interrupted := waitForReady(sess.ctx, readyChan)
	if interrupted {
		return
	}
	if !success {
		// Not "removing the sidecar": with --eject=false the teardown
		// deliberately leaves it in place.
		log.Errorf("%s failed to become ready", kind)
		// Exit non-zero, for the reason given in expose: a rollout that
		// never completed is not a successful run, and this branch
		// documents its exit codes.
		sess.finish()
		os.Exit(1)
	}

	supervise(sess, withTLSDowngrade(forwardAndTunnelAttempt(svc, kind, Namespace, objectName, port, args[1:])))
}

// addInjectFlags gives a subcommand the flags `inject` takes. Both
// subcommands take exactly the same ones -- a StatefulSet is not configured
// differently from a Deployment -- so they are registered from one place
// rather than copied, which is how injectDeploymentCmd and injectCmd already
// drifted apart over --server-image and --eject.
func addInjectFlags(cmd *cobra.Command) {
	cmd.PreRunE = noteDeprecatedTLS
	cmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "tls cert auth file")
	cmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	cmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	cmd.Flags().StringVarP(&Namespace, "namespace", "n", "", namespaceFlagUsage)
	cmd.Flags().StringVar(&KubeContext, "context", "", "Kubernetes Context")
	cmd.Flags().StringVarP(&ServerImage, "server-image", "i", fmt.Sprintf("%s:v%s", k8s.Image, version), "Ktunnel server image to use")
	cmd.Flags().StringVar(&CertFile, "cert", "", "TLS certificate file")
	cmd.Flags().StringVar(&KeyFile, "key", "", "TLS key file")
	cmd.Flags().BoolVarP(&eject, "eject", "e", true, "Eject the sidecar when finished")
	addReconnectFlags(cmd)
}

func init() {
	injectCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "TLS cert auth file")
	injectCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	injectCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	injectCmd.Flags().StringVarP(&Namespace, "namespace", "n", "", namespaceFlagUsage)
	injectCmd.Flags().StringVar(&KubeContext, "context", "", "Kubernetes Context")
	injectCmd.Flags().StringVar(&CertFile, "cert", "", "TLS certificate file")
	injectCmd.Flags().StringVar(&KeyFile, "key", "", "TLS key file")
	addInjectFlags(injectDeploymentCmd)
	addInjectFlags(injectStatefulSetCmd)
	injectCmd.AddCommand(injectDeploymentCmd)
	injectCmd.AddCommand(injectStatefulSetCmd)
	rootCmd.AddCommand(injectCmd)
}
