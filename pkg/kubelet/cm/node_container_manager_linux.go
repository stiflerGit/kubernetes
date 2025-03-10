// +build linux

/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cm

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/stats/pidlimit"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

const (
	defaultNodeAllocatableCgroupName = "kubepods"
)

//createNodeAllocatableCgroups creates Node Allocatable Cgroup when CgroupsPerQOS flag is specified as true
func (cm *containerManagerImpl) createNodeAllocatableCgroups() error {
	cgroupConfig := &CgroupConfig{
		Name: cm.cgroupRoot,
		// The default limits for cpu shares can be very low which can lead to CPU starvation for pods.
		ResourceParameters: getCgroupConfig(cm.internalCapacity),
	}
	if cm.cgroupManager.Exists(cgroupConfig.Name) {
		return nil
	}

	if err := cm.cgroupManager.Create(cgroupConfig); err != nil {
		klog.Errorf("Failed to create %q cgroup", cm.cgroupRoot)
		return err
	}
	return nil
}

// enforceNodeAllocatableCgroups enforce Node Allocatable Cgroup settings.
func (cm *containerManagerImpl) enforceNodeAllocatableCgroups() error {
	nc := cm.NodeConfig.NodeAllocatableConfig

	// We need to update limits on node allocatable cgroup no matter what because
	// default cpu shares on cgroups are low and can cause cpu starvation.
	nodeAllocatable := cm.internalCapacity
	// Use Node Allocatable limits instead of capacity if the user requested enforcing node allocatable.
	if cm.CgroupsPerQOS && nc.EnforceNodeAllocatable.Has(kubetypes.NodeAllocatableEnforcementKey) {
		nodeAllocatable = cm.getNodeAllocatableInternalAbsolute()
	}

	klog.V(4).Infof("Attempting to enforce Node Allocatable with config: %+v", nc)

	cgroupConfig := &CgroupConfig{
		Name:               cm.cgroupRoot,
		ResourceParameters: getCgroupConfig(nodeAllocatable),
	}

	// Using ObjectReference for events as the node maybe not cached; refer to #42701 for detail.
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      cm.nodeInfo.Name,
		UID:       types.UID(cm.nodeInfo.Name),
		Namespace: "",
	}

	// If Node Allocatable is enforced on a node that has not been drained or is updated on an existing node to a lower value,
	// existing memory usage across pods might be higher than current Node Allocatable Memory Limits.
	// Pod Evictions are expected to bring down memory usage to below Node Allocatable limits.
	// Until evictions happen retry cgroup updates.
	// Update limits on non root cgroup-root to be safe since the default limits for CPU can be too low.
	// Check if cgroupRoot is set to a non-empty value (empty would be the root container)
	if len(cm.cgroupRoot) > 0 {
		go func() {
			for {
				err := cm.cgroupManager.Update(cgroupConfig)
				if err == nil {
					cm.recorder.Event(nodeRef, v1.EventTypeNormal, events.SuccessfulNodeAllocatableEnforcement, "Updated Node Allocatable limit across pods")
					return
				}
				message := fmt.Sprintf("Failed to update Node Allocatable Limits %q: %v", cm.cgroupRoot, err)
				cm.recorder.Event(nodeRef, v1.EventTypeWarning, events.FailedNodeAllocatableEnforcement, message)
				time.Sleep(time.Minute)
			}
		}()
	}
	// Now apply kube reserved and system reserved limits if required.
	if nc.EnforceNodeAllocatable.Has(kubetypes.SystemReservedEnforcementKey) {
		klog.V(2).Infof("Enforcing System reserved on cgroup %q with limits: %+v", nc.SystemReservedCgroupName, nc.SystemReserved)
		if err := enforceExistingCgroup(cm.cgroupManager, cm.cgroupManager.CgroupName(nc.SystemReservedCgroupName), nc.SystemReserved); err != nil {
			message := fmt.Sprintf("Failed to enforce System Reserved Cgroup Limits on %q: %v", nc.SystemReservedCgroupName, err)
			cm.recorder.Event(nodeRef, v1.EventTypeWarning, events.FailedNodeAllocatableEnforcement, message)
			return fmt.Errorf(message)
		}
		cm.recorder.Eventf(nodeRef, v1.EventTypeNormal, events.SuccessfulNodeAllocatableEnforcement, "Updated limits on system reserved cgroup %v", nc.SystemReservedCgroupName)
	}
	if nc.EnforceNodeAllocatable.Has(kubetypes.KubeReservedEnforcementKey) {
		klog.V(2).Infof("Enforcing kube reserved on cgroup %q with limits: %+v", nc.KubeReservedCgroupName, nc.KubeReserved)
		if err := enforceExistingCgroup(cm.cgroupManager, cm.cgroupManager.CgroupName(nc.KubeReservedCgroupName), nc.KubeReserved); err != nil {
			message := fmt.Sprintf("Failed to enforce Kube Reserved Cgroup Limits on %q: %v", nc.KubeReservedCgroupName, err)
			cm.recorder.Event(nodeRef, v1.EventTypeWarning, events.FailedNodeAllocatableEnforcement, message)
			return fmt.Errorf(message)
		}
		cm.recorder.Eventf(nodeRef, v1.EventTypeNormal, events.SuccessfulNodeAllocatableEnforcement, "Updated limits on kube reserved cgroup %v", nc.KubeReservedCgroupName)
	}
	return nil
}

// enforceExistingCgroup updates the limits `rl` on existing cgroup `cName` using `cgroupManager` interface.
func enforceExistingCgroup(cgroupManager CgroupManager, cName CgroupName, rl v1.ResourceList) error {
	cgroupConfig := &CgroupConfig{
		Name:               cName,
		ResourceParameters: getCgroupConfig(rl),
	}
	if cgroupConfig.ResourceParameters == nil {
		return fmt.Errorf("%q cgroup is not config properly", cgroupConfig.Name)
	}
	klog.V(4).Infof("Enforcing limits on cgroup %q with %d cpu shares, %d bytes of memory, and %d processes", cName, cgroupConfig.ResourceParameters.CpuShares, cgroupConfig.ResourceParameters.Memory, cgroupConfig.ResourceParameters.PidsLimit)
	if !cgroupManager.Exists(cgroupConfig.Name) {
		return fmt.Errorf("%q cgroup does not exist", cgroupConfig.Name)
	}
	if err := cgroupManager.Update(cgroupConfig); err != nil {
		return err
	}
	return nil
}

// getCgroupConfig returns a ResourceConfig object that can be used to create or update cgroups via CgroupManager interface.
func getCgroupConfig(rl v1.ResourceList) *ResourceConfig {
	// TODO(vishh): Set CPU Quota if necessary.
	if rl == nil {
		return nil
	}
	var rc ResourceConfig
	if q, exists := rl[v1.ResourceMemory]; exists {
		// Memory is defined in bytes.
		val := q.Value()
		rc.Memory = &val
	}
	if q, exists := rl[v1.ResourceCPU]; exists {
		// CPU is defined in milli-cores.
		val := MilliCPUToShares(q.MilliValue())
		rc.CpuShares = &val
	}
	if q, exists := rl[v1.ResourceRtPeriod]; exists {
		val := uint64(q.Value())
		rc.CpuRtPeriod = &val
	}
	if q, exists := rl[v1.ResourceRtRuntime]; exists {
		val := q.Value()
		rc.CpuRtRuntime = &val
	}
	if q, exists := rl[pidlimit.PIDs]; exists {
		val := q.Value()
		rc.PidsLimit = &val
	}
	rc.HugePageLimit = HugePageLimits(rl)

	return &rc
}

// getNodeAllocatableAbsolute returns the absolute value of Node Allocatable which is primarily useful for enforcement.
// Note that not all resources that are available on the node are included in the returned list of resources.
// Returns a ResourceList.
func (cm *containerManagerImpl) getNodeAllocatableAbsolute() v1.ResourceList {
	return cm.getNodeAllocatableAbsoluteImpl(cm.capacity)
}

func (cm *containerManagerImpl) getNodeAllocatableAbsoluteImpl(capacity v1.ResourceList) v1.ResourceList {
	result := make(v1.ResourceList)
	for k, v := range capacity {
		value := v.DeepCopy()
		if cm.NodeConfig.SystemReserved != nil {
			value.Sub(cm.NodeConfig.SystemReserved[k])
		}
		if cm.NodeConfig.KubeReserved != nil {
			value.Sub(cm.NodeConfig.KubeReserved[k])
		}
		if value.Sign() < 0 {
			// Negative Allocatable resources don't make sense.
			value.Set(0)
		}
		result[k] = value
	}
	// TODO(stefano.fiori): subtract system and kube reserved rt quantities ??
	result[v1.ResourceRtPeriod] = *resource.NewQuantity(cm.CpuRtPeriod.Microseconds(), resource.DecimalSI)
	result[v1.ResourceRtRuntime] = *resource.NewQuantity(cm.CpuRtRuntime.Microseconds(), resource.DecimalSI)

	return result
}

// getNodeAllocatableInternalAbsolute is similar to getNodeAllocatableAbsolute except that
// it also includes internal resources (currently process IDs).  It is intended for setting
// up top level cgroups only.
func (cm *containerManagerImpl) getNodeAllocatableInternalAbsolute() v1.ResourceList {
	return cm.getNodeAllocatableAbsoluteImpl(cm.internalCapacity)
}

// GetNodeAllocatableReservation returns amount of compute or storage resource that have to be reserved on this node from scheduling.
func (cm *containerManagerImpl) GetNodeAllocatableReservation() v1.ResourceList {
	evictionReservation := hardEvictionReservation(cm.HardEvictionThresholds, cm.capacity)
	result := make(v1.ResourceList)
	for k := range cm.capacity {
		value := resource.NewQuantity(0, resource.DecimalSI)
		if cm.NodeConfig.SystemReserved != nil {
			value.Add(cm.NodeConfig.SystemReserved[k])
		}
		if cm.NodeConfig.KubeReserved != nil {
			value.Add(cm.NodeConfig.KubeReserved[k])
		}
		if evictionReservation != nil {
			value.Add(evictionReservation[k])
		}
		if !value.IsZero() {
			result[k] = *value
		}
	}
	return result
}

// validateNodeAllocatable ensures that the user specified Node Allocatable Configuration doesn't reserve more than the node capacity.
// Returns error if the configuration is invalid, nil otherwise.
func (cm *containerManagerImpl) validateNodeAllocatable() error {
	var errors []string
	nar := cm.GetNodeAllocatableReservation()
	for k, v := range nar {
		value := cm.capacity[k].DeepCopy()
		value.Sub(v)

		if value.Sign() < 0 {
			errors = append(errors, fmt.Sprintf("Resource %q has an allocatable of %v, capacity of %v", k, v, value))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("invalid Node Allocatable configuration. %s", strings.Join(errors, " "))
	}
	return nil
}
