package plugin

import (
	"fmt"
	"net"
	"time"

	log "github.com/golang/glog"

	kapi "k8s.io/kubernetes/pkg/api"
	kapiunversioned "k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/client/cache"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/types"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	utilwait "k8s.io/kubernetes/pkg/util/wait"

	osapi "github.com/openshift/origin/pkg/sdn/api"
	"github.com/openshift/origin/pkg/util/netutils"
)

func (master *OsdnMaster) SubnetStartMaster(clusterNetwork *net.IPNet, hostSubnetLength uint32) error {
	subrange := make([]string, 0)
	subnets, err := master.osClient.HostSubnets().List(kapi.ListOptions{})
	if err != nil {
		log.Errorf("Error in initializing/fetching subnets: %v", err)
		return err
	}
	for _, sub := range subnets.Items {
		subrange = append(subrange, sub.Subnet)
		if err = master.networkInfo.validateNodeIP(sub.HostIP); err != nil {
			// Don't error out; just warn so the error can be corrected with 'oc'
			log.Errorf("Failed to validate HostSubnet %s: %v", hostSubnetToString(&sub), err)
		} else {
			log.Infof("Found existing HostSubnet %s", hostSubnetToString(&sub))
		}
	}

	master.subnetAllocator, err = netutils.NewSubnetAllocator(clusterNetwork.String(), hostSubnetLength, subrange)
	if err != nil {
		return err
	}

	go utilwait.Forever(master.watchNodes, 0)
	return nil
}

func (master *OsdnMaster) addNode(nodeName string, nodeIP string) error {
	// Validate node IP before proceeding
	if err := master.networkInfo.validateNodeIP(nodeIP); err != nil {
		return err
	}

	// Check if subnet needs to be created or updated
	sub, err := master.osClient.HostSubnets().Get(nodeName)
	if err == nil {
		if sub.HostIP == nodeIP {
			return nil
		} else {
			// Node IP changed, update old subnet
			sub.HostIP = nodeIP
			sub, err = master.osClient.HostSubnets().Update(sub)
			if err != nil {
				return fmt.Errorf("Error updating subnet %s for node %s: %v", sub.Subnet, nodeName, err)
			}
			log.Infof("Updated HostSubnet %s", hostSubnetToString(sub))
			return nil
		}
	}

	// Create new subnet
	sn, err := master.subnetAllocator.GetNetwork()
	if err != nil {
		return fmt.Errorf("Error allocating network for node %s: %v", nodeName, err)
	}

	sub = &osapi.HostSubnet{
		TypeMeta:   kapiunversioned.TypeMeta{Kind: "HostSubnet"},
		ObjectMeta: kapi.ObjectMeta{Name: nodeName},
		Host:       nodeName,
		HostIP:     nodeIP,
		Subnet:     sn.String(),
	}
	sub, err = master.osClient.HostSubnets().Create(sub)
	if err != nil {
		master.subnetAllocator.ReleaseNetwork(sn)
		return fmt.Errorf("Error creating subnet %s for node %s: %v", sn.String(), nodeName, err)
	}
	log.Infof("Created HostSubnet %s", hostSubnetToString(sub))
	return nil
}

func (master *OsdnMaster) deleteNode(nodeName string) error {
	sub, err := master.osClient.HostSubnets().Get(nodeName)
	if err != nil {
		return fmt.Errorf("Error fetching subnet for node %q for deletion: %v", nodeName, err)
	}
	_, ipnet, err := net.ParseCIDR(sub.Subnet)
	if err != nil {
		return fmt.Errorf("Error parsing subnet %q for node %q for deletion: %v", sub.Subnet, nodeName, err)
	}
	master.subnetAllocator.ReleaseNetwork(ipnet)
	err = master.osClient.HostSubnets().Delete(nodeName)
	if err != nil {
		return fmt.Errorf("Error deleting subnet %v for node %q: %v", sub, nodeName, err)
	}

	log.Infof("Deleted HostSubnet %s", hostSubnetToString(sub))
	return nil
}

func getNodeIP(node *kapi.Node) (string, error) {
	if len(node.Status.Addresses) > 0 && node.Status.Addresses[0].Address != "" {
		return node.Status.Addresses[0].Address, nil
	} else {
		return netutils.GetNodeIP(node.Name)
	}
}

// Because openshift-sdn uses an overlay and doesn't need GCE Routes, we need to
// clear the NetworkUnavailable condition that kubelet adds to initial node
// status when using GCE.
// TODO: make upstream kubelet more flexible with overlays and GCE so this
// condition doesn't get added for network plugins that don't want it, and then
// we can remove this function.
func (master *OsdnMaster) clearInitialNodeNetworkUnavailableCondition(node *kapi.Node) {
	knode := node
	cleared := false
	resultErr := kclient.RetryOnConflict(kclient.DefaultBackoff, func() error {
		var err error

		if knode != node {
			knode, err = master.kClient.Nodes().Get(node.ObjectMeta.Name)
			if err != nil {
				return err
			}
		}

		// Let caller modify knode's status, then push to api server.
		_, condition := kapi.GetNodeCondition(&node.Status, kapi.NodeNetworkUnavailable)
		if condition != nil && condition.Status != kapi.ConditionFalse && condition.Reason == "NoRouteCreated" {
			condition.Status = kapi.ConditionFalse
			condition.Reason = "RouteCreated"
			condition.Message = "openshift-sdn cleared kubelet-set NoRouteCreated"
			condition.LastTransitionTime = kapiunversioned.Now()
			knode, err = master.kClient.Nodes().UpdateStatus(knode)
			if err == nil {
				cleared = true
			}
		}
		return err
	})
	if resultErr != nil {
		utilruntime.HandleError(fmt.Errorf("Status update failed for local node: %v", resultErr))
	} else if cleared {
		log.Infof("Cleared node NetworkUnavailable/NoRouteCreated condition for %s", node.ObjectMeta.Name)
	}
}

func (master *OsdnMaster) watchNodes() {
	nodeAddressMap := map[types.UID]string{}
	RunEventQueue(master.kClient, Nodes, func(delta cache.Delta) error {
		node := delta.Object.(*kapi.Node)
		name := node.ObjectMeta.Name
		uid := node.ObjectMeta.UID

		nodeIP, err := getNodeIP(node)
		if err != nil {
			return fmt.Errorf("failed to get node IP for %s, skipping event: %v, node: %v", name, delta.Type, node)
		}

		switch delta.Type {
		case cache.Sync, cache.Added, cache.Updated:
			master.clearInitialNodeNetworkUnavailableCondition(node)

			if oldNodeIP, ok := nodeAddressMap[uid]; ok && (oldNodeIP == nodeIP) {
				break
			}
			// Node status is frequently updated by kubelet, so log only if the above condition is not met
			log.V(5).Infof("Watch %s event for Node %q", delta.Type, name)

			err = master.addNode(name, nodeIP)
			if err != nil {
				return fmt.Errorf("error creating subnet for node %s, ip %s: %v", name, nodeIP, err)
			}
			nodeAddressMap[uid] = nodeIP
		case cache.Deleted:
			log.V(5).Infof("Watch %s event for Node %q", delta.Type, name)
			delete(nodeAddressMap, uid)

			err = master.deleteNode(name)
			if err != nil {
				return fmt.Errorf("Error deleting node %s: %v", name, err)
			}
		}
		return nil
	})
}

func (node *OsdnNode) SubnetStartNode(mtu uint32) (bool, error) {
	err := node.initSelfSubnet()
	if err != nil {
		return false, err
	}

	networkChanged, err := node.SetupSDN(node.localSubnet.Subnet, node.networkInfo.ClusterNetwork.String(), node.networkInfo.ServiceNetwork.String(), mtu)
	if err != nil {
		return false, err
	}

	go utilwait.Forever(node.watchSubnets, 0)
	return networkChanged, nil
}

func (node *OsdnNode) initSelfSubnet() error {
	// timeout: 30 secs
	retries := 60
	retryInterval := 500 * time.Millisecond

	var err error
	var subnet *osapi.HostSubnet
	// Try every retryInterval and bail-out if it exceeds max retries
	for i := 0; i < retries; i++ {
		// Get subnet for current node
		subnet, err = node.osClient.HostSubnets().Get(node.hostName)
		if err == nil {
			break
		}
		log.Warningf("Could not find an allocated subnet for node: %s, Waiting...", node.hostName)
		time.Sleep(retryInterval)
	}
	if err != nil {
		return fmt.Errorf("Failed to get subnet for this host: %s, error: %v", node.hostName, err)
	}

	if err = node.networkInfo.validateNodeIP(subnet.HostIP); err != nil {
		return fmt.Errorf("Failed to validate own HostSubnet: %v", err)
	}

	log.Infof("Found local HostSubnet %s", hostSubnetToString(subnet))
	node.localSubnet = subnet
	return nil
}

// Only run on the nodes
func (node *OsdnNode) watchSubnets() {
	subnets := make(map[string]*osapi.HostSubnet)
	RunEventQueue(node.osClient, HostSubnets, func(delta cache.Delta) error {
		hs := delta.Object.(*osapi.HostSubnet)
		if hs.HostIP == node.localIP {
			return nil
		}

		log.V(5).Infof("Watch %s event for HostSubnet %q", delta.Type, hs.ObjectMeta.Name)
		switch delta.Type {
		case cache.Sync, cache.Added, cache.Updated:
			oldSubnet, exists := subnets[string(hs.UID)]
			if exists {
				if oldSubnet.HostIP == hs.HostIP {
					break
				} else {
					// Delete old subnet rules
					if err := node.DeleteHostSubnetRules(oldSubnet); err != nil {
						return err
					}
				}
			}
			if err := node.networkInfo.validateNodeIP(hs.HostIP); err != nil {
				log.Warningf("Ignoring invalid subnet for node %s: %v", hs.HostIP, err)
				break
			}

			if err := node.AddHostSubnetRules(hs); err != nil {
				return err
			}
			subnets[string(hs.UID)] = hs
		case cache.Deleted:
			delete(subnets, string(hs.UID))
			if err := node.DeleteHostSubnetRules(hs); err != nil {
				return err
			}
		}
		return nil
	})
}
