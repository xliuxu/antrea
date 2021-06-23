// +build linux

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

package wireguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"antrea.io/antrea/pkg/agent/types"
	"antrea.io/antrea/pkg/agent/util/sysctl"
)

const (
	ifName            = "antrea-wg0"
	routeRulePriority = 10
	routeTable        = 10
)

var zeroKey = wgtypes.Key{}

// wgctrlClient is an interface to mock wgctrl.Client
type wgctrlClient interface {
	io.Closer
	Devices() ([]*wgtypes.Device, error)
	Device(name string) (*wgtypes.Device, error)
	ConfigureDevice(name string, config wgtypes.Config) error
}

var _ Interface = (*client)(nil)

type client struct {
	wgClient                 wgctrlClient
	nodeName                 string
	k8sClient                clientset.Interface
	mtu                      int
	listenPort               int
	privateKey               wgtypes.Key
	peerByNodeName           *sync.Map
	enabledIPv4, enabledIPv6 bool
	nodeIPv4, nodeIPv6       net.IP
}

func New(nodeName string, k8sClient clientset.Interface, mtu int, nodeIPv4, nodeIPv6 net.IP, enabledIPv4, enabledIPv6 bool, port int) (Interface, error) {
	wgClient, err := wgctrl.New()
	if err != nil {
		return nil, err
	}
	return &client{
		wgClient:       wgClient,
		nodeName:       nodeName,
		k8sClient:      k8sClient,
		mtu:            mtu,
		listenPort:     port,
		nodeIPv4:       nodeIPv4,
		nodeIPv6:       nodeIPv6,
		enabledIPv4:    enabledIPv4,
		enabledIPv6:    enabledIPv6,
		peerByNodeName: &sync.Map{},
	}, nil
}

func (client *client) Init() error {
	link := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: ifName, MTU: client.mtu}}
	err := netlink.LinkAdd(link)
	// ignore existing link as it has already been created or managed by userspace process.
	if err != nil && !errors.Is(err, unix.EEXIST) {
		if errors.Is(err, unix.EOPNOTSUPP) {
			return fmt.Errorf("WireGuard not supported by the Linux kernel (netlink: %w)", err)
		}
		return err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	if client.enabledIPv4 {
		if sysctl.EnsureSysctlNetValue("ipv4/conf/all/rp_filter", 0) != nil {
			return fmt.Errorf("setting net.ipv4.conf.all.rp_filter failed: %w", err)
		}
		if sysctl.EnsureSysctlNetValue(fmt.Sprintf("ipv4/conf/%s/rp_filter", ifName), 0) != nil {
			return fmt.Errorf("setting net.ipv4.conf.%s.rp_filter failed: %w", ifName, err)
		}
		if err := client.ensureRouting(netlink.FAMILY_V4); err != nil {
			return err
		}
	}
	if client.enabledIPv6 {
		if err := client.ensureRouting(netlink.FAMILY_V6); err != nil {
			return err
		}
	}
	wgDev, err := client.wgClient.Device(ifName)
	if err != nil {
		return err
	}
	client.privateKey = wgDev.PrivateKey
	// WireGuard private key will be persistent when agent restarts. So antrea only needs to
	// generate a new private key if it is empty (all zero).
	if client.privateKey == zeroKey {
		newPkey, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return err
		}
		client.privateKey = newPkey
	}
	cfg := wgtypes.Config{
		PrivateKey:   &client.privateKey,
		ListenPort:   &client.listenPort,
		ReplacePeers: false,
	}
	patch, _ := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				types.NodeWireGuardPublicKey: client.privateKey.PublicKey().String(),
			},
		},
	})
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := client.k8sClient.CoreV1().Nodes().Patch(context.TODO(), client.nodeName, apitypes.MergePatchType, patch, metav1.PatchOptions{})
		return err
	}); err != nil {
		return err
	}

	return client.wgClient.ConfigureDevice(ifName, cfg)
}

func (client *client) RemoveStalePeers(existingPeerPublickeys []string) error {
	wgdev, err := client.wgClient.Device(ifName)
	if err != nil {
		return err
	}
	restoredPeerPublicKeys := make(map[wgtypes.Key]struct{})
	for _, peer := range wgdev.Peers {
		restoredPeerPublicKeys[peer.PublicKey] = struct{}{}
	}
	for _, e := range existingPeerPublickeys {
		pubKey, err := wgtypes.ParseKey(e)
		if err != nil {
			klog.ErrorS(err, "Parse WireGuard public error")
			continue
		}
		delete(restoredPeerPublicKeys, pubKey)
	}
	for k := range restoredPeerPublicKeys {
		if err := client.deletePeerByPublicKey(k); err != nil {
			klog.ErrorS(err, "Delete WireGuard peer error")
		}
	}
	return nil
}

func (client *client) UpdatePeer(nodeName, publicKeyString string, nodeIPv4, nodeIPv6 net.IP) error {
	pubKey, err := wgtypes.ParseKey(publicKeyString)
	if err != nil {
		return err
	}
	var endpoint string
	// We can only set one endpoint for each peer. Shoud we prioritize using IPv6?
	if client.enabledIPv4 && nodeIPv4 != nil {
		endpoint = net.JoinHostPort(nodeIPv4.String(), strconv.Itoa(client.listenPort))
	} else if client.enabledIPv6 && nodeIPv6 != nil {
		endpoint = net.JoinHostPort(nodeIPv6.String(), strconv.Itoa(client.listenPort))
	} else {
		return fmt.Errorf("node IP missing: %s", nodeName)
	}
	if p, exist := client.peerByNodeName.Load(nodeName); exist {
		peer := p.(wgtypes.PeerConfig)
		if peer.PublicKey.String() != publicKeyString {
			klog.InfoS("WireGuard peer public key changed", "nodeName", nodeName, "publicKey", publicKeyString)
			// delete old peer by public key
			if err := client.deletePeerByPublicKey(peer.PublicKey); err != nil {
				return err
			}
		}
	}
	var allowedIPs []net.IPNet
	if nodeIPv4 != nil {
		allowedIPs = append(allowedIPs, net.IPNet{
			IP:   nodeIPv4,
			Mask: net.CIDRMask(net.IPv4len*8, net.IPv4len*8),
		})
	}
	if nodeIPv6 != nil {
		allowedIPs = append(allowedIPs, net.IPNet{
			IP:   nodeIPv6,
			Mask: net.CIDRMask(net.IPv6len*8, net.IPv6len*8),
		})
	}
	endpointUDP, err := net.ResolveUDPAddr("udp", endpoint)
	if err != nil {
		return err
	}
	peerConfig := wgtypes.PeerConfig{
		PublicKey:         pubKey,
		Endpoint:          endpointUDP,
		AllowedIPs:        allowedIPs,
		ReplaceAllowedIPs: true,
	}
	client.peerByNodeName.Store(nodeName, peerConfig)
	cfg := wgtypes.Config{
		ReplacePeers: false,
		Peers:        []wgtypes.PeerConfig{peerConfig},
	}
	return client.wgClient.ConfigureDevice(ifName, cfg)
}

func (client *client) deletePeerByPublicKey(pubKey wgtypes.Key) error {
	cfg := wgtypes.Config{Peers: []wgtypes.PeerConfig{
		{PublicKey: pubKey, Remove: true},
	}}
	return client.wgClient.ConfigureDevice(ifName, cfg)
}

func (client *client) DeletePeer(nodeName string) error {
	p, exist := client.peerByNodeName.Load(nodeName)
	if !exist {
		return nil
	}
	peer := p.(wgtypes.PeerConfig)
	if err := client.deletePeerByPublicKey(peer.PublicKey); err != nil {
		return err
	}
	client.peerByNodeName.Delete(nodeName)
	return nil
}

// ensureRouting sets route rule, route table and default route for marked traffic.
func (client *client) ensureRouting(ipFamily int) error {
	rule := netlink.NewRule()
	rule.Mark = types.WireGuardRouteMark
	rule.Mask = types.WireGuardRouteMask
	rule.Priority = routeRulePriority
	rule.Table = routeTable
	rule.Family = ipFamily
	exist, err := lookupRule(rule, ipFamily)
	if err != nil {
		return err
	}
	if !exist {
		if err := netlink.RuleAdd(rule); err != nil {
			klog.ErrorS(err, "Failed to add IP rule for WireGuard")
			return err
		}
	}
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return err
	}
	rt := netlink.Route{
		LinkIndex: link.Attrs().Index,
		Table:     routeTable,
		Scope:     netlink.SCOPE_LINK,
	}
	if ipFamily == netlink.FAMILY_V4 {
		rt.Dst = &net.IPNet{
			IP:   net.IPv4zero,
			Mask: net.CIDRMask(0, net.IPv4len),
		}
		rt.Src = client.nodeIPv4
	} else if ipFamily == netlink.FAMILY_V6 {
		rt.Dst = &net.IPNet{
			IP:   net.IPv6zero,
			Mask: net.CIDRMask(0, net.IPv6len),
		}
		rt.Src = client.nodeIPv6
	}
	return netlink.RouteReplace(&rt)
}

func (client *client) Cleanup() error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); !ok {
			return err
		}
	} else {
		if err := netlink.LinkDel(link); err != nil {
			klog.ErrorS(err, "Failed to delete WireGuard link")
		}
	}
	if client.enabledIPv4 {
		if err := client.cleanupRouting(netlink.FAMILY_V4); err != nil {
			return err
		}
	}
	if client.enabledIPv6 {
		if err := client.cleanupRouting(netlink.FAMILY_V6); err != nil {
			return err
		}
	}
	return nil
}

// cleanupRouting delete route rule, route table created by WireGuard client.
func (client *client) cleanupRouting(ipFamily int) error {
	rule := netlink.NewRule()
	rule.Mark = types.WireGuardRouteMark
	rule.Mask = types.WireGuardRouteMask
	rule.Priority = routeRulePriority
	rule.Table = routeTable
	rule.Family = ipFamily
	if err := netlink.RuleDel(rule); err != nil && err != unix.ENOENT {
		klog.ErrorS(err, "Failed to delete routing rule for WireGuard")
		return err
	}
	rt := netlink.Route{
		Table: routeTable,
	}
	if ipFamily == netlink.FAMILY_V4 {
		rt.Dst = &net.IPNet{
			IP:   net.IPv4zero,
			Mask: net.CIDRMask(0, net.IPv4len),
		}
		rt.Src = client.nodeIPv4
	} else if ipFamily == netlink.FAMILY_V6 {
		rt.Dst = &net.IPNet{
			IP:   net.IPv6zero,
			Mask: net.CIDRMask(0, net.IPv6len),
		}
		rt.Src = client.nodeIPv6
	}
	if err := netlink.RouteDel(&rt); err != nil && err != unix.ESRCH {
		klog.ErrorS(err, "Failed to delete route for WireGuard")
		return err
	}
	return nil
}

// lookupRule checks whether existing rule matches. We only care the properities
// needed by WireGuard.
func lookupRule(rule *netlink.Rule, family int) (bool, error) {
	rules, err := netlink.RuleList(family)
	if err != nil {
		return false, err
	}
	for _, r := range rules {
		if rule.Priority != r.Priority || r.Mask != rule.Mask ||
			r.Mark != rule.Mark || rule.Table != r.Table {
			continue
		}
		return true, nil
	}
	return false, nil
}
