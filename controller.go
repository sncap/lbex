/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/sostheim/lbex/annotations"
	"github.com/sostheim/lbex/nginx"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
)

var (
	resyncPeriod = 30 * time.Second
)

// List Watch (lw) Controller (lwc)
type lwController struct {
	controller *cache.Controller
	stopCh     chan struct{}
}

// External LB Controller (lbex)
type lbExController struct {
	clientset *kubernetes.Clientset

	endpointsLWC   *lwController
	endpointStore  cache.Store
	endpointsQueue *TaskQueue

	servciesLWC   *lwController
	servicesStore cache.Store
	servicesQueue *TaskQueue

	nodesLWC   *lwController
	nodesStore cache.Store
	nodesQueue *TaskQueue

	// The service to provide load balancing for, or "all" if empty
	service string

	stopCh chan struct{}

	cfgtor *nginx.Configurator
}

func newLbExController(clientset *kubernetes.Clientset, service *string) *lbExController {
	// local testing -> no actual NGINX instance
	cfgType := nginx.StreamCfg
	if runtime.GOOS == "darwin" {
		cfgType = nginx.LocalCfg
	}

	// Create and start the NGINX LoadBalancer
	ngxc, _ := nginx.NewNginxController(cfgType, "/etc/nginx/", false)
	ngxc.Start()

	configtor := nginx.NewConfigurator(ngxc)

	// create external loadbalancer controller struct
	lbexc := lbExController{
		clientset: clientset,
		stopCh:    make(chan struct{}),
		service:   *service,
		cfgtor:    configtor,
	}
	lbexc.nodesQueue = NewTaskQueue(lbexc.syncNodes)
	lbexc.nodesLWC = newNodesListWatchControllerForClientset(&lbexc)
	lbexc.servicesQueue = NewTaskQueue(lbexc.syncServices)
	lbexc.servciesLWC = newServicesListWatchControllerForClientset(&lbexc)
	lbexc.endpointsQueue = NewTaskQueue(lbexc.syncEndpoints)
	lbexc.endpointsLWC = newEndpointsListWatchControllerForClientset(&lbexc)

	return &lbexc
}

func (lbex *lbExController) run() {
	// run the controller and queue goroutines
	go lbex.nodesLWC.controller.Run(lbex.stopCh)
	go lbex.nodesQueue.Run(time.Second, lbex.stopCh)

	go lbex.endpointsLWC.controller.Run(lbex.stopCh)
	go lbex.endpointsQueue.Run(time.Second, lbex.stopCh)

	// Allow time for the initial cache update for all nodes and endpoints to take place 1st
	time.Sleep(5 * time.Second)
	go lbex.servciesLWC.controller.Run(lbex.stopCh)
	go lbex.servicesQueue.Run(time.Second, lbex.stopCh)

}

func (lbex *lbExController) syncNodes(obj interface{}) error {
	if lbex.nodesQueue.IsShuttingDown() {
		return nil
	}

	key, ok := obj.(string)
	if !ok {
		return errors.New("type assertion faild for key string")
	}

	storeObj, exists, err := lbex.nodesStore.GetByKey(key)
	if err != nil {
		return err
	}
	affectedServices := []string{}
	if !exists {
		glog.V(2).Infof("deleting node: %v\n", key)
		affectedServices = lbex.cfgtor.DeleteNode(key)
	} else {
		err = ValidateNodeObjectType(storeObj)
		if err != nil {
			glog.V(3).Infof("failed ValidateNodeObjectType(): err: %v", err)
			return nil
		}
		addrs, err := GetNodeAddress(storeObj)
		if err != nil {
			glog.V(3).Infof("failed GetNodeAddress(): err: %v", err)
			return nil
		}
		active := IsNodeScheduleable(storeObj)
		node := nginx.Node{
			Name:       key,
			Hostname:   addrs.Hostname,
			ExternalIP: addrs.ExternalIP,
			InternalIP: addrs.InternalIP,
			Active:     active,
		}
		glog.V(3).Infof("add/update node: %s", key)
		affectedServices = lbex.cfgtor.AddOrUpdateNode(node)
	}
	glog.V(4).Infof("queuing updates for affected services: %v", affectedServices)
	for _, svc := range affectedServices {
		lbex.servicesQueue.Enqueue(svc)
	}
	return nil
}

func (lbex *lbExController) syncServices(obj interface{}) error {
	if lbex.servicesQueue.IsShuttingDown() {
		return nil
	}

	key, ok := obj.(string)
	if !ok {
		return errors.New("syncServices: type assertion faild for key string")
	}
	// some-namespace/some-service -> some-namespace-some-service
	filename := strings.Replace(key, "/", "-", -1)

	storeObj, exists, err := lbex.servicesStore.GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		glog.V(2).Infof("syncServices: deleting service: %v\n", key)
		lbex.cfgtor.DeleteConfiguration(filename, nginx.StreamCfg)
	} else {
		err = ValidateServiceObjectType(storeObj)
		if err != nil {
			glog.V(3).Infof("syncServices: ValidateServiceObjectType(): err: %v", err)
			return nil
		}

		tcpSvc, udpSvc := lbex.getService(key)
		if len(udpSvc) == 0 && len(tcpSvc) == 0 {
			glog.V(4).Infof("syncServices: %s: not an lbex manage service", key)
			return nil
		}
		svcSpec := &nginx.ServiceSpec{
			Key:     key,
			Service: storeObj.(*v1.Service),
		}
		glog.V(3).Infof("syncServices: add/update service: %s,\n%v", key, svcSpec)
		lbex.cfgtor.AddOrUpdateService(filename, svcSpec)
	}
	return nil
}

func (lbex *lbExController) syncEndpoints(obj interface{}) error {
	if lbex.endpointsQueue.IsShuttingDown() {
		return nil
	}

	key, ok := obj.(string)
	if !ok {
		return errors.New("syncEndpoints: key string type assertion failed")
	}

	_, exists, err := lbex.endpointStore.GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		glog.V(2).Infof("syncEndpoints: deleting removed endpoint: %v\n", key)
		// TODO, need a service object here...
		// lbex.cfgtor.UpdateServiceEndpoints(key, <future thing>)
	} else {
		tcpSvc, udpSvc := lbex.getService(key)
		if len(udpSvc) == 0 && len(tcpSvc) == 0 {
			glog.V(4).Info("syncEndpoints: not a lbex managed service endpoint")
		} else {
			glog.V(3).Infof("syncEndpoints: add/update lbex managed service: %s, with endpoints:\nTCP Services: %v\nUDP Services: %v", key, tcpSvc, udpSvc)
		}
	}
	return nil
}

// getServiceEndpoints returns the endpoints v1 api object for the specified service name / namesapce.
func (lbex *lbExController) getServiceEndpoints(service *v1.Service) (endpoints v1.Endpoints, err error) {
	for _, ep := range lbex.endpointStore.List() {
		endpoints = *ep.(*v1.Endpoints)
		if service.Name == endpoints.Name && service.Namespace == endpoints.Namespace {
			return endpoints, nil
		}
	}
	err = fmt.Errorf("could not find endpoints for service: %v", service.Name)
	return
}

// getEndpoints returns a list of <endpoint ip>:<port> for a given service/target port combination.
func (lbex *lbExController) getEndpoints(service *v1.Service, servicePort *v1.ServicePort) (endpoints []string) {
	svcEndpoints, err := lbex.getServiceEndpoints(service)
	if err != nil {
		return
	}

	// The intent here is to create a union of all subsets that match a targetPort.
	// We know the endpoint already matches the service, so all pod ips that have
	// the target port are capable of service traffic for it.
	for _, subsets := range svcEndpoints.Subsets {

		for _, epPort := range subsets.Ports {

			var targetPort int
			switch servicePort.TargetPort.Type {
			case intstr.Int:
				servicePortInt, err := GetServicePortTargetPortInt(servicePort)
				if err != nil {
					continue
				}
				if epPort.Port == int32(servicePortInt) {
					targetPort = servicePortInt
				}
			case intstr.String:
				if epPort.Name == servicePort.TargetPort.StrVal {
					targetPort = int(epPort.Port)
				}
			}
			if targetPort == 0 {
				continue
			}

			for _, epAddress := range subsets.Addresses {
				endpoints = append(endpoints, fmt.Sprintf("%v:%v", epAddress.IP, targetPort))
			}
		}
	}
	return
}

// getServices returns a list of TCP and UDP services
func (lbex *lbExController) getServices() (tcpServices []Service, udpServices []Service) {
	objects := lbex.servicesStore.List()
	for _, obj := range objects {
		if !ValidateServiceObject(obj) {
			glog.V(4).Info("getServices: ValidateServiceObject(): false")
			continue
		}
		namespace, err := GetServiceNamespace(obj)
		if err != nil {
			continue
		}
		serviceName, err := GetServiceName(obj)
		if err != nil {
			continue
		}
		svcTCPServices, svcUDPServices := lbex.getService(namespace + "/" + serviceName)
		glog.V(4).Info("getServices: : false")

		tcpServices = append(tcpServices, svcTCPServices...)
		udpServices = append(udpServices, svcUDPServices...)
	}

	sort.Sort(serviceByName(tcpServices))
	sort.Sort(serviceByName(udpServices))

	return
}

// getService returns a services and it's endpoints.
func (lbex *lbExController) getService(key string) (tcpServices []Service, udpServices []Service) {
	obj, exists, err := lbex.servicesStore.GetByKey(key)
	if err != nil || !exists {
		return nil, nil
	}

	if !ValidateServiceObject(obj) {
		// Normal case for non-LB services (e.g. other service type or no annotation)
		glog.V(4).Infof("getService: can't validate service object key: %s", key)
		return nil, nil
	}

	serviceName, _ := GetServiceName(obj)
	if lbex.service != "" && lbex.service != serviceName {
		glog.V(3).Infof("getService: ignoring non-matching service name: %s", serviceName)
		return nil, nil
	}

	service, _ := obj.(*v1.Service)

	ep := []string{}
	for _, servicePort := range service.Spec.Ports {

		ep = lbex.getEndpoints(service, &servicePort)
		if len(ep) == 0 {
			glog.V(3).Infof("getService: no endpoints found for service %s, port %+d", service.Name, servicePort)
			continue
		}
		backendPort, _ := GetServicePortTargetPortInt(&servicePort)
		newSvc := Service{
			Name:        GetServiceNameForLBRule(serviceName, int(servicePort.Port)),
			Ep:          ep,
			BackendPort: backendPort,
		}

		if val, ok := annotations.GetHost(service); ok {
			newSvc.Host = val
		}

		if val, ok := annotations.GetAlgorithm(service); ok {
			for _, current := range nginx.SupportedAlgorithms {
				if val == current {
					newSvc.Algorithm = val
					break
				}
			}
		} else {
			newSvc.Algorithm = nginx.DefaultAlgorithm
		}
		newSvc.FrontendPort = int(servicePort.Port)

		if servicePort.Protocol == v1.ProtocolUDP {
			udpServices = append(udpServices, newSvc)
		} else {
			tcpServices = append(tcpServices, newSvc)
		}

		glog.V(3).Infof("getService: lbex supported service: %+v", newSvc)
	}

	sort.Sort(serviceByName(tcpServices))
	sort.Sort(serviceByName(udpServices))

	return
}
