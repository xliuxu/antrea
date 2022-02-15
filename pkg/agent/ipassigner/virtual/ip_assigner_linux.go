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
	"fmt"
	"net"
	"sync"

	"antrea.io/antrea/pkg/agent/util"
	"antrea.io/antrea/pkg/util/ip"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	solicitedNodeMulticastAddressPrefix = "ff02::1:ff00:0"
)

// ipAssigner manages external IPs on the Node. It will create raw sockets and listen for ARP requests packets (IPv4)
// and Neighbor Discovery packets (IPv6) directly without assigning any IP to any interfaces. This avoids the kernel
// creating local routes for the external IP, which have a higher priority than the routes installed by antrea-proxy
// in proxyAll mode.
type ipAssigner struct {
	arpResponder    *ARPResponder
	ndpResponder    *NDPResponder
	respondersLock  sync.Mutex
	assignedIPs     sync.Map
	ipChan          chan net.IP
	stopChan        chan struct{}
	multicastGroups map[int]int
}

// NewIPAssigner returns an *ipAssigner.
func NewIPAssigner(nodeTransportIPAddr net.IP) (*ipAssigner, error) {
	assigner := &ipAssigner{
		multicastGroups: make(map[int]int),
		ipChan:          make(chan net.IP),
		stopChan:        make(chan struct{}),
	}

	nodeTransportIPs := new(ip.DualStackIPs)
	if nodeTransportIPAddr.To4() == nil {
		nodeTransportIPs.IPv6 = nodeTransportIPAddr
	} else {
		nodeTransportIPs.IPv4 = nodeTransportIPAddr
	}

	_, _, externalInterface, err := util.GetIPNetDeviceFromIP(nodeTransportIPs)
	if err != nil {
		return nil, fmt.Errorf("get IPNetDevice from ip %v error: %+v", nodeTransportIPAddr, err)
	}

	arpResponder, err := NewARPResponder(externalInterface, assigner.hasIP)
	if err != nil {
		return nil, fmt.Errorf("failed to create ARP responder for link %s: %v", externalInterface.Name, err)
	}
	klog.InfoS("Created ARP responder for link", "index", externalInterface.Index, "deviceName", externalInterface.Name)
	assigner.arpResponder = arpResponder

	assignNdpResponder := false
	addrs, err := externalInterface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("failed to list IP addresses for link %s: %v", externalInterface.Name, err)
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if ipNet.IP.To4() == nil && ipNet.IP.To16() != nil && ipNet.IP.IsLinkLocalUnicast() {
			assignNdpResponder = true
			break
		}
	}
	if !assignNdpResponder {
		return assigner, nil
	}
	ndpResponder, err := NewNDPResponder(externalInterface, assigner.hasIP)
	if err != nil {
		return nil, fmt.Errorf("failed to create NDP responder for link %s: %v", externalInterface.Name, err)
	}
	klog.InfoS("Created NDP responder for link", "index", externalInterface.Index, "deviceName", externalInterface.Name)
	assigner.ndpResponder = ndpResponder

	return assigner, nil
}

func (a *ipAssigner) hasIP(ip net.IP) bool {
	_, exist := a.assignedIPs.Load(ip.String())
	return exist
}

func (a *ipAssigner) Run(stopCh <-chan struct{}) {
	go a.gratuitous()
	go a.arpResponder.Run(stopCh)
	go a.ndpResponder.Run(stopCh)
	<-stopCh
	close(a.ipChan)
	close(a.stopChan)
}

func (a *ipAssigner) gratuitous() {
	for ip := range a.ipChan {
		func() {
			a.respondersLock.Lock()
			defer a.respondersLock.Unlock()
			if utilnet.IsIPv4(ip) {
				if err := a.arpResponder.GratuitousARP(ip); err != nil {
					klog.ErrorS(err, "Failed to send Gratuitous ARP", "ip", ip)
				} else {
					klog.V(4).InfoS("Sent Gratuitous ARP", "ip", ip, "interface", a.arpResponder.InterfaceName())
				}
			} else {
				if err := a.ndpResponder.NeighborAdvertisement(ip); err != nil {
					klog.ErrorS(err, "Failed to send Neighbor Advertisement", "ip", ip)
				} else {
					klog.V(4).InfoS("Sent NDP Neighbor Advertisement", "ip", ip, "interface", a.ndpResponder.InterfaceName())
				}
			}
		}()
	}
}

func parseIPv6SolicitedNodeMulticastAddress(ip net.IP) (net.IP, int) {
	if !utilnet.IsIPv6(ip) {
		return nil, 0
	}
	group := net.ParseIP(solicitedNodeMulticastAddressPrefix)
	// copy lower 24 bits
	copy(group[13:], ip[13:])
	key := int(group[13])<<16 | int(group[14])<<8 | int(group[15])
	return group, key
}

func keyToSolicitedNodeMulticastAddress(key int) net.IP {
	group := net.ParseIP(solicitedNodeMulticastAddressPrefix)
	group[13] = byte((key >> 16) & 0xFF)
	group[14] = byte((key >> 8) & 0xFF)
	group[15] = byte(key & 0xFF)
	return group
}

// AssignIP ensures the provided IP is assigned to ARP/NDP responders.
func (a *ipAssigner) AssignIP(ip string) error {
	klog.InfoS("assigned IP", "ip", ip)
	parsed := net.ParseIP(ip)

	if parsed == nil {
		return fmt.Errorf("invalid IP format: %q", ip)
	}
	if _, ok := a.assignedIPs.Load(parsed.String()); ok {
		klog.V(2).InfoS("The IP is already assigned", "ip", ip)
		return nil
	}
	if utilnet.IsIPv6(parsed) {
		if err := func() error {
			a.respondersLock.Lock()
			defer a.respondersLock.Unlock()
			group, key := parseIPv6SolicitedNodeMulticastAddress(parsed)
			if a.multicastGroups[key] == 0 {
				if err := a.ndpResponder.JoinMulticastGroup(group); err != nil {
					return fmt.Errorf("join solicited-node multicast group %s for %q failed: %v", group, ip, err)
				}
			}
			a.multicastGroups[key]++
			return nil
		}(); err != nil {
			return err
		}
	}
	a.ipChan <- parsed

	a.assignedIPs.Store(parsed.String(), struct{}{})
	return nil
}

// UnassignIP ensures the provided IP is not assigned to ARP/NDP responders.
func (a *ipAssigner) UnassignIP(ip string) error {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return fmt.Errorf("invalid IP format: %q", ip)
	}
	if utilnet.IsIPv6(parsed) {
		if err := func() error {
			a.respondersLock.Lock()
			defer a.respondersLock.Unlock()
			group, key := parseIPv6SolicitedNodeMulticastAddress(parsed)
			if count, ok := a.multicastGroups[key]; ok {
				count--
				if count == 0 {
					if err := a.ndpResponder.LeaveMulticastGroup(group); err != nil {
						return fmt.Errorf("leave solicited-node multicast group %s for %q failed: %v", group, ip, err)
					}
					delete(a.multicastGroups, key)
				} else {
					a.multicastGroups[key] = count
				}
			}
			return nil
		}(); err != nil {
			return err
		}
	}
	a.assignedIPs.Delete(ip)
	return nil
}

// AssignedIPs return the IPs that are assigned to ARP/NDP responders.
func (a *ipAssigner) AssignedIPs() sets.String {
	res := sets.NewString()
	a.assignedIPs.Range(func(key interface{}, _ interface{}) bool {
		ip, ok := key.(string)
		if !ok {
			return true
		}
		res.Insert(ip)
		return true
	})
	return res
}
