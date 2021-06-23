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
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type fakeWireGuardClient struct {
	peers map[wgtypes.Key]wgtypes.Peer
}

func (f *fakeWireGuardClient) Close() error {
	return nil
}

func (f *fakeWireGuardClient) Devices() ([]*wgtypes.Device, error) {
	return nil, nil
}

func (f *fakeWireGuardClient) Device(name string) (*wgtypes.Device, error) {
	var res []wgtypes.Peer
	for _, p := range f.peers {
		res = append(res, p)
	}
	return &wgtypes.Device{
		Peers: res,
	}, nil
}

func (f *fakeWireGuardClient) ConfigureDevice(name string, cfg wgtypes.Config) error {
	for _, c := range cfg.Peers {
		if c.Remove {
			delete(f.peers, c.PublicKey)
		} else {
			peer := wgtypes.Peer{
				PublicKey: c.PublicKey,
				Endpoint:  c.Endpoint,
			}
			if c.Endpoint != nil {
				ep := &net.UDPAddr{}
				ip := c.Endpoint.IP
				ep.IP = append(ip[:0:0], ip...)
				ep.Port = c.Endpoint.Port
				ep.Zone = c.Endpoint.Zone
				peer.Endpoint = ep
			}
			for _, n := range c.AllowedIPs {
				peer.AllowedIPs = append(peer.AllowedIPs, net.IPNet{
					IP:   append(n.IP[:0:0], n.IP...),
					Mask: append(n.Mask[:0:0], n.Mask...),
				})
			}
			f.peers[c.PublicKey] = peer
		}
	}
	return nil
}
func getFakeClient() *client {
	return &client{
		wgClient:       &fakeWireGuardClient{},
		nodeName:       "fake-node-1",
		mtu:            1420,
		listenPort:     12345,
		peerByNodeName: &sync.Map{},
		enabledIPv4:    true,
		enabledIPv6:    false,
		nodeIPv4:       net.ParseIP("10.20.30.41"),
	}
}

func deepCopyForWireGuardPeer(peer wgtypes.Peer) wgtypes.Peer {
	c := wgtypes.Peer{}
	c.AllowedIPs = make([]net.IPNet, len(peer.AllowedIPs))
	for idx := range peer.AllowedIPs {
		ip := peer.AllowedIPs[idx].IP
		mask := peer.AllowedIPs[idx].Mask
		c.AllowedIPs[idx].IP = append(ip[:0:0], ip...)
		c.AllowedIPs[idx].Mask = append(mask[:0:0], mask...)
	}
	if peer.Endpoint != nil {
		ep := &net.UDPAddr{}
		ip := peer.Endpoint.IP
		ep.IP = append(ip[:0:0], ip...)
		ep.Port = peer.Endpoint.Port
		ep.Zone = peer.Endpoint.Zone
		c.Endpoint = ep
	}
	c.PublicKey = peer.PublicKey
	return c
}

func Test_RemoveStalePeers(t *testing.T) {
	pk1, _ := wgtypes.GeneratePrivateKey()
	pk2, _ := wgtypes.GeneratePrivateKey()
	pk3, _ := wgtypes.GeneratePrivateKey()
	tests := []struct {
		name            string
		existingPeers   map[wgtypes.Key]wgtypes.Peer
		inputPublicKeys []string
		expectedPeers   map[wgtypes.Key]wgtypes.Peer
	}{
		{
			"pass empty/nil slice should remove all existing peers",
			map[wgtypes.Key]wgtypes.Peer{
				pk1.PublicKey(): {PublicKey: pk1.PublicKey()},
				pk2.PublicKey(): {PublicKey: pk2.PublicKey()},
			},
			nil,
			map[wgtypes.Key]wgtypes.Peer{},
		},
		{
			"args has no intersection with existing peers",
			map[wgtypes.Key]wgtypes.Peer{
				pk1.PublicKey(): {PublicKey: pk1.PublicKey()},
				pk2.PublicKey(): {PublicKey: pk2.PublicKey()},
			},
			[]string{pk3.PublicKey().String()},
			map[wgtypes.Key]wgtypes.Peer{},
		},
		{
			"should only keep peers passed by args",
			map[wgtypes.Key]wgtypes.Peer{
				pk1.PublicKey(): {PublicKey: pk1.PublicKey()},
				pk2.PublicKey(): {PublicKey: pk2.PublicKey()},
				pk3.PublicKey(): {PublicKey: pk3.PublicKey()},
			},
			[]string{pk3.PublicKey().String()},
			map[wgtypes.Key]wgtypes.Peer{
				pk3.PublicKey(): {PublicKey: pk3.PublicKey()},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := getFakeClient()
			fc := &fakeWireGuardClient{peers: tt.existingPeers}
			client.wgClient = fc
			err := client.RemoveStalePeers(tt.inputPublicKeys)
			assert.Nil(t, err)
			assert.Equal(t, tt.expectedPeers, fc.peers)
		})
	}
}

func Test_UpdatePeer(t *testing.T) {
	pk1, _ := wgtypes.GeneratePrivateKey()
	pk2, _ := wgtypes.GeneratePrivateKey()
	ip1, ipnet1, _ := net.ParseCIDR("10.20.30.42/32")
	listenPort := getFakeClient().listenPort
	tests := []struct {
		name                   string
		existingPeers          map[string]wgtypes.Peer
		inputPeerNodeName      string
		inputPeerNodePublicKey string
		inputPeerNodeIPv4      net.IP
		inputPeerNodeIPv6      net.IP
		expectedError          bool
		expectedPeers          map[wgtypes.Key]wgtypes.Peer
	}{
		{
			"call update peer to add new peers",
			map[string]wgtypes.Peer{},
			"fake-node-2",
			pk1.PublicKey().String(),
			ip1.To4(),
			nil,
			false,
			map[wgtypes.Key]wgtypes.Peer{
				pk1.PublicKey(): {
					PublicKey: pk1.PublicKey(),
					Endpoint: &net.UDPAddr{
						IP:   ip1,
						Port: listenPort,
					},
					AllowedIPs: []net.IPNet{*ipnet1},
				},
			},
		},
		{
			"call update peer to update existing peers",
			map[string]wgtypes.Peer{
				"fake-node-2": {
					PublicKey: pk2.PublicKey(),
					Endpoint: &net.UDPAddr{
						IP:   ip1,
						Port: listenPort,
					},
					AllowedIPs: []net.IPNet{*ipnet1},
				},
			},
			"fake-node-2",
			pk2.PublicKey().String(),
			ip1.To4(),
			nil,
			false,
			map[wgtypes.Key]wgtypes.Peer{
				pk2.PublicKey(): {
					PublicKey: pk2.PublicKey(),
					Endpoint: &net.UDPAddr{
						IP:   ip1,
						Port: listenPort,
					},
					AllowedIPs: []net.IPNet{*ipnet1},
				},
			},
		},
		{
			"call update peer with invalid public key",
			map[string]wgtypes.Peer{
				"fake-node-2": {
					PublicKey: pk2.PublicKey(),
					Endpoint: &net.UDPAddr{
						IP:   ip1,
						Port: listenPort,
					},
					AllowedIPs: []net.IPNet{*ipnet1},
				},
			},
			"fake-node-2",
			"invalid key",
			ip1.To4(),
			nil,
			true,
			map[wgtypes.Key]wgtypes.Peer{
				pk2.PublicKey(): {
					PublicKey: pk2.PublicKey(),
					Endpoint: &net.UDPAddr{
						IP:   ip1,
						Port: listenPort,
					},
					AllowedIPs: []net.IPNet{*ipnet1},
				},
			},
		},
		{
			"call update peer with nil IP",
			map[string]wgtypes.Peer{
				"fake-node-2": {
					PublicKey: pk2.PublicKey(),
					Endpoint: &net.UDPAddr{
						IP:   ip1,
						Port: listenPort,
					},
					AllowedIPs: []net.IPNet{*ipnet1},
				},
			},
			"fake-node-2",
			pk2.PublicKey().String(),
			nil,
			nil,
			true,
			map[wgtypes.Key]wgtypes.Peer{
				pk2.PublicKey(): {
					PublicKey: pk2.PublicKey(),
					Endpoint: &net.UDPAddr{
						IP:   ip1,
						Port: listenPort,
					},
					AllowedIPs: []net.IPNet{
						*ipnet1,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := getFakeClient()
			fc := &fakeWireGuardClient{
				peers: map[wgtypes.Key]wgtypes.Peer{},
			}
			for _, ec := range tt.existingPeers {
				fc.peers[ec.PublicKey] = deepCopyForWireGuardPeer(ec)
			}
			client.wgClient = fc
			err := client.UpdatePeer(tt.inputPeerNodeName, tt.inputPeerNodePublicKey, tt.inputPeerNodeIPv4, tt.inputPeerNodeIPv6)
			if tt.expectedError {
				assert.NotNil(t, err)
			} else {
				assert.Nil(t, err)
			}
			t.Logf("fc peers: %#v", fc.peers)
			assert.Equal(t, tt.expectedPeers, fc.peers)
		})
	}
}

func Test_DeletePeer(t *testing.T) {
	client := getFakeClient()
	fc := &fakeWireGuardClient{
		peers: map[wgtypes.Key]wgtypes.Peer{},
	}
	client.wgClient = fc
	pk1, _ := wgtypes.GeneratePrivateKey()
	ip1, _, _ := net.ParseCIDR("10.20.30.42/32")
	t.Run("delete non-existing peer", func(tt *testing.T) {
		err := client.UpdatePeer("fake-node-1", pk1.String(), ip1.To4(), nil)
		assert.NoError(tt, err)
		assert.Len(tt, fc.peers, 1)
		_, ok := client.peerByNodeName.Load("fake-node-1")
		assert.True(t, ok)
		err = client.DeletePeer("fake-node-2")
		assert.NoError(tt, err)
		assert.Len(tt, fc.peers, 1)
		_, ok = client.peerByNodeName.Load("fake-node-1")
		assert.True(t, ok)
	})

	t.Run("delete existing peer", func(tt *testing.T) {
		err := client.DeletePeer("fake-node-1")
		assert.NoError(tt, err)
		assert.Len(tt, fc.peers, 0)
		_, ok := client.peerByNodeName.Load("fake-node-1")
		assert.False(t, ok)
	})

}
