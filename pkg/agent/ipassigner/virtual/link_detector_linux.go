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

package virtual

import (
	"time"

	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

type linkDetector struct {
	eventHandlers []LinkEventHandler
}

func NewLinkDetector() *linkDetector {
	return &linkDetector{}
}

func (d *linkDetector) AddEventHandler(handler LinkEventHandler) {
	d.eventHandlers = append(d.eventHandlers, handler)
}

func (d *linkDetector) Run(stopCh <-chan struct{}) {
	klog.Infof("Starting linkDetector")

	go wait.NonSlidingUntil(func() {
		d.listAndWatchLinks(stopCh)
	}, 5*time.Second, stopCh)

	<-stopCh
}

func (d *linkDetector) notify(linkIndex int) {
	for _, handler := range d.eventHandlers {
		handler(linkIndex)
	}
}

func (d *linkDetector) listAndWatchLinks(stopCh <-chan struct{}) {
	ch := make(chan netlink.LinkUpdate)
	if err := netlink.LinkSubscribeWithOptions(ch, stopCh, netlink.LinkSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Errorf("Received error from link update subscription: %v", err)
		},
	}); err != nil {
		klog.Errorf("Failed to subscribe link update: %v", err)
		return
	}

	links, err := netlink.LinkList()
	if err != nil {
		klog.Errorf("Failed to list links on the Node")
		return
	}

	for _, link := range links {
		d.notify(link.Attrs().Index)
	}

	for {
		select {
		case <-stopCh:
			return
		case linkUpdate, ok := <-ch:
			if !ok {
				klog.Warning("Link update channel was closed")
				return
			}
			klog.V(4).Infof("Received Link update: %+v", linkUpdate)
			for _, hander := range d.eventHandlers {
				hander(linkUpdate.Attrs().Index)
			}
		}
	}
}
