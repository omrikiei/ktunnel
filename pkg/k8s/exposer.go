package k8s

import (
	"errors"
	log "github.com/sirupsen/logrus"
	v12 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"ktunnel/pkg/common"
	"time"
)

var supportedSchemes = map[string]v12.Protocol{
	"tcp": v12.ProtocolTCP,
	"ufp": v12.ProtocolUDP,
}

func ExposeAsService(namespace, name *string, tunnelPort int, scheme string, rawPorts []string, readyChan chan<- bool) error {
	getClients(namespace)
	deployment := newDeployment(*namespace, *name, tunnelPort)

	ports := make([]v12.ServicePort, len(rawPorts))
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
		ports[i] = v12.ServicePort{
			Protocol: protocol,
			Port:     parsed.Source,
			TargetPort: intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: parsed.Target,
				StrVal: "",
			},
		}
	}

	service := newService(*namespace, *name, ports)
	creationTime := time.Now().Add(-1 * time.Second)
	_, err := deploymentsClient.Create(deployment)
	if err != nil {
		return err
	}
	newSvc, err := svcClient.Create(service)
	if err != nil {
		return err
	}
	log.Infof("Exposed service's cluster ip is: %s", newSvc.Spec.ClusterIP)
	waitForReady(name, &creationTime, *deployment.Spec.Replicas, readyChan)
	return nil
}

func TeardownExposedService(namespace, name string) error {
	getClients(&namespace)
	log.Infof("Deleting service %s", name)
	err := svcClient.Delete(name, &v1.DeleteOptions{})
	if err != nil {
		return err
	}
	log.Infof("Deleting deployment %s", name)
	err = deploymentsClient.Delete(name, &v1.DeleteOptions{})
	if err != nil {
		return err
	}
	return nil
}
