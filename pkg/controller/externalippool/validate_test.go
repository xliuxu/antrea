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
	"encoding/json"
	"errors"
	"net"
	"testing"

	antreacrds "antrea.io/antrea/pkg/apis/crd/v1alpha2"
	corev1a2 "antrea.io/antrea/pkg/apis/crd/v1alpha2"
	"antrea.io/antrea/pkg/client/clientset/versioned"
	fakeversioned "antrea.io/antrea/pkg/client/clientset/versioned/fake"
	crdinformers "antrea.io/antrea/pkg/client/informers/externalversions"
	"antrea.io/antrea/pkg/controller/externalippool/ipallocator"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func marshal(object runtime.Object) []byte {
	raw, _ := json.Marshal(object)
	return raw
}

func newExternalIPPool(name, cidr, start, end string) *antreacrds.ExternalIPPool {
	pool := &antreacrds.ExternalIPPool{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	if len(cidr) > 0 {
		pool.Spec.IPRanges = append(pool.Spec.IPRanges, corev1a2.IPRange{CIDR: cidr})
	}
	if len(start) > 0 && len(end) > 0 {
		pool.Spec.IPRanges = append(pool.Spec.IPRanges, corev1a2.IPRange{Start: start, End: end})
	}
	return pool
}

type controller struct {
	controller         *ExternalIPPoolController
	client             kubernetes.Interface
	crdClient          versioned.Interface
	informerFactory    informers.SharedInformerFactory
	crdInformerFactory crdinformers.SharedInformerFactory
}

// objects is an initial set of K8s objects that is exposed through the client.
func newController(objects, crdObjects []runtime.Object) *controller {
	client := fake.NewSimpleClientset(objects...)
	crdClient := fakeversioned.NewSimpleClientset(crdObjects...)
	informerFactory := informers.NewSharedInformerFactory(client, resyncPeriod)
	crdInformerFactory := crdinformers.NewSharedInformerFactory(crdClient, resyncPeriod)
	externalIPPoolController, _ := NewExternalIPPoolController(crdClient, crdInformerFactory.Crd().V1alpha2().ExternalIPPools())
	return &controller{
		externalIPPoolController,
		client,
		crdClient,
		informerFactory,
		crdInformerFactory,
	}
}

func TestControllerValidateExternalIPPool(t *testing.T) {
	tests := []struct {
		name             string
		request          *admv1.AdmissionRequest
		expectedResponse *admv1.AdmissionResponse
	}{
		{
			name: "CREATE operation should be allowed",
			request: &admv1.AdmissionRequest{
				Name:      "foo",
				Operation: "CREATE",
				Object:    runtime.RawExtension{Raw: marshal(newExternalIPPool("foo", "10.10.10.0/24", "", ""))},
			},
			expectedResponse: &admv1.AdmissionResponse{Allowed: true},
		},
		{
			name: "Deleting IPRange should not be allowed",
			request: &admv1.AdmissionRequest{
				Name:      "foo",
				Operation: "UPDATE",
				OldObject: runtime.RawExtension{Raw: marshal(newExternalIPPool("foo", "10.10.10.0/24", "10.10.20.1", "10.10.20.2"))},
				Object:    runtime.RawExtension{Raw: marshal(newExternalIPPool("foo", "10.10.10.0/24", "", ""))},
			},
			expectedResponse: &admv1.AdmissionResponse{
				Allowed: false,
				Result: &metav1.Status{
					Message: "existing IPRanges [10.10.20.1-10.10.20.2] cannot be deleted",
				},
			},
		},
		{
			name: "Adding IPRange should be allowed",
			request: &admv1.AdmissionRequest{
				Name:      "foo",
				Operation: "UPDATE",
				OldObject: runtime.RawExtension{Raw: marshal(newExternalIPPool("foo", "10.10.10.0/24", "", ""))},
				Object:    runtime.RawExtension{Raw: marshal(newExternalIPPool("foo", "10.10.10.0/24", "10.10.20.1", "10.10.20.2"))},
			},
			expectedResponse: &admv1.AdmissionResponse{Allowed: true},
		},
		{
			name: "DELETE operation should be allowed",
			request: &admv1.AdmissionRequest{
				Name:      "foo",
				Operation: "DELETE",
				Object:    runtime.RawExtension{Raw: marshal(newExternalIPPool("foo", "10.10.10.0/24", "", ""))},
			},
			expectedResponse: &admv1.AdmissionResponse{Allowed: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newController(nil, nil)
			stopCh := make(chan struct{})
			defer close(stopCh)
			c.informerFactory.Start(stopCh)
			c.crdInformerFactory.Start(stopCh)
			c.informerFactory.WaitForCacheSync(stopCh)
			c.crdInformerFactory.WaitForCacheSync(stopCh)
			go c.controller.Run(stopCh)
			review := &admv1.AdmissionReview{
				Request: tt.request,
			}
			gotResponse := c.controller.ValidateExternalIPPool(review)
			assert.Equal(t, tt.expectedResponse, gotResponse)
		})
	}
}

type fakeExternalIPAllocator struct {
	allocators map[string]ipallocator.IPAllocator
}

var _ ExternalIPAllocator = (*fakeExternalIPAllocator)(nil)

func (f *fakeExternalIPAllocator) HasSynced() bool {
	return true
}

func (f *fakeExternalIPAllocator) AddConsumer(func(ippool string)) {

}
func (f *fakeExternalIPAllocator) LocateIP(ip net.IP) (string, error) {
	return "", nil
}

func (f *fakeExternalIPAllocator) ConsumerRestoreFinished() {
}

func (f *fakeExternalIPAllocator) AllocateIPFromPool(pool string) (net.IP, error) {
	allocator, exists := f.allocators[pool]
	if !exists {
		return nil, ErrExternalIPPoolNotFound
	}
	return allocator.AllocateNext()
}

func (f *fakeExternalIPAllocator) AllocateIP() (net.IP, string, error) {
	for p, a := range f.allocators {
		ip, err := a.AllocateNext()
		if err == nil {
			return ip, p, nil
		}
	}
	return nil, "", errors.New("No IP pool available")
}

func (f *fakeExternalIPAllocator) IPPoolExists(pool string) bool {
	_, exists := f.allocators[pool]
	return exists
}

func (f *fakeExternalIPAllocator) IPPoolHasIP(pool string, ip net.IP) bool {
	allocator, exists := f.allocators[pool]
	if !exists {
		return false
	}
	return allocator.Has(ip)
}

func (f *fakeExternalIPAllocator) UpdateIPAllocation(ip net.IP, pool string) error {
	allocator, exists := f.allocators[pool]
	if !exists {
		return ErrExternalIPPoolNotFound
	}
	return allocator.AllocateIP(ip)
}

func (f *fakeExternalIPAllocator) ReleaseIP(ip net.IP, pool string) {
	allocator, exists := f.allocators[pool]
	if !exists {
		return
	}
	allocator.Release(ip)
}

func NewFakeExternalIPAllocator(t *testing.T, pools ...*antreacrds.ExternalIPPool) ExternalIPAllocator {
	f := fakeExternalIPAllocator{
		allocators: make(map[string]ipallocator.IPAllocator),
	}
	for _, p := range pools {
		for _, r := range p.Spec.IPRanges {
			allocator, err := ipallocator.NewCIDRAllocator(r.CIDR)
			require.NoError(t, err)
			f.allocators[p.Name] = allocator

		}
	}
	return nil
}
