// Copyright 2021 Antrea Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package loadbalancer

import (
	"fmt"
	"net"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"antrea.io/antrea/pkg/agent/floatingip"
	"antrea.io/antrea/pkg/agent/memberlist"
	"antrea.io/antrea/pkg/agent/types"
)

const (
	controllerName = "AntreaAgentLoadBalancerController"
	// How long to wait before retrying the processing of an Service change.
	minRetryDelay = 5 * time.Second
	maxRetryDelay = 300 * time.Second
	// Default number of workers processing an Service change.
	defaultWorkers = 4
	// Disable resyncing.
	resyncPeriod time.Duration = 0

	loadBalancerIPIndex = "loadBalancerIP"
	externalIPPoolIndex = "externalIPPool"

	// loadBalancerDummyDevice is the dummy device that holds the LoadBalancer IPs configured to the system by antrea-agent.
	loadBalancerDummyDevice = "antrea-lb0"
)

type LoadBalancerController struct {
	serviceInformer     cache.SharedIndexInformer
	serviceLister       corelisters.ServiceLister
	serviceListerSynced cache.InformerSynced

	endpointsInformer     cache.SharedIndexInformer
	endpointsLister       corelisters.EndpointsLister
	endpointsListerSynced cache.InformerSynced

	queue workqueue.RateLimitingInterface

	loadBalancerIPs      map[apimachinerytypes.NamespacedName]string
	loadBalancerIPsMutex sync.RWMutex

	cluster         *memberlist.Cluster
	ipAssigner      floatingip.IPAssigner
	localIPDetector floatingip.LocalIPDetector
}

func NewLoadBalancerController(
	nodeIP net.IP,
	cluster *memberlist.Cluster,
	serviceInformer coreinformers.ServiceInformer,
	endpointsInformer coreinformers.EndpointsInformer,
	localIPDetector floatingip.LocalIPDetector,
) (*LoadBalancerController, error) {
	c := &LoadBalancerController{
		cluster:               cluster,
		queue:                 workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "loadbalancer"),
		serviceInformer:       serviceInformer.Informer(),
		serviceLister:         serviceInformer.Lister(),
		serviceListerSynced:   serviceInformer.Informer().HasSynced,
		endpointsInformer:     endpointsInformer.Informer(),
		endpointsLister:       endpointsInformer.Lister(),
		endpointsListerSynced: endpointsInformer.Informer().HasSynced,
		loadBalancerIPs:       make(map[apimachinerytypes.NamespacedName]string),
		localIPDetector:       localIPDetector,
	}
	ipAssigner, err := floatingip.NewIPAssigner(nodeIP, loadBalancerDummyDevice)
	if err != nil {
		return nil, fmt.Errorf("initializing LoadBalancer IP assigner failed: %v", err)
	}
	c.ipAssigner = ipAssigner

	c.serviceInformer.AddIndexers(cache.Indexers{loadBalancerIPIndex: func(obj interface{}) ([]string, error) {
		service, ok := obj.(*corev1.Service)
		if !ok {
			return nil, fmt.Errorf("obj is not Service: %+v", obj)
		}
		if len(service.Status.LoadBalancer.Ingress) == 0 {
			return nil, nil
		}
		return []string{service.Status.LoadBalancer.Ingress[0].IP}, nil
	}})

	c.serviceInformer.AddIndexers(cache.Indexers{externalIPPoolIndex: func(obj interface{}) ([]string, error) {
		service, ok := obj.(*corev1.Service)
		if !ok {
			return nil, fmt.Errorf("obj is not Service: %+v", obj)
		}
		eipName, ok := service.Annotations[types.ServiceExternalIPPoolAnnotationKey]
		if !ok {
			return nil, nil
		}
		return []string{eipName}, nil
	}})

	c.serviceInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: c.enqueueService,
			UpdateFunc: func(old, cur interface{}) {
				c.enqueueService(cur)
			},
			DeleteFunc: c.enqueueService,
		},
		resyncPeriod,
	)

	c.endpointsInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: c.enqueueServiceForEndpoints,
			UpdateFunc: func(old, cur interface{}) {
				c.enqueueServiceForEndpoints(cur)
			},
			DeleteFunc: c.enqueueServiceForEndpoints,
		},
		resyncPeriod,
	)

	c.localIPDetector.AddEventHandler(c.onLocalIPUpdate)
	c.cluster.AddClusterEventHandler(c.enqueueServicesByExternalIPPool)
	return c, nil
}

func (c *LoadBalancerController) enqueueService(obj interface{}) {
	service, ok := obj.(*corev1.Service)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		service, ok = deletedState.Obj.(*corev1.Service)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Service object: %v", deletedState.Obj)
			return
		}
	}
	c.queue.Add(apimachinerytypes.NamespacedName{
		Namespace: service.Namespace,
		Name:      service.Name,
	})
}

func (c *LoadBalancerController) enqueueServiceForEndpoints(obj interface{}) {
	endpoints, ok := obj.(*corev1.Endpoints)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		endpoints, ok = deletedState.Obj.(*corev1.Endpoints)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Endpoint object: %v", deletedState.Obj)
			return
		}
	}
	service, err := c.serviceLister.Services(endpoints.Namespace).Get(endpoints.Name)
	if err != nil {
		klog.ErrorS(err, "failed to get Service for Endpoint", "namespace", endpoints.Namespace, "name", endpoints.Name)
		return
	}
	// we only care services with ServiceExternalTrafficPolicy setting to local
	if service.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyTypeLocal {
		return
	}
	c.queue.Add(apimachinerytypes.NamespacedName{
		Namespace: service.Namespace,
		Name:      service.Name,
	})
}

func (c *LoadBalancerController) onLocalIPUpdate(ip string, added bool) {
	services, _ := c.serviceInformer.GetIndexer().ByIndex(loadBalancerIPIndex, ip)
	if len(services) == 0 {
		return
	}
	if added {
		klog.Infof("Detected LoadBalancer IP address %s added to this Node", ip)
	} else {
		klog.Infof("Detected LoadBalancer IP address %s deleted from this Node", ip)
	}
	for _, s := range services {
		c.enqueueService(s)
	}
}

// enqueueServiceesByExternalIPPool enqueues all LoadBalancer type Services that refer to the provided ExternalIPPool,
// the ExternalIPPool is affected by a Node update/create/delete event or
// Node leaves/join cluster event or ExternalIPPool changed.
func (c *LoadBalancerController) enqueueServicesByExternalIPPool(eipName string) {
	objects, _ := c.serviceInformer.GetIndexer().ByIndex(externalIPPoolIndex, eipName)
	for _, object := range objects {
		service := object.(*corev1.Service)
		c.queue.Add(apimachinerytypes.NamespacedName{
			Namespace: service.Namespace,
			Name:      service.Name,
		})
	}
	klog.InfoS("Detected ExternalIPPool event", "ExternalIPPool", eipName, "enqueueServiceNum", len(objects))
}

// Run will create defaultWorkers workers (go routines) which will process the Service events from the
// workqueue.
func (c *LoadBalancerController) Run(stopCh <-chan struct{}) {
	defer c.queue.ShutDown()

	klog.Infof("Starting %s", controllerName)
	defer klog.Infof("Shutting down %s", controllerName)

	go c.localIPDetector.Run(stopCh)

	if !cache.WaitForNamedCacheSync(controllerName, stopCh, c.serviceListerSynced, c.endpointsListerSynced, c.localIPDetector.HasSynced) {
		return
	}

	c.removeStaleLoadBalancerIPs()

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(c.worker, time.Second, stopCh)
	}
	<-stopCh
}

// removeStaleLoadBalancerIPs unassigns stale LoadBalancer IPs that shouldn't be present on this Node.
// This function will only delete IPs which caused by Service changes when the agent on this Node was
// not running. Those IPs should be deleted caused by migration will be deleted by processNextWorkItem.
func (c *LoadBalancerController) removeStaleLoadBalancerIPs() {
	desiredLoadBalancerIPs := sets.NewString()
	services, _ := c.serviceLister.List(labels.Everything())
	for _, service := range services {
		if service.Spec.Type == corev1.ServiceTypeLoadBalancer &&
			service.ObjectMeta.Annotations[types.ServiceExternalIPPoolAnnotationKey] != "" &&
			len(service.Status.LoadBalancer.Ingress) != 0 {
			desiredLoadBalancerIPs.Insert(service.Status.LoadBalancer.Ingress[0].IP)
		}
	}
	actualLocalLoadBalancerIPs := c.ipAssigner.AssignedIPs()
	for ip := range actualLocalLoadBalancerIPs.Difference(desiredLoadBalancerIPs) {
		if err := c.ipAssigner.UnassignIP(ip); err != nil {
			klog.ErrorS(err, "Failed to clean up stale LoadBalancer IP", "ip", ip)
		}
	}
}

// worker is a long-running function that will continually call the processNextWorkItem function in
// order to read and process a message on the workqueue.
func (c *LoadBalancerController) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *LoadBalancerController) processNextWorkItem() bool {
	obj, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(obj)
	if key, ok := obj.(apimachinerytypes.NamespacedName); !ok {
		c.queue.Forget(obj)
		klog.Errorf("Expected NamespacedName in work queue but got %#v", obj)
		return true
	} else if err := c.syncService(key); err == nil {
		// If no error occurs we Forget this item so it does not get queued again until
		// another change happens.
		c.queue.Forget(key)
	} else {
		// Put the item back on the workqueue to handle any transient errors.
		c.queue.AddRateLimited(key)
		klog.Errorf("Error syncing Service %s, requeuing. Error: %v", key, err)
	}
	return true
}

func (c *LoadBalancerController) deleteLoadBalancer(service apimachinerytypes.NamespacedName) error {
	c.loadBalancerIPsMutex.Lock()
	defer c.loadBalancerIPsMutex.Unlock()
	if ip, exist := c.loadBalancerIPs[service]; !exist {
		return nil
	} else {
		delete(c.loadBalancerIPs, service)
		return c.ipAssigner.UnassignIP(ip)
	}
}

func (c *LoadBalancerController) getAssignedLoadBalancerIP(service apimachinerytypes.NamespacedName) (string, bool) {
	c.loadBalancerIPsMutex.RLock()
	defer c.loadBalancerIPsMutex.RUnlock()
	ip, exist := c.loadBalancerIPs[service]
	return ip, exist
}

func (c *LoadBalancerController) getServiceLoadBalancerIP(service *corev1.Service) string {
	if len(service.Status.LoadBalancer.Ingress) == 0 {
		return ""
	}
	return service.Status.LoadBalancer.Ingress[0].IP
}

func (c *LoadBalancerController) syncService(key apimachinerytypes.NamespacedName) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing Service for %s. (%v)", key, time.Since(startTime))
	}()

	service, err := c.serviceLister.Services(key.Namespace).Get(key.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			return c.deleteLoadBalancer(key)
		}
		return err
	}

	if service.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return c.deleteLoadBalancer(key)
	}

	ip, exist := c.getAssignedLoadBalancerIP(key)
	currentLoadBalancerIP := c.getServiceLoadBalancerIP(service)
	if exist && ip != currentLoadBalancerIP {
		if err := c.deleteLoadBalancer(key); err != nil {
			return err
		}
	}

	ipPoool := service.ObjectMeta.Annotations[types.ServiceExternalIPPoolAnnotationKey]

	if currentLoadBalancerIP == "" || ipPoool == "" {
		return nil
	}

	var filters []func(string) bool

	if service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyTypeLocal {
		nodes, err := c.nodesHasHealthyServiceEndpoint(service)
		if err != nil {
			return err
		}
		filters = append(filters, func(s string) bool {
			return nodes.Has(s)
		})
	}

	localNodeSelected, err := c.cluster.ShouldSelectIP(currentLoadBalancerIP, ipPoool, filters...)
	if err != nil {
		return err
	}
	klog.InfoS("ShouldSelectIP", "localNodeSelected", localNodeSelected, "currentLoadBalancerIP", currentLoadBalancerIP, "ipPoool", ipPoool)
	if localNodeSelected {
		c.loadBalancerIPsMutex.Lock()
		c.loadBalancerIPs[key] = currentLoadBalancerIP
		c.loadBalancerIPsMutex.Unlock()
		return c.ipAssigner.AssignIP(currentLoadBalancerIP)
	} else {
		return c.ipAssigner.UnassignIP(currentLoadBalancerIP)
	}
}

// nodesHasHealthyServiceEndpoint returns the set of Nodes which has at least one healthy endpoint.
func (c *LoadBalancerController) nodesHasHealthyServiceEndpoint(lbService *corev1.Service) (sets.String, error) {
	nodes := sets.NewString()
	endpoints, err := c.endpointsLister.Endpoints(lbService.Namespace).Get(lbService.Name)
	if err != nil {
		return nodes, err
	}
	for _, subset := range endpoints.Subsets {
		for _, ep := range subset.Addresses {
			if ep.NodeName == nil {
				continue
			}
			nodes.Insert(*ep.NodeName)
		}
		for _, ep := range subset.NotReadyAddresses {
			if ep.NodeName == nil {
				continue
			}
			nodes.Delete(*ep.NodeName)
		}
	}
	return nodes, nil
}
