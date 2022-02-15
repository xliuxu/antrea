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

	"github.com/mdlayher/ndp"
	"golang.org/x/net/ipv6"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type ndpConn interface {
	WriteTo(message ndp.Message, cm *ipv6.ControlMessage, dstIP net.IP) error
	ReadFrom() (ndp.Message, *ipv6.ControlMessage, net.IP, error)
	JoinGroup(net.IP) error
	LeaveGroup(net.IP) error
	Close() error
}

type NDPResponder struct {
	iface           *net.Interface
	conn            ndpConn
	closed          chan struct{}
	isIPAssigned    AssignedIPChecker
	multicastGroups map[int]int
}

func NewNDPResponder(iface *net.Interface, assignedIPChecker AssignedIPChecker) (*NDPResponder, error) {
	conn, _, err := ndp.Listen(iface, ndp.LinkLocal)
	if err != nil {
		return nil, err
	}
	return &NDPResponder{
		iface:           iface,
		conn:            conn,
		closed:          make(chan struct{}),
		multicastGroups: make(map[int]int),
		isIPAssigned:    assignedIPChecker,
	}, nil
}

func (r *NDPResponder) InterfaceName() string {
	return r.iface.Name
}

func (r *NDPResponder) JoinMulticastGroup(group net.IP) error {
	return r.conn.JoinGroup(group)
}

func (r *NDPResponder) LeaveMulticastGroup(group net.IP) error {
	return r.conn.LeaveGroup(group)
}

func (r *NDPResponder) NeighborAdvertisement(ip net.IP) error {
	select {
	case <-r.closed:
		return fmt.Errorf("NDP responder socket is closed")
	default:
		if !utilnet.IsIPv6(ip) {
			return fmt.Errorf("only IPv6 is supported")
		}
		na := &ndp.NeighborAdvertisement{
			Override:      true,
			TargetAddress: ip,
			Options: []ndp.Option{
				&ndp.LinkLayerAddress{
					Direction: ndp.Target,
					Addr:      r.iface.HardwareAddr,
				},
			},
		}
		return r.conn.WriteTo(na, nil, net.IPv6linklocalallnodes)
	}
}

func (r *NDPResponder) handleNeighborSolicitation() error {
	pkt, _, srcIP, err := r.conn.ReadFrom()
	if err != nil {
		return err
	}
	ns, ok := pkt.(*ndp.NeighborSolicitation)
	if !ok {
		return nil
	}
	var nsSourceHWAddr net.HardwareAddr
	for _, o := range ns.Options {
		addr, ok := o.(*ndp.LinkLayerAddress)
		if !ok {
			continue
		}
		if addr.Direction != ndp.Source {
			continue
		}
		nsSourceHWAddr = addr.Addr
		break
	}
	if nsSourceHWAddr == nil {
		return nil
	}
	if !r.isIPAssigned(ns.TargetAddress) {
		klog.V(4).InfoS("Ignore Neighbor Solicitation", "ip", ns.TargetAddress.String(), "interface", r.iface.Name)
		return nil
	}
	na := &ndp.NeighborAdvertisement{
		Solicited:     true,
		TargetAddress: ns.TargetAddress,
		Options: []ndp.Option{
			&ndp.LinkLayerAddress{
				Direction: ndp.Target,
				Addr:      r.iface.HardwareAddr,
			},
		},
	}
	klog.V(4).InfoS("Send Neighbor Advertisement", "ip", ns.TargetAddress.String(), "interface", r.iface.Name)
	return r.conn.WriteTo(na, nil, srcIP)
}

func (r *NDPResponder) Run(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		case <-r.closed:
			return
		default:
			err := r.handleNeighborSolicitation()
			if err != nil {
				klog.ErrorS(err, "Handle Neighbor Solicitation error", "deviceName", r.iface.Name)
			}
		}
	}
}

func (r *NDPResponder) Stop() error {
	close(r.closed)
	return r.conn.Close()
}
