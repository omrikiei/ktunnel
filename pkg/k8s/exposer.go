package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/omrikiei/ktunnel/pkg/common"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	v12 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var supportedSchemes = map[string]v12.Protocol{
	"tcp": v12.ProtocolTCP,
	"udp": v12.ProtocolUDP,
}

func ExposeAsService(namespace, name *string, tunnelPort int, scheme string, rawPorts []string, image string, Reuse bool, readyChan chan<- bool) error {
	getClients(namespace)

	ports := make([]v12.ServicePort, len(rawPorts))
	ctrPorts := make([]v12.ContainerPort, len(ports))
	protocol, ok := supportedSchemes[scheme]
	if !ok {
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

	var d *appsv1.Deployment
	var err error
	deploymentCreated := false
	_, err = deploymentsClient.Get(context.Background(), *name, v1.GetOptions{})
	if err != nil && apierrors.IsNotFound(err) {
		d, err = deploymentsClient.Create(context.Background(), deployment, v1.CreateOptions{
			TypeMeta:     v1.TypeMeta{},
			DryRun:       nil,
			FieldManager: "",
		})
		if err != nil {
			return err
		}
		deploymentCreated = true
	}
	if !deploymentCreated && Reuse {
		patch, err := json.Marshal(deployment)
		if err != nil {
			return err
		}
		d, err = deploymentsClient.Patch(context.Background(), *name, types.MergePatchType, patch, v1.PatchOptions{
			TypeMeta:     v1.TypeMeta{},
			DryRun:       nil,
			FieldManager: "",
		})
		time.Sleep(time.Millisecond * 300)
		if err != nil {
			return err
		}
	}

	if d == nil {
		if !deploymentCreated {
			return errors.New("deployment with same name already exists")
		}
		return errors.New("error in creating deployment")
	}

	var newSvc *v12.Service
	serviceCreated := false
	_, err = svcClient.Get(context.Background(), *name, v1.GetOptions{})
	if err != nil && apierrors.IsNotFound(err) {

		newSvc, err = svcClient.Create(context.Background(), service, v1.CreateOptions{
			TypeMeta:     v1.TypeMeta{},
			DryRun:       nil,
			FieldManager: "",
		})

		if err != nil {
			return err
		}
		serviceCreated = true
	}
	if !serviceCreated && Reuse {
		patch, err := json.Marshal(service)
		if err != nil {
			return err
		}
		newSvc, err = svcClient.Patch(context.Background(), *name, types.MergePatchType, patch, v1.PatchOptions{
			TypeMeta:     v1.TypeMeta{},
			DryRun:       nil,
			FieldManager: "",
		})
		time.Sleep(time.Millisecond * 300)
		if err != nil {
			return err
		}
	}
	if newSvc == nil {
		if !serviceCreated {
			return errors.New("service with same name already exists")
		}
		return errors.New("error in creating service")
	}

	log.Infof("Exposed service's cluster ip is: %s", newSvc.Spec.ClusterIP)
	waitForReady(name, d.GetCreationTimestamp().Time, *deployment.Spec.Replicas, readyChan)
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
