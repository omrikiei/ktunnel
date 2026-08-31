// Package cmd implements the command line interface for ktunnel
package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/omrikiei/ktunnel/pkg/k8s"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	apiv1 "k8s.io/api/core/v1"
)

var PrintManifests bool
var Reuse bool
var Force bool
var DeploymentOnly bool
var PortName string
var ServiceType string
var NodeSelectorTags []string
var DeploymentLabels []string
var DeploymentAnnotations []string
var PodTolerations []string
var ServerCPURequest int64
var ServerCPULimit int64
var ServerMemRequest int64
var ServerMemLimit int64

var exposeCmd = &cobra.Command{
	Use:   "expose [flags] SERVICE_NAME [ports]",
	Short: "Expose local machine as a service on the kubernetes cluster",
	Long: `Creates a Deployment and a Service running the tunnel server, and opens a tunnel
to your machine, so that traffic sent to that Service is forwarded to the same
port on localhost.

Objects are only ever created or adopted, never rewritten. --reuse uses an
existing Deployment and Service exactly as they stand -- your image, your
security context, your resources -- and creates them only if they are not
there. Whatever this run created is removed on exit; whatever it adopted is
left alone. Use --force to delete and recreate instead.

The tunnel is not authenticated: anything in the cluster that can reach the
Service reaches whatever is behind it on your machine. See docs/security.md.`,
	Args: cobra.MinimumNArgs(2),
	Example: `
# Expose a local application running on port 8000 via http
ktunnel expose kewlapp 80:8000

# Use a deployment and service you wrote yourself, as they are
ktunnel expose kewlapp 80:8000 -r
                          
# Expose a local redis server
ktunnel expose redis 6379
              `,
	Run: func(cmd *cobra.Command, args []string) {
		if verbose {
			logger.SetLevel(log.DebugLevel)
			k8s.SetLogLevel(log.DebugLevel)
		}
		// Resolved before anything else: it decides which namespace every
		// object below belongs to. --print-manifests keeps stdout for the
		// YAML, so the line saying where the namespace came from goes to
		// stderr with the rest of the logging.
		namespace, namespaceSource := resolveNamespaceQuietly()
		Namespace = namespace
		if PrintManifests {
			fmt.Fprintf(os.Stderr, "# %s\n", namespaceLine(namespace, namespaceSource))
		} else {
			logger.Info(namespaceLine(namespace, namespaceSource))
		}

		// Create service and deployment
		svcName, ports := args[0], args[1:]
		readyChan := make(chan bool, 1)
		nodeSelectorTags := map[string]string{}
		for _, tag := range NodeSelectorTags {
			parsed := strings.Split(tag, "=")
			if len(parsed) != 2 {
				log.Errorf("failed to parse node selector tag: %v", tag)
				continue
			}
			nodeSelectorTags[parsed[0]] = parsed[1]
		}

		deploymentLabels := map[string]string{}
		for _, label := range DeploymentLabels {
			parsed := strings.Split(label, "=")
			if len(parsed) != 2 {
				log.Errorf("failed to parse deployment label: %v", label)
				continue
			}
			deploymentLabels[parsed[0]] = parsed[1]
		}

		deploymentAnnotations := map[string]string{}
		for _, label := range DeploymentAnnotations {
			parsed := strings.Split(label, "=")
			if len(parsed) != 2 {
				log.Errorf("failed to parse deployment label: %v", label)
				continue
			}
			deploymentAnnotations[parsed[0]] = parsed[1]
		}

		podTolerations := make([]apiv1.Toleration, 0, len(PodTolerations))
		for _, label := range PodTolerations {
			parsed := strings.Split(label, "=")
			if len(parsed) != 2 {
				log.Errorf("failed to parse pod tolerations: %v", label)
				continue
			}
			valueAndEffect := strings.Split(parsed[1], ":")
			if len(valueAndEffect) != 2 {
				log.Errorf("failed to parse pod tolerations: %v", label)
				continue
			}

			podTolerations = append(podTolerations, apiv1.Toleration{
				Key:      parsed[0],
				Operator: apiv1.TolerationOpEqual,
				Value:    valueAndEffect[0],
				Effect:   apiv1.TaintEffect(valueAndEffect[1]),
			})
		}

		manifestOptions := k8s.ManifestOptions{
			Namespace:             Namespace,
			Name:                  svcName,
			TunnelPort:            port,
			Scheme:                Scheme,
			RawPorts:              ports,
			PortName:              PortName,
			Image:                 ServerImage,
			DeploymentOnly:        DeploymentOnly,
			NodeSelectorTags:      nodeSelectorTags,
			DeploymentLabels:      deploymentLabels,
			DeploymentAnnotations: deploymentAnnotations,
			PodTolerations:        podTolerations,
			Cert:                  CertFile,
			Key:                   KeyFile,
			ServiceType:           ServiceType,
			CPURequest:            ServerCPURequest,
			CPULimit:              ServerCPULimit,
			MemRequest:            ServerMemRequest,
			MemLimit:              ServerMemLimit,
		}

		// --print-manifests reaches no cluster, so it comes before the client
		// is built: it works with an unreachable API server, and in a CI job
		// that only wants the YAML.
		if PrintManifests {
			rendered, err := k8s.RenderManifests(manifestOptions)
			if err != nil {
				// stderr, like the namespace line above it: on this path
				// stdout is the data, and half a manifest followed by an
				// error message is worse than an empty pipe.
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			// The manifests go to stdout on their own, so the output can be
			// piped straight into kubectl; where the namespace came from is a
			// log line and belongs on stderr with the rest of them.
			fmt.Print(rendered)
			return
		}

		svc, err := k8s.NewKubeService(KubeContext, Namespace)
		if err != nil {
			log.Fatalf("Failed to create new kube service: %v", err)
		}

		if Force {
			// Said before the delete, for the same reason the plan below is
			// said before the creates: this one removes objects, and it runs
			// against whatever is there, including a deployment the user
			// wrote themselves.
			if DeploymentOnly {
				logger.Infof("--force: deleting deployment %s/%s, if it exists, and creating it anew", Namespace, svcName)
			} else {
				logger.Infof("--force: deleting deployment and service %s/%s, if they exist, and creating them anew", Namespace, svcName)
			}
			err := svc.TeardownExposedService(svcName, DeploymentOnly)
			if err != nil {
				log.Infof("Force delete: Failed deleting k8s objects: %s", err)
			}
		}

		// The tracker comes back holding what this call created, and only
		// that, so teardown below removes exactly what ktunnel put there.
		tracker, err := svc.ExposeAsService(
			Namespace,
			svcName,
			port,
			Scheme,
			ports,
			PortName,
			ServerImage,
			Reuse,
			DeploymentOnly,
			readyChan,
			nodeSelectorTags,
			deploymentLabels,
			deploymentAnnotations,
			podTolerations,
			CertFile,
			KeyFile,
			ServiceType,
			ServerCPURequest,
			ServerCPULimit,
			ServerMemRequest,
			ServerMemLimit,
		)
		if err != nil {
			// Whatever was created before the failure goes with it. The
			// deployment is created before the service, so a service that
			// could not be created used to leave the deployment behind and
			// exit -- and log.Fatalf runs no deferred function to catch it.
			cleanupCreated(tracker)
			log.Fatalf("Failed to expose local machine as a service: %v", err)
		}

		// Teardown removes what this run created, and nothing else. It runs
		// exactly once, whether the command ends on Ctrl+C, on a failed
		// rollout or because the supervisor gave up.
		//
		// This used to key off --reuse rather than off what happened, which
		// was wrong in both directions: --reuse against objects that did not
		// exist created them and then left them in the cluster, and the
		// message promised to remove objects that ktunnel had only adopted.
		// Created here rather than at the top of the command: --print-manifests
		// returns above without ever running a tunnel, and a cancel func that
		// some paths never call is a context leak go vet is right about.
		ctx, cancel := context.WithCancel(context.Background())

		createdDeployments, createdServices := tracker.GetTrackedResources()
		created := len(createdDeployments) + len(createdServices)
		exitMsg := "Got exit signal, closing client tunnels and removing the objects ktunnel created"
		if created == 0 {
			exitMsg = "Got exit signal, closing client tunnels; the deployment and service were already there and are left as they are"
		}
		sess := newTunnelSession(ctx, cancel, exitMsg, func() {
			cleanupCreated(tracker)
		})
		defer sess.finish()

		log.Info("waiting for deployment to be ready")
		ready, interrupted := waitForReady(sess.ctx, readyChan)
		if interrupted {
			return
		}
		if !ready {
			// Not "cleaning up": teardown removes only what this run
			// created, so against adopted objects it deliberately leaves
			// them alone, and a line promising cleanup that never comes
			// sends the user looking for the wrong thing.
			log.Error("deployment failed to become ready")
			// Exit non-zero, like every other way this command can fail. A
			// plain return exited 0, so a systemd unit or a CI step saw
			// success for a tunnel server that never started -- and this
			// branch now documents its exit codes, which has to mean all of
			// them. finish first: os.Exit runs no deferred function, and
			// the deployment and service are already in the cluster.
			sess.finish()
			os.Exit(1)
		}

		// Kube Service
		kubeService, err := k8s.NewKubeService(KubeContext, Namespace)
		if err != nil {
			// Not log.Fatalf: os.Exit would skip the teardown above and
			// leave the deployment and service behind.
			log.Errorf("Failed to start k8s clients: %v", err)
			return
		}

		supervise(sess, forwardAndTunnelAttempt(kubeService, Namespace, svcName, port, ports))
	},
}

// cleanupCreated removes the resources a run created, and is a no-op when it
// created none -- the --reuse case, where every object was already there.
func cleanupCreated(tracker *k8s.ResourceTracker) {
	deployments, services := tracker.GetTrackedResources()
	if len(deployments)+len(services) == 0 {
		return
	}
	// Background rather than the session context, which is already cancelled
	// by the time teardown runs. The tracker carries its own 30s timeout.
	if err := tracker.Cleanup(context.Background()); err != nil {
		logger.Errorf("Failed deleting k8s objects: %s", err)
	}
}

func init() {
	exposeCmd.PreRunE = rejectInClusterTLS("expose")
	exposeCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "TLS cert auth file")
	exposeCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	exposeCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	exposeCmd.Flags().StringVarP(&Namespace, "namespace", "n", "", namespaceFlagUsage)
	exposeCmd.Flags().StringVar(&KubeContext, "context", "", "Kubernetes Context")
	exposeCmd.Flags().StringVarP(&ServerImage, "server-image", "i", fmt.Sprintf("%s:v%s", k8s.Image, version), "Ktunnel server image to use")
	exposeCmd.Flags().StringVar(&CertFile, "cert", "", "TLS certificate file")
	exposeCmd.Flags().StringVar(&KeyFile, "key", "", "TLS key file")
	exposeCmd.Flags().StringVar(&ServiceType, "service-type", "ClusterIP", "exposed service type (ClusterIP, NodePort, LoadBalancer or ExternalName)")
	exposeCmd.Flags().StringVar(&PortName, "portname", "", "specify container port name")
	exposeCmd.Flags().BoolVarP(&Reuse, "reuse", "r", false, "delete k8s objects before expose")
	exposeCmd.Flags().BoolVarP(&Force, "force", "f", false, "deployment & service will be removed before")
	exposeCmd.Flags().BoolVarP(&DeploymentOnly, "deployment-only", "d", false, "create only deployment")
	exposeCmd.Flags().BoolVar(&PrintManifests, "print-manifests", false, "print the deployment and service ktunnel would create, as YAML, and exit without contacting the cluster")
	exposeCmd.Flags().StringSliceVarP(&NodeSelectorTags, "node-selector-tags", "q", []string{}, "tag and value seperated by the '=' character (i.e kubernetes.io/os=linux)")
	exposeCmd.Flags().StringSliceVarP(&DeploymentLabels, "deployment-labels", "l", []string{}, "comma separated list of labels and values seperated by the '=' character (i.e app=application,env=prod)")
	exposeCmd.Flags().StringSliceVarP(&DeploymentAnnotations, "deployment-annotations", "", []string{}, "comma separated list of annotations and values seperated by the '=' character (i.e sidecar.istio.io/inject=false)")
	exposeCmd.Flags().StringSliceVarP(&PodTolerations, "pod-tolerations", "", []string{}, "comma separated list of tolerations seperated by the '=' character (i.e key=value:NoSchedule)")
	exposeCmd.Flags().Int64Var(&ServerCPURequest, "server-cpu-request", 100, "Server container CPU Request in milli-cpus")
	exposeCmd.Flags().Int64Var(&ServerCPULimit, "server-cpu-limit", 500, "Server container CPU Limit in milli-cpus")
	exposeCmd.Flags().Int64Var(&ServerMemRequest, "server-memory-request", 100, "Server container memory request in mega-bytes")
	exposeCmd.Flags().Int64Var(&ServerMemLimit, "server-memory-limit", 1000, "Server container memory limit in mega-bytes")
	addReconnectFlags(exposeCmd)
	rootCmd.AddCommand(exposeCmd)
}
