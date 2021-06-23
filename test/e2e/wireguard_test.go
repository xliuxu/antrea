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

package e2e

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/wait"

	"antrea.io/antrea/pkg/agent/config"
)

// TestWireGuardTunnelConnectivity checks that Pod traffic across two Nodes over
// the WireGuard tunnel, by creating multiple Pods across distinct Nodes and having
// them ping each other.
func TestWireGuardTunnelConnectivity(t *testing.T) {
	skipIfProviderIs(t, "kind", "pkt_mark does not work with ovs netdev datapath")
	skipIfNumNodesLessThan(t, 2)
	skipIfHasWindowsNodes(t)
	for _, node := range clusterInfo.nodes {
		skipIfMissingKernelModule(t, node.name, []string{"wireguard"})
	}
	data, err := setupTest(t)
	if err != nil {
		t.Fatalf("Error when setting up test: %v", err)
	}
	defer teardownTest(t, data)
	// WireGuard currently only works with encap mode
	skipIfEncapModeIsNot(t, data, config.TrafficEncapModeEncap)

	ac := []configChange{
		{"trafficEncryptionMode", "wireguard", false},
	}
	if err := data.mutateAntreaConfigMap(nil, ac, false, true); err != nil {
		t.Fatalf("Failed to enable WireGuard tunnel: %v", err)
	}
	defer func() {
		ac = []configChange{
			{"trafficEncryptionMode", "none", false},
		}
		if err := data.mutateAntreaConfigMap(nil, ac, false, true); err != nil {
			t.Fatalf("Failed to disable WireGuard tunnel: %v", err)
		}
	}()
	data.testPodConnectivityDifferentNodes(t)
}

func TestWireGuardRoutesAndCleanup(t *testing.T) {
	skipIfProviderIs(t, "kind", "pkt_mark does not work with ovs netdev datapath")
	skipIfNumNodesLessThan(t, 2)
	skipIfHasWindowsNodes(t)

	var antreaPodName string
	var err error
	data, err := setupTest(t)
	if err != nil {
		t.Fatalf("Error when setting up test: %v", err)
	}
	defer teardownTest(t, data)
	// WireGuard currently only works with encap mode
	skipIfEncapModeIsNot(t, data, config.TrafficEncapModeEncap)

	ac := []configChange{
		{"enableWireGuardTunnel", "true", false},
	}

	getAntreaPodName := func() string {
		nodeName := nodeName(0)
		if antreaPodName, err = data.getAntreaPodOnNode(nodeName); err != nil {
			t.Fatalf("Error when retrieving the name of the Antrea Pod running on Node '%s': %v", nodeName, err)
		}
		return antreaPodName
	}

	wireGuardInterfaceExist := func() bool {
		cmd := []string{"ip", "link", "show", "antrea-wg"}
		stdout, stderr, err := data.runCommandFromPod(antreaNamespace, getAntreaPodName(), agentContainerName, cmd)
		if err != nil {
			if strings.Contains(stderr, "does not exist") {
				return false
			}
			t.Fatalf("Error when running ip command in Pod '%s': %v - stdout: %s - stderr: %s", antreaPodName, err, stdout, stderr)
		}
		return true
	}

	wireGuardRoutingRuleExist := func() bool {
		cmd := []string{"ip", "rule", "list"}
		stdout, stderr, err := data.runCommandFromPod(antreaNamespace, getAntreaPodName(), agentContainerName, cmd)
		if err != nil {
			t.Fatalf("Error when running ip command in Pod '%s': %v - stdout: %s - stderr: %s", antreaPodName, err, stdout, stderr)
			return false
		}
		for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
			if strings.Contains(line, "from all fwmark 0x100/0x100") {
				return true
			}
		}
		return false
	}

	defaultRoutingExist := func() bool {
		cmd := []string{"ip", "route", "show", "default", "table", "10"}
		stdout, stderr, err := data.runCommandFromPod(antreaNamespace, getAntreaPodName(), agentContainerName, cmd)
		if err != nil {
			if strings.Contains(stderr, "does not exist") {
				return false
			}
			t.Fatalf("Error when running ip command in Pod '%s': %v - stdout: %s - stderr: %s", getAntreaPodName(), err, stdout, stderr)
		}
		for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
			if strings.Contains(line, "antrea-wg") {
				return true
			}
		}
		return false
	}

	defer func() {
		ac = []configChange{
			{"enableWireGuardTunnel", "false", false},
		}
		if err := data.mutateAntreaConfigMap(nil, ac, false, true); err != nil {
			t.Fatalf("Failed to disable WireGuard tunnel: %v", err)
		}
		t.Logf("Checking that WireGuard tunnel interface has been deleted")
		if err := wait.PollImmediate(defaultInterval, defaultTimeout, func() (found bool, err error) {
			return !wireGuardInterfaceExist(), nil
		}); err == wait.ErrWaitTimeout {
			t.Fatalf("Timed out while waiting for WireGuard interface to be deleted")
		} else if err != nil {
			t.Fatalf("Error while waiting for WireGuard interface to be created")
		}
		t.Logf("Checking that WireGuard routes and rules has been deleted")
		if wireGuardRoutingRuleExist() {
			t.Errorf("WireGuard routing rule has not beed deleted")
		}
		if defaultRoutingExist() {
			t.Errorf("Default routing for WireGuard has not beed deleted")
		}
	}()

	if err := data.mutateAntreaConfigMap(nil, ac, false, true); err != nil {
		t.Fatalf("Failed to enable WireGuard tunnel: %v", err)
	}

	t.Logf("Checking that WireGuard tunnel interface has been created")
	if err := wait.PollImmediate(defaultInterval, defaultTimeout, func() (found bool, err error) {
		return wireGuardInterfaceExist(), nil
	}); err == wait.ErrWaitTimeout {
		t.Fatalf("Timed out while waiting for WireGuard interface to be created")
	} else if err != nil {
		t.Fatalf("Error while waiting for WireGuard interface to be created")
	}
	t.Logf("Checking that WireGuard routes and rules has been created")

	if !wireGuardRoutingRuleExist() {
		t.Errorf("WireGuard routing rule does not exist")
	}
	if !defaultRoutingExist() {
		t.Errorf("Default routing for tabe WireGuard with pkt_mark does not exist")
	}
}
