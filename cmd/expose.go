// Package cmd implements the command line interface for ktunnel
package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/omrikiei/ktunnel/pkg/k8s"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	apiv1 "k8s.io/api/core/v1"
)

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
	Long: `This command would inject a new service and deployment to the cluster, and open the tunnel to the server 
                        forwarding tunnel ingress traffic to the the same port on localhost`,
	Args: cobra.MinimumNArgs(2),
	Example: `
# Expose a local application running on port 8000 via http
ktunnel expose kewlapp 80:8000

ktunnel expose kewlapp 80:8000 -r
                          
# Expose a local redis server
ktunnel expose redis 6379
              `,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(context.Background())
		if verbose {
			logger.SetLevel(log.DebugLevel)
			k8s.SetLogLevel(log.DebugLevel)
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

		svc, err := k8s.NewKubeService(KubeContext, Namespace)
		if err != nil {
			log.Fatalf("Failed to create new kube service: %v", err)
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

		if Force {
			err := svc.TeardownExposedService(svcName, DeploymentOnly)
			if err != nil {
				log.Infof("Force delete: Failed deleting k8s objects: %s", err)
			}
		}

		err = svc.ExposeAsService(
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
			KubeContext,
			ServerCPURequest,
			ServerCPULimit,
			ServerMemRequest,
			ServerMemLimit,
		)
		if err != nil {
			log.Fatalf("Failed to expose local machine as a service: %v", err)
		}
		// Teardown of the deployment and service this created runs exactly
		// once, whether the command ends on Ctrl+C, on a failed rollout or
		// because the supervisor gave up.
		exitMsg := "Got exit signal, closing client tunnels and removing k8s objects"
		if Reuse {
			exitMsg = "Got exit signal, closing client tunnels"
		}
		sess := newTunnelSession(ctx, cancel, exitMsg, func() {
			if Reuse {
				return
			}
			if err := svc.TeardownExposedService(svcName, DeploymentOnly); err != nil {
				logger.Errorf("Failed deleting k8s objects: %s", err)
			}
		})
		defer sess.finish()

		log.Info("waiting for deployment to be ready")
		ready, interrupted := waitForReady(sess.ctx, readyChan)
		if interrupted {
			return
		}
		if !ready {
			// Not "cleaning up": under -r/--reuse the teardown deliberately
			// leaves the deployment and service alone, and a line promising
			// cleanup that never comes sends the user looking for the wrong
			// thing.
			log.Error("deployment failed to become ready")
			return
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

func init() {
	exposeCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "TLS cert auth file")
	exposeCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	exposeCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	exposeCmd.Flags().StringVarP(&Namespace, "namespace", "n", "default", "Namespace")
	exposeCmd.Flags().StringVar(&KubeContext, "context", "", "Kubernetes Context")
	exposeCmd.Flags().StringVarP(&ServerImage, "server-image", "i", fmt.Sprintf("%s:v%s", k8s.Image, version), "Ktunnel server image to use")
	exposeCmd.Flags().StringVar(&CertFile, "cert", "", "TLS certificate file")
	exposeCmd.Flags().StringVar(&KeyFile, "key", "", "TLS key file")
	exposeCmd.Flags().StringVar(&ServiceType, "service-type", "ClusterIP", "exposed service type (ClusterIP, NodePort, LoadBalancer or ExternalName)")
	exposeCmd.Flags().StringVar(&PortName, "portname", "", "specify container port name")
	exposeCmd.Flags().BoolVarP(&Reuse, "reuse", "r", false, "delete k8s objects before expose")
	exposeCmd.Flags().BoolVarP(&Force, "force", "f", false, "deployment & service will be removed before")
	exposeCmd.Flags().BoolVarP(&DeploymentOnly, "deployment-only", "d", false, "create only deployment")
	exposeCmd.Flags().StringSliceVarP(&NodeSelectorTags, "node-selector-tags", "q", []string{}, "tag and value seperated by the '=' character (i.e kubernetes.io/os=linux)")
	exposeCmd.Flags().StringSliceVarP(&DeploymentLabels, "deployment-labels", "l", []string{}, "comma separated list of labels and values seperated by the '=' character (i.e app=application,env=prod)")
	exposeCmd.Flags().StringSliceVarP(&DeploymentAnnotations, "deployment-annotations", "", []string{}, "comma separated list of annotations and values seperated by the '=' character (i.e sidecar.istio.io/inject=false)")
	exposeCmd.Flags().StringSliceVarP(&PodTolerations, "pod-tolerations", "", []string{}, "comma separated list of tolerations seperated by the '=' character (i.e key=value:NoSchedule)")
	exposeCmd.Flags().Int64Var(&ServerCPURequest, "server-cpu-request", 100, "Server container CPU Request in milli-cpus")
	exposeCmd.Flags().Int64Var(&ServerCPULimit, "server-cpu-limit", 500, "Server container CPU Limit in milli-cpus")
	exposeCmd.Flags().Int64Var(&ServerMemRequest, "server-memory-request", 100, "Server container CPU Request in mega-bytes")
	exposeCmd.Flags().Int64Var(&ServerMemLimit, "server-memory-limit", 1000, "Server container CPU Limit in mega-bytes")
	addReconnectFlags(exposeCmd)
	rootCmd.AddCommand(exposeCmd)
}
