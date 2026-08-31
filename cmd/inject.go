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

var injectDeploymentCmd = &cobra.Command{
	Use:   "deployment [flags] DEPLOYMENT_NAME [ports]",
	Short: "Inject server sidecar to a deployment and run the ktunnel client to establish a connection",
	Long: `Adds the tunnel server to a deployment's pod template as a sidecar, waits for the
rollout, and establishes a reverse tunnel from those pods to your machine.

The sidecar's listeners are pod-local: containers in an injected pod reach your
machine at localhost:PORT, and nothing outside the pod is routed through the
tunnel. Every replica is injected, so every replica is tunnelled -- a deployment
with three replicas takes three local ports, counting up from --port, and opens
three streams to your machine. Replicas added while the tunnel is up are picked
up the next time it is rebuilt.

The tunnel is not authenticated. Anything in the cluster that can reach an
injected pod on a tunnelled port reaches whatever is behind it on your machine.
See docs/security.md.`,
	Args: cobra.MinimumNArgs(2),
	Example: `
# Inject a back tunnel from a running deployment to local mysql and redis 
ktunnel inject deployment mydeployment 3306 6379
`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(context.Background())
		if verbose {
			logger.SetLevel(log.DebugLevel)
			k8s.SetLogLevel(log.DebugLevel)
		}
		// Inject
		deployment := args[0]
		readyChan := make(chan bool, 1)
		// Kube Service
		svc, err := k8s.NewKubeService(KubeContext, Namespace)
		if err != nil {
			log.Fatalf("failed creating kube service: %v", err)
		}
		_, err = svc.InjectSidecar(&Namespace, &deployment, &port, ServerImage, CertFile, KeyFile, readyChan, &KubeContext)
		if err != nil {
			log.Fatalf("failed injecting sidecar: %v", err)
		}

		// Ejecting the sidecar runs exactly once, whether the command ends on
		// Ctrl+C, on a failed rollout or because the supervisor gave up. It
		// runs after the supervisor has returned, so the deployment is only
		// patched back once nothing is still forwarding to it.
		sess := newTunnelSession(ctx, cancel, "Stopping streams", func() {
			if !eject {
				return
			}
			ejectReady := make(chan bool, 1)
			ok, err := svc.RemoveSidecar(&Namespace, &deployment, ServerImage, ejectReady, &KubeContext)
			if !ok {
				logger.Errorf("Failed removing tunnel sidecar; %v", err)
				return
			}
			<-ejectReady
			logger.Info("Finished, exiting")
		})
		defer sess.finish()

		log.Info("Waiting for deployment to be ready")
		success, interrupted := waitForReady(sess.ctx, readyChan)
		if interrupted {
			return
		}
		if !success {
			// Not "removing the sidecar": with --eject=false the teardown
			// deliberately leaves it in place.
			log.Error("deployment failed to become ready")
			// Exit non-zero, for the reason given in expose: a rollout that
			// never completed is not a successful run, and this branch
			// documents its exit codes.
			sess.finish()
			os.Exit(1)
		}

		supervise(sess, forwardAndTunnelAttempt(svc, Namespace, deployment, port, args[1:]))
	},
}

func init() {
	injectCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "TLS cert auth file")
	injectCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	injectCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	injectCmd.Flags().StringVarP(&Namespace, "namespace", "n", "default", "Namespace")
	injectCmd.Flags().StringVar(&KubeContext, "context", "", "Kubernetes Context")
	injectCmd.Flags().StringVar(&CertFile, "cert", "", "TLS certificate file")
	injectCmd.Flags().StringVar(&KeyFile, "key", "", "TLS key file")
	injectDeploymentCmd.PreRunE = rejectInClusterTLS("inject deployment")
	injectDeploymentCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "tls cert auth file")
	injectDeploymentCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	injectDeploymentCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	injectDeploymentCmd.Flags().StringVarP(&Namespace, "namespace", "n", "default", "Namespace")
	injectDeploymentCmd.Flags().StringVar(&KubeContext, "context", "", "Kubernetes Context")
	injectDeploymentCmd.Flags().StringVarP(&ServerImage, "server-image", "i", fmt.Sprintf("%s:v%s", k8s.Image, version), "Ktunnel server image to use")
	injectDeploymentCmd.Flags().StringVar(&CertFile, "cert", "", "TLS certificate file")
	injectDeploymentCmd.Flags().StringVar(&KeyFile, "key", "", "TLS key file")
	injectDeploymentCmd.Flags().BoolVarP(&eject, "eject", "e", true, "Eject the sidecar when finished")
	addReconnectFlags(injectDeploymentCmd)
	injectCmd.AddCommand(injectDeploymentCmd)
	rootCmd.AddCommand(injectCmd)
}
