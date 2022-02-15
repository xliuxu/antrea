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
	"bytes"
	"net"
	"testing"

	"github.com/mdlayher/ndp"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/ipv6"
	"k8s.io/apimachinery/pkg/util/sets"
)

type fakeNDPConn struct {
	readFrom func() (ndp.Message, *ipv6.ControlMessage, net.IP, error)
	writeTo  func(ndp.Message, *ipv6.ControlMessage, net.IP) error
}

func (c *fakeNDPConn) ReadFrom() (ndp.Message, *ipv6.ControlMessage, net.IP, error) {
	return c.readFrom()
}

func (c *fakeNDPConn) WriteTo(message ndp.Message, cm *ipv6.ControlMessage, dstIP net.IP) error {
	return c.writeTo(message, cm, dstIP)
}

func (c *fakeNDPConn) Close() error {
	return nil
}

func (c *fakeNDPConn) JoinGroup(ip net.IP) error {
	return nil
}

func (c *fakeNDPConn) LeaveGroup(ip net.IP) error {
	return nil
}

func TestNDPResponder_NeighborAdvertisement(t *testing.T) {
	buffer := bytes.NewBuffer(nil)
	fakeConn := &fakeNDPConn{
		writeTo: func(msg ndp.Message, _ *ipv6.ControlMessage, _ net.IP) error {
			bs, err := ndp.MarshalMessage(msg)
			assert.NoError(t, err)
			buffer.Write(bs)
			return nil
		},
	}
	responder := &NDPResponder{
		iface: newFakeNetworkInterface(),
		conn:  fakeConn,
	}
	err := responder.NeighborAdvertisement(net.ParseIP("fe80::250:56ff:fea7:e29d"))
	assert.NoError(t, err)
	//  Neighbor Advertisement Message Format - RFC 4861 Section 4.4.
	//   0                   1                   2                   3
	//   0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	//  +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//  |     Type      |     Code      |          Checksum             |
	//  +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//  |R|S|O|                     Reserved                            |
	//  +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//  |                                                               |
	//  +                                                               +
	//  |                                                               |
	//  +                       Target Address                          +
	//  |                                                               |
	//  +                                                               +
	//  |                                                               |
	//  +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//  |   Options ...
	//  +-+-+-+-+-+-+-+-+-+-+-+-
	//
	//  Options formats - Source/Target Link-layer Address. RFC 4861 Section 4.6.1.
	//   0                   1                   2                   3
	//   0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	//  +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//  |     Type      |    Length     |    Link-Layer Address ...
	//  +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	expectedBytes := []byte{
		0x88,       // type - 136 for Neighbor Advertisement
		0x00,       // code
		0x00, 0x00, // checksum
		0x20, 0x00, 0x00, 0x00, // flags and reserved bits. Override bit is set.
		0xfe, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x50, 0x56, 0xff, 0xfe, 0xa7, 0xe2, 0x9d, // IPv6 address
		0x02,                               // option - 2 for Target Link-layer Address
		0x01,                               // length (units of 8 octets including type and length fields)
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, // hardware address
	}
	assert.Equal(t, expectedBytes, buffer.Bytes())
}

func TestNDPResponder_handleNeighborSolicitation(t *testing.T) {
	tests := []struct {
		name           string
		requestMessage []byte
		requestIP      net.IP
		assignedIPs    []net.IP
		expectError    bool
		expectedReply  []byte
	}{
		{
			name: "request to assigned IP",
			requestMessage: []byte{
				0x87,       // type - 135 for Neighbor Solicitation
				0x00,       // code
				0x00, 0x00, // checksum
				0x00, 0x00, 0x00, 0x00, // reserved bits.
				0xfe, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xa1, // IPv6 address
				0x01,                               // option - 1 for Source Link-layer Address
				0x01,                               // length (units of 8 octets including type and length fields)
				0x00, 0x11, 0x22, 0x33, 0x44, 0x66, // hardware address
			},
			requestIP: net.ParseIP("fe80::c1"),
			assignedIPs: []net.IP{
				net.ParseIP("fe80::a1"),
				net.ParseIP("fe80::a2"),
			},
			expectError: false,
			expectedReply: []byte{
				0x88,       // type - 136 for Neighbor Advertisement
				0x00,       // code
				0x00, 0x00, // checksum
				0x40, 0x00, 0x00, 0x00, // flags and reserved bits. Solicited bit is set.
				0xfe, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xa1, // IPv6 address
				0x02,                               // option - 2 for Target Link-layer Address
				0x01,                               // length (units of 8 octets including type and length fields)
				0x00, 0x11, 0x22, 0x33, 0x44, 0x55, // hardware address
			},
		},
		{
			name: "request to not assigned IP",
			requestMessage: []byte{
				0x87,       // type - 135 for Neighbor Solicitation
				0x00,       // code
				0x00, 0x00, // checksum
				0x00, 0x00, 0x00, 0x00, // reserved bits.
				0xfe, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xa3, // IPv6 address
				0x01,                               // option - 1 for Source Link-layer Address
				0x01,                               // length (units of 8 octets including type and length fields)
				0x00, 0x11, 0x22, 0x33, 0x44, 0x66, // hardware address
			},
			requestIP: net.ParseIP("fe80::c1"),
			assignedIPs: []net.IP{
				net.ParseIP("fe80::a1"),
				net.ParseIP("fe80::a2"),
			},
			expectError:   false,
			expectedReply: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buffer := bytes.NewBuffer(nil)
			fakeConn := &fakeNDPConn{
				writeTo: func(msg ndp.Message, _ *ipv6.ControlMessage, _ net.IP) error {
					bs, err := ndp.MarshalMessage(msg)
					assert.NoError(t, err)
					buffer.Write(bs)
					return nil
				},
				readFrom: func() (ndp.Message, *ipv6.ControlMessage, net.IP, error) {
					msg, err := ndp.ParseMessage(tt.requestMessage)
					return msg, nil, tt.requestIP, err
				},
			}
			assignedIPs := sets.NewString()
			for _, ip := range tt.assignedIPs {
				assignedIPs.Insert(ip.String())
			}
			responder := &NDPResponder{
				iface: newFakeNetworkInterface(),
				conn:  fakeConn,
				isIPAssigned: func(ip net.IP) bool {
					return assignedIPs.Has(ip.String())
				},
			}
			err := responder.handleNeighborSolicitation()
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedReply, buffer.Bytes())
			}
		})
	}
}
