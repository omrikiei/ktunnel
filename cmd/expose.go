// Package cmd implements the command line interface for ktunnel
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/omrikiei/ktunnel/pkg/client"
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
		o := sync.Once{}

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
		sigs := make(chan os.Signal, 1)
		wg := &sync.WaitGroup{}
		done := make(chan bool, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

		// Teardown
		go func() {
			o.Do(func() {
				<-sigs
				if Reuse {
					log.Info("Got exit signal, closing client tunnels")
				} else {
					log.Info("Got exit signal, closing client tunnels and removing k8s objects")
				}
				cancel()
				if !Reuse {
					err := svc.TeardownExposedService(svcName, DeploymentOnly)
					if err != nil {
						log.Errorf("Failed deleting k8s objects: %s", err)
					}
				}
				done <- true
			})
		}()

		log.Info("waiting for deployment to be ready")
		ready, interrupted := waitForReady(ctx, readyChan)
		if interrupted {
			// The signal handler is already tearing down; wait for it
			// rather than racing ahead to port-forward.
			<-done
			return
		}
		if !ready {
			log.Error("deployment failed to become ready, cleaning up")
			sigs <- syscall.SIGINT
			<-done
			return
		}

		// Kube Service
		kubeService, err := k8s.NewKubeService(KubeContext, Namespace)
		if err != nil {
			log.Fatalf("Failed to start k8s clients: %v", err)
			os.Exit(1)
		}
		// port-Forward
		strPort := strconv.FormatInt(int64(port), 10)
		stopChan := make(chan struct{}, 1)
		// Create a tunnel client for each replica
		sourcePorts, fwdErrChan, err := kubeService.PortForward(Namespace, svcName, strPort, wg, stopChan)
		if err != nil {
			log.Fatalf("Failed to run port forwarding: %v", err)
			os.Exit(1)
		}
		// A forward that dies later takes its tunnel with it. Reporting it
		// is all we can do for now; reconnecting is the supervisor's job.
		// The range ends when PortForward closes the channel, once the
		// forwards it is watching are gone.
		go func() {
			for err := range fwdErrChan {
				log.Errorf("Port forwarding failed: %v", err)
			}
		}()
		// RunClient used to return only when its context was cancelled, so
		// this path never ran. Now that a lost tunnel gets here, log.Fatalf
		// would call os.Exit and skip the teardown that lives in the signal
		// handler, leaving the deployment and service it created behind after a
		// network blip. Ask for the shutdown Ctrl+C asks for, and record
		// that this exit is not a clean one.
		//
		// The signal is sent but done is deliberately not read here: it
		// carries a single value the main goroutine is already waiting on,
		// and a second receiver would take it and leave the command blocked
		// forever. Retrying instead of shutting down is the supervisor's
		// job, in the next commit.
		clientFailed := make(chan struct{}, 1)
		shutdown := func() {
			// Neither send blocks: several replicas can fail at once, and
			// one record and one signal are all that is needed.
			select {
			case clientFailed <- struct{}{}:
			default:
			}
			select {
			case sigs <- syscall.SIGINT:
			default:
				// The buffer already holds a signal the handler has not
				// picked up yet; it will act on that one.
			}
		}
		for _, srcPort := range *sourcePorts {
			go func(port string) {
				p, err := strconv.ParseInt(port, 10, 0)
				if err != nil {
					log.Errorf("Failed to parse the forwarded local port %q: %v", port, err)
					shutdown()
					return
				}
				prt := int(p)
				opts := []client.Option{
					client.WithServer(Host, prt),
					client.WithTunnels(Scheme, ports...),
					client.WithLogger(&logger),
				}
				if tls {
					opts = append(opts, client.WithTLS(CaFile, ServerHostOverride))
				}
				if err := client.RunClient(ctx, opts...); err != nil {
					log.Errorf("Tunnel lost, shutting down: %v", err)
					shutdown()
					return
				}
			}(srcPort)
		}
		<-done

		// A tunnel that ended on its own is not a clean exit. The restart
		// wrappers this feature exists to replace -- Restart=on-failure,
		// `until ktunnel ...` -- read the exit code, and log.Fatalf, which
		// the client goroutines above used to call, exited 1. Which code
		// belongs to which give-up policy is the supervisor's to decide.
		select {
		case <-clientFailed:
			os.Exit(1)
		default:
		}
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
	rootCmd.AddCommand(exposeCmd)
}
