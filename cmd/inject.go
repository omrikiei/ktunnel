// Package cmd implements the command line interface for ktunnel
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"github.com/omrikiei/ktunnel/pkg/client"
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
	Args:  cobra.MinimumNArgs(2),
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
		o := sync.Once{}
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

		done := make(chan bool, 1)

		// Signal hook to remove sidecar
		sigs := make(chan os.Signal, 1)
		wg := &sync.WaitGroup{}
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
		stopChan := make(chan struct{}, 1)

		go func() {
			o.Do(func() {
				<-sigs
				log.Info("Stopping streams")
				cancel()
				wg.Wait()
				if eject {
					readyChan = make(chan bool, 1)
					ok, err := svc.RemoveSidecar(&Namespace, &deployment, ServerImage, readyChan, &KubeContext)
					if !ok {
						log.Errorf("Failed removing tunnel sidecar; %v", err)
					}
					<-readyChan
				}
				log.Info("Finished, exiting")
				done <- true

			})
		}()

		log.Info("Waiting for deployment to be ready")
		success, interrupted := waitForReady(ctx, readyChan)
		if interrupted {
			<-done
			return
		}
		if !success {
			sigs <- syscall.SIGQUIT
			<-done
			return
		}

		// port-Forward
		strPort := strconv.FormatInt(int64(port), 10)
		// Create a tunnel client for each replica
		sourcePorts, fwdErrChan, err := svc.PortForward(Namespace, deployment, strPort, wg, stopChan)
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
		// handler, leaving the user's own deployment patched with a sidecar behind after a
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
					client.WithTunnels(Scheme, args[1:]...),
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
	injectCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "TLS cert auth file")
	injectCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	injectCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	injectCmd.Flags().StringVarP(&Namespace, "namespace", "n", "default", "Namespace")
	injectCmd.Flags().StringVar(&KubeContext, "context", "", "Kubernetes Context")
	injectCmd.Flags().StringVar(&CertFile, "cert", "", "TLS certificate file")
	injectCmd.Flags().StringVar(&KeyFile, "key", "", "TLS key file")
	injectDeploymentCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "tls cert auth file")
	injectDeploymentCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	injectDeploymentCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	injectDeploymentCmd.Flags().StringVarP(&Namespace, "namespace", "n", "default", "Namespace")
	injectDeploymentCmd.Flags().StringVar(&KubeContext, "context", "", "Kubernetes Context")
	injectDeploymentCmd.Flags().StringVarP(&ServerImage, "server-image", "i", fmt.Sprintf("%s:v%s", k8s.Image, version), "Ktunnel server image to use")
	injectDeploymentCmd.Flags().StringVar(&CertFile, "cert", "", "TLS certificate file")
	injectDeploymentCmd.Flags().StringVar(&KeyFile, "key", "", "TLS key file")
	injectDeploymentCmd.Flags().BoolVarP(&eject, "eject", "e", true, "Eject the sidecar when finished")
	injectCmd.AddCommand(injectDeploymentCmd)
	rootCmd.AddCommand(injectCmd)
}
