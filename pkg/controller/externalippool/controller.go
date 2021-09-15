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

package externalippool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	antreacrds "antrea.io/antrea/pkg/apis/crd/v1alpha2"
	clientset "antrea.io/antrea/pkg/client/clientset/versioned"
	antreainformers "antrea.io/antrea/pkg/client/informers/externalversions/crd/v1alpha2"
	antrealisters "antrea.io/antrea/pkg/client/listers/crd/v1alpha2"
	"antrea.io/antrea/pkg/controller/externalippool/ipallocator"
	"antrea.io/antrea/pkg/controller/metrics"
)

const (
	controllerName = "ExternalIPPoolController"
	// Set resyncPeriod to 0 to disable resyncing.
	resyncPeriod time.Duration = 0
	// How long to wait before retrying the processing of an ExternalIPPool change.
	minRetryDelay = 5 * time.Second
	maxRetryDelay = 300 * time.Second
	// Default number of workers processing an ExternalIPPool change.
	defaultWorkers = 4
)

var (
	ErrExternalIPPoolNotFound = errors.New("ExternalIPPool not found")
)

type ExternalIPAllocator interface {
	AddConsumer(func(string))
	ConsumerRestoreFinished()
	AllocateIPFromPool(string) (net.IP, error)
	AllocateIP() (net.IP, string, error)
	IPPoolExists(string) bool
	IPPoolHasIP(string, net.IP) bool
	LocateIP(net.IP) (string, error)
	UpdateIPAllocation(net.IP, string) error
	ReleaseIP(net.IP, string)
	HasSynced() bool
}

var _ ExternalIPAllocator = (*ExternalIPPoolController)(nil)

// ExternalIPPoolController is responsible for synchronizing the ExternalIPPool resources.
type ExternalIPPoolController struct {
	crdClient                  clientset.Interface
	externalIPPoolLister       antrealisters.ExternalIPPoolLister
	externalIPPoolListerSynced cache.InformerSynced

	// ipAllocatorMap is a map from ExternalIPPool name to MultiIPAllocator.
	ipAllocatorMap   map[string]ipallocator.MultiIPAllocator
	ipAllocatorMutex sync.RWMutex

	// ipAllocatorInitFinished stores a boolean value, which tracks if the ipAllocatorMap has been initialized
	// with the full lists of ExternalIPPool.
	ipAllocatorInitFinished *atomic.Value

	// consumers is an array of consumers will be notified when ExternalIPPool updates.
	consumers          []func(string)
	consumersWaitGroup sync.WaitGroup

	// queue maintains the ExternalIPPool objects that need to be synced.
	queue workqueue.RateLimitingInterface
}

// NewExternalIPPoolController returns a new *ExternalIPPoolController.
func NewExternalIPPoolController(crdClient clientset.Interface, externalIPPoolInformer antreainformers.ExternalIPPoolInformer) (*ExternalIPPoolController, error) {
	c := &ExternalIPPoolController{
		crdClient:                  crdClient,
		externalIPPoolLister:       externalIPPoolInformer.Lister(),
		externalIPPoolListerSynced: externalIPPoolInformer.Informer().HasSynced,
		queue:                      workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "externalIPPool"),
		ipAllocatorInitFinished:    &atomic.Value{},
		ipAllocatorMap:             make(map[string]ipallocator.MultiIPAllocator),
	}
	externalIPPoolInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.addExternalIPPool,
			UpdateFunc: c.updateExternalIPPool,
			DeleteFunc: c.deleteExternalIPPool,
		},
		resyncPeriod,
	)
	c.ipAllocatorInitFinished.Store(false)
	return c, nil
}

func (c *ExternalIPPoolController) HasSynced() bool {
	return c.ipAllocatorInitFinished.Load().(bool)
}

func (c *ExternalIPPoolController) AddConsumer(consumer func(ippool string)) {
	c.consumers = append(c.consumers, consumer)
	c.consumersWaitGroup.Add(1)
}

func (c *ExternalIPPoolController) ConsumerRestoreFinished() {
	c.consumersWaitGroup.Done()
}

// Run begins watching and syncing of the ExternalIPPoolController .
func (c *ExternalIPPoolController) Run(stopCh <-chan struct{}) {
	defer c.queue.ShutDown()

	klog.Infof("Starting %s", controllerName)
	defer klog.Infof("Shutting down %s", controllerName)

	cacheSyncs := []cache.InformerSynced{c.externalIPPoolListerSynced}
	if !cache.WaitForNamedCacheSync(controllerName, stopCh, cacheSyncs...) {
		return
	}

	// Initialize the ipAllocatorMap with the existing ExternalIPPools.
	ipPools, _ := c.externalIPPoolLister.List(labels.Everything())
	for _, ipPool := range ipPools {
		c.createOrUpdateIPAllocator(ipPool)
	}

	c.ipAllocatorInitFinished.Store(true)

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(c.worker, time.Second, stopCh)
	}
	<-stopCh
}

// createOrUpdateIPAllocator creates or updates the IP allocator based on the provided ExternalIPPool.
// Currently it's assumed that only new ranges will be added and existing ranges should not be deleted.
// TODO: Use validation webhook to ensure it.
func (c *ExternalIPPoolController) createOrUpdateIPAllocator(ipPool *antreacrds.ExternalIPPool) bool {
	changed := false
	c.ipAllocatorMutex.Lock()
	defer c.ipAllocatorMutex.Unlock()

	existingIPRanges := sets.NewString()
	multiIPAllocator, exists := c.ipAllocatorMap[ipPool.Name]
	if !exists {
		multiIPAllocator = ipallocator.MultiIPAllocator{}
		changed = true
	} else {
		existingIPRanges.Insert(multiIPAllocator.Names()...)
	}

	for _, ipRange := range ipPool.Spec.IPRanges {
		ipRangeStr := ipRange.CIDR
		if ipRangeStr == "" {
			ipRangeStr = fmt.Sprintf("%s-%s", ipRange.Start, ipRange.End)
		}
		// The ipRange is already in the allocator.
		if existingIPRanges.Has(ipRangeStr) {
			continue
		}
		var ipAllocator *ipallocator.SingleIPAllocator
		var err error
		if ipRange.CIDR != "" {
			ipAllocator, err = ipallocator.NewCIDRAllocator(ipRange.CIDR)
		} else {
			ipAllocator, err = ipallocator.NewIPRangeAllocator(ipRange.Start, ipRange.End)
		}
		if err != nil {
			klog.ErrorS(err, "Failed to create IPAllocator", "ipRange", ipRange)
			continue
		}
		multiIPAllocator = append(multiIPAllocator, ipAllocator)
		changed = true
	}
	c.ipAllocatorMap[ipPool.Name] = multiIPAllocator
	c.queue.Add(ipPool.Name)
	return changed
}

// deleteIPAllocator deletes the IP allocator of the given IP pool.
func (c *ExternalIPPoolController) deleteIPAllocator(ipPoolName string) {
	c.ipAllocatorMutex.Lock()
	defer c.ipAllocatorMutex.Unlock()
	delete(c.ipAllocatorMap, ipPoolName)
}

// getIPAllocator gets the IP allocator of the given IP pool.
func (c *ExternalIPPoolController) getIPAllocator(ipPoolName string) (ipallocator.MultiIPAllocator, bool) {
	c.ipAllocatorMutex.RLock()
	defer c.ipAllocatorMutex.RUnlock()
	ipAllocator, exists := c.ipAllocatorMap[ipPoolName]
	return ipAllocator, exists
}

// AllocateIPFromPool allocate an IP from the the given IP pool.
func (c *ExternalIPPoolController) AllocateIPFromPool(ipPoolName string) (net.IP, error) {
	c.consumersWaitGroup.Wait()
	ipAllocator, exists := c.getIPAllocator(ipPoolName)
	if !exists {
		return nil, ErrExternalIPPoolNotFound
	}
	ip, err := ipAllocator.AllocateNext()
	if err == nil {
		c.queue.Add(ipPoolName)
	}
	return ip, err
}

// AllocateIP allocate an IP from an available IP pool.
func (c *ExternalIPPoolController) AllocateIP() (net.IP, string, error) {
	c.consumersWaitGroup.Wait()
	c.ipAllocatorMutex.RLock()
	defer c.ipAllocatorMutex.RUnlock()
	for pool, allocator := range c.ipAllocatorMap {
		ip, err := allocator.AllocateNext()
		if err == nil {
			c.queue.Add(pool)
			return ip, pool, nil
		}
	}
	return nil, "", fmt.Errorf("no IP pool available")
}

// UpdateIPAllocation sets the IP in the specified ExternalIPPool.
func (c *ExternalIPPoolController) UpdateIPAllocation(ip net.IP, pool string) error {
	ipAllocator, exists := c.getIPAllocator(pool)
	if !exists {
		return ErrExternalIPPoolNotFound
	}
	return ipAllocator.AllocateIP(ip)
}

func (c *ExternalIPPoolController) updateExternalIPPoolStatus(poolName string) error {
	eip, err := c.externalIPPoolLister.Get(poolName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	ipAllocator, exists := c.getIPAllocator(eip.Name)
	if !exists {
		return ErrExternalIPPoolNotFound
	}
	total, used := ipAllocator.Total(), ipAllocator.Used()
	klog.Infof("Upgrade status for pool %s, total: %d, used %d", poolName, total, used)
	toUpdate := eip.DeepCopy()
	var getErr error
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		actualStatus := eip.Status
		usage := antreacrds.ExternalIPPoolUsage{Total: total, Used: used}
		if actualStatus.Usage == usage {
			return nil
		}
		klog.V(2).InfoS("Updating ExternalIPPool status", "ExternalIPPool", poolName, "usage", usage)
		toUpdate.Status.Usage = usage
		if _, updateErr := c.crdClient.CrdV1alpha2().ExternalIPPools().UpdateStatus(context.TODO(), toUpdate, metav1.UpdateOptions{}); updateErr != nil && apierrors.IsConflict(updateErr) {
			toUpdate, getErr = c.crdClient.CrdV1alpha2().ExternalIPPools().Get(context.TODO(), poolName, metav1.GetOptions{})
			if getErr != nil {
				return getErr
			}
			return updateErr
		}
		return nil
	}); err != nil {
		return fmt.Errorf("updating ExternalIPPool %s status error: %v", poolName, err)
	}
	klog.V(2).InfoS("Updated ExternalIPPool status", "ExternalIPPool", poolName)
	metrics.AntreaExternalIPPoolStatusUpdates.Inc()
	return nil
}

// ReleaseIP releases the IP to the pool.
func (c *ExternalIPPoolController) ReleaseIP(ip net.IP, poolName string) {
	allocator, exists := c.getIPAllocator(poolName)
	if !exists {
		klog.ErrorS(ErrExternalIPPoolNotFound, "Failed to release IP", "ip", ip, "pool", poolName)
		return
	}
	if err := allocator.Release(ip); err != nil {
		klog.ErrorS(err, "Failed to release IP", "ip", ip, "pool", poolName)
		return
	}
	c.queue.Add(poolName)
	klog.InfoS("Released IP", "ip", ip, "pool", poolName)
}

func (c *ExternalIPPoolController) IPPoolHasIP(pool string, ip net.IP) bool {
	allocator, exists := c.getIPAllocator(pool)
	if !exists {
		return false
	}
	return allocator.Has(ip)
}

func (c *ExternalIPPoolController) IPPoolExists(pool string) bool {
	_, exists := c.getIPAllocator(pool)
	return exists
}

func (c *ExternalIPPoolController) LocateIP(ip net.IP) (string, error) {
	c.ipAllocatorMutex.RLock()
	defer c.ipAllocatorMutex.RUnlock()
	for pool, allocator := range c.ipAllocatorMap {
		if allocator.Has(ip) {
			return pool, nil
		}
	}
	return "", ErrExternalIPPoolNotFound
}

func (c *ExternalIPPoolController) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *ExternalIPPoolController) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.updateExternalIPPoolStatus(key.(string))
	if err != nil {
		// Put the item back in the workqueue to handle any transient errors.
		c.queue.AddRateLimited(key)
		klog.ErrorS(err, "Failed to sync ExternalIPPool status", "ExternalIPPool", key)
		return true
	}
	// If no error occurs we Forget this item so it does not get queued again until
	// another change happens.
	c.queue.Forget(key)
	return true
}

// addExternalIPPool processes ExternalIPPool ADD events. It creates an IPAllocator for the pool and triggers
// reconciliation of consumers that refer to the pool.
func (c *ExternalIPPoolController) addExternalIPPool(obj interface{}) {
	pool := obj.(*antreacrds.ExternalIPPool)
	klog.InfoS("Processing ExternalIPPool ADD event", "pool", pool.Name, "ipRanges", pool.Spec.IPRanges)
	c.createOrUpdateIPAllocator(pool)
	for _, c := range c.consumers {
		c(pool.Name)
	}
}

// updateExternalIPPool processes ExternalIPPool UPDATE events. It updates the IPAllocator for the pool and triggers
// reconciliation of consumers that refer to the pool if the IPAllocator changes.
func (c *ExternalIPPoolController) updateExternalIPPool(_, cur interface{}) {
	pool := cur.(*antreacrds.ExternalIPPool)
	klog.InfoS("Processing ExternalIPPool UPDATE event", "pool", pool.Name, "ipRanges", pool.Spec.IPRanges)
	if c.createOrUpdateIPAllocator(pool) {
		for _, c := range c.consumers {
			c(pool.Name)
		}
	}
}

// deleteExternalIPPool processes ExternalIPPool DELETE events. It deletes the IPAllocator for the pool and triggers
// reconciliation of all consumers that refer to the pool.
func (c *ExternalIPPoolController) deleteExternalIPPool(obj interface{}) {
	pool := obj.(*antreacrds.ExternalIPPool)
	klog.InfoS("Processing ExternalIPPool DELETE event", "pool", pool.Name, "ipRanges", pool.Spec.IPRanges)
	c.deleteIPAllocator(pool.Name)
	// Call consumers to reclaim the IPs allocated from the pool.
	for _, c := range c.consumers {
		c(pool.Name)
	}
}
