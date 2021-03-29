package k8s

import (
	"context"
	"errors"
	"fmt"
	"github.com/omrikiei/ktunnel/pkg/common"
	log "github.com/sirupsen/logrus"
	v12 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"time"
)

var supportedSchemes = map[string]v12.Protocol{
	"tcp": v12.ProtocolTCP,
	"udp": v12.ProtocolUDP,
}

func ExposeAsService(namespace, name *string, tunnelPort int, scheme string, rawPorts []string, image string, readyChan chan<- bool) error {
	getClients(namespace)

	ports := make([]v12.ServicePort, len(rawPorts))
	ctrPorts := make([]v12.ContainerPort, len(ports))
	protocol, ok := supportedSchemes[scheme]
	if ok == false {
		return errors.New("unsupported scheme")
	}
	for i, p := range rawPorts {
		parsed, err := common.ParsePorts(p)
		if err != nil {
			log.Errorf("Failed to parse %s, skipping", p)
			continue
		}
		portname := fmt.Sprintf("%s-%d", scheme, parsed.Source)
		ports[i] = v12.ServicePort{
			Protocol: protocol,
			Name:     portname,
			Port:     parsed.Source,
			TargetPort: intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: parsed.Source,
				StrVal: "",
			},
		}
		ctrPorts[i] = v12.ContainerPort{
			ContainerPort: parsed.Source,
			Protocol:      protocol,
			Name:          portname,
		}
	}

	deployment := newDeployment(*namespace, *name, tunnelPort, image, ctrPorts)

	service := newService(*namespace, *name, ports)
	creationTime := time.Now().Add(-1 * time.Second)
	_, err := deploymentsClient.Create(context.Background(), deployment, v1.CreateOptions{
		TypeMeta:     v1.TypeMeta{},
		DryRun:       nil,
		FieldManager: "",
	})
	if err != nil {
		return err
	}
	newSvc, err := svcClient.Create(context.Background(), service, v1.CreateOptions{
		TypeMeta:     v1.TypeMeta{},
		DryRun:       nil,
		FieldManager: "",
	})
	if err != nil {
		return err
	}
	log.Infof("Exposed service's cluster ip is: %s", newSvc.Spec.ClusterIP)
	waitForReady(name, creationTime, *deployment.Spec.Replicas, readyChan)
	return nil
}

func TeardownExposedService(namespace, name string) error {
	getClients(&namespace)
	log.Infof("Deleting service %s", name)
	err := svcClient.Delete(context.Background(), name, v1.DeleteOptions{})
	if err != nil {
		return err
	}
	log.Infof("Deleting deployment %s", name)
	err = deploymentsClient.Delete(context.Background(), name, v1.DeleteOptions{})
	if err != nil {
		return err
	}
	return nil
}
