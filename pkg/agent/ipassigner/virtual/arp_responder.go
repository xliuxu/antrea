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

	"github.com/mdlayher/arp"
	"github.com/mdlayher/ethernet"
	"k8s.io/klog/v2"
)

type AssignedIPChecker func(ip net.IP) bool

type ARPResponder struct {
	iface        *net.Interface
	conn         *arp.Client
	isIPAssigned AssignedIPChecker
	closed       chan struct{}
}

func NewARPResponder(iface *net.Interface, isIPAssigned AssignedIPChecker) (*ARPResponder, error) {
	conn, err := arp.Dial(iface)
	if err != nil {
		return nil, fmt.Errorf("creating ARP responder for %q: %s", iface.Name, err)
	}
	return &ARPResponder{
		iface:        iface,
		conn:         conn,
		isIPAssigned: isIPAssigned,
		closed:       make(chan struct{}),
	}, nil
}

// GratuitousARP sends an gratuitous ARP packet for the IP.
func (r *ARPResponder) GratuitousARP(ip net.IP) error {
	if ip.To4() == nil {
		return fmt.Errorf("only IPv4 is supported")
	}
	pkt, err := arp.NewPacket(arp.OperationRequest, r.iface.HardwareAddr, ip, ethernet.Broadcast, ip)
	if err != nil {
		return err
	}
	return r.conn.WriteTo(pkt, ethernet.Broadcast)
}

func (r *ARPResponder) InterfaceName() string {
	return r.iface.Name
}

func (r *ARPResponder) HandleARPRequest() error {
	pkt, _, err := r.conn.Read()
	if err != nil {
		return err
	}
	if pkt.Operation != arp.OperationRequest {
		return nil
	}
	if !r.isIPAssigned(pkt.TargetIP) {
		klog.V(4).InfoS("Ignore ARP request", "ip", pkt.TargetIP, "interface", r.iface.Name)
		return nil
	}
	klog.V(4).InfoS("Send ARP response", "ip", pkt.TargetIP, "interface", r.iface.Name)
	if err := r.conn.Reply(pkt, r.iface.HardwareAddr, pkt.TargetIP); err != nil {
		return fmt.Errorf("failed to reply ARP packet for IP %s: %v", pkt.TargetIP, err)
	}
	return nil
}

func (r *ARPResponder) Run(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		case <-r.closed:
			return
		default:
			err := r.HandleARPRequest()
			if err != nil {
				klog.ErrorS(err, "Handle ARP request error", "deviceName", r.iface.Name)
			}
		}
	}
}

func (r *ARPResponder) Stop() error {
	close(r.closed)
	return r.conn.Close()
}
