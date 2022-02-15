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
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_parseIPv6SolicitedNodeMulticastAddress(t *testing.T) {
	tests := []struct {
		name          string
		ip            net.IP
		expectedGroup net.IP
		expectedKey   int
	}{
		{
			name:          "global unicast IPv6 address 1",
			ip:            net.ParseIP("2022:abcd::11:1111"),
			expectedGroup: net.ParseIP("ff02::1:ff11:1111"),
			expectedKey:   0x111111,
		},
		{
			name:          "global unicast IPv6 address 2",
			ip:            net.ParseIP("2022:ffff::1234:5678"),
			expectedGroup: net.ParseIP("ff02::1:ff34:5678"),
			expectedKey:   0x345678,
		},
		{
			name:          "link-local unicast IPv6 address",
			ip:            net.ParseIP("fe80::1122:3344"),
			expectedGroup: net.ParseIP("ff02::1:ff22:3344"),
			expectedKey:   0x223344,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group, key := parseIPv6SolicitedNodeMulticastAddress(tt.ip)
			assert.Equal(t, tt.expectedGroup, group)
			assert.Equal(t, tt.expectedKey, key)
		})
	}
}

func Test_keyToSolicitedNodeMulticastAddress(t *testing.T) {
	tests := []struct {
		name          string
		key           int
		expectedGroup net.IP
	}{
		{
			name:          "test key 1",
			key:           0x111111,
			expectedGroup: net.ParseIP("ff02::1:ff11:1111"),
		},
		{
			name:          "test key 2",
			key:           0x345678,
			expectedGroup: net.ParseIP("ff02::1:ff34:5678"),
		},
		{
			name:          "test key 3",
			key:           0x223344,
			expectedGroup: net.ParseIP("ff02::1:ff22:3344"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group := keyToSolicitedNodeMulticastAddress(tt.key)
			assert.Equal(t, tt.expectedGroup, group)
		})
	}
}
