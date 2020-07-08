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
	"io/ioutil"
	"k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
)

type InternalContainerLifecycle interface {
	PreStartContainer(pod *v1.Pod, container *v1.Container, containerID string) error
	PreStopContainer(containerID string) error
	PostStopContainer(containerID string) error
}

// Implements InternalContainerLifecycle interface.
type internalContainerLifecycleImpl struct {
	cpuManager      cpumanager.Manager
	topologyManager topologymanager.Manager
	cm              ContainerManager
}

func (i *internalContainerLifecycleImpl) PreStartContainer(pod *v1.Pod, container *v1.Container, containerID string) error {
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CPUManager) {
		err := i.cpuManager.AddContainer(pod, container, containerID)
		if err != nil {
			return err
		}
	}
	_, ok := i.cpuManager.State().GetCPUSet(containerID)
	cpuRtRuntime := container.Resources.Requests.CpuRtRuntime()
	if ok && !cpuRtRuntime.IsZero() {
		if err := i.ensureCpuRtMultiRuntime(pod, container, containerID); err != nil {
			return err
		}
	}
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.TopologyManager) {
		err := i.topologyManager.AddContainer(pod, containerID)
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *internalContainerLifecycleImpl) PreStopContainer(containerID string) error {
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CPUManager) {
		return i.cpuManager.RemoveContainer(containerID)
	}
	return nil
}

func (i *internalContainerLifecycleImpl) PostStopContainer(containerID string) error {
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CPUManager) {
		err := i.cpuManager.RemoveContainer(containerID)
		if err != nil {
			return err
		}
	}
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.TopologyManager) {
		err := i.topologyManager.RemoveContainer(containerID)
		if err != nil {
			return err
		}
	}
	return nil
}

//
func writeCpuRtMultiRuntimeFile(cgroupFs string, cpuSet cpuset.CPUSet, rtRuntime int64) error {
	// TODO(stefano.fiori): can we write with opencontainer approach?
	const (
		CpuRtMultiRuntimeFile = "cpu.rt_multi_runtime_us"
	)

	if cpuSet.IsEmpty() {
		return nil
	}

	if err := os.MkdirAll(cgroupFs, os.ModePerm); err != nil {
		return fmt.Errorf("creating the container cgroupFs %s: %v", cgroupFs, err)
	}

	filePath := filepath.Join(cgroupFs, CpuRtMultiRuntimeFile)
	// BUG: write 0 gives error
	if rtRuntime == 0 {
		rtRuntime = 2
	}

	rtRuntimeStr := strconv.FormatInt(rtRuntime, 10)
	str := cpuSet.String() + " " + rtRuntimeStr

	if err := ioutil.WriteFile(filePath, []byte(str), os.ModePerm); err != nil {
		return fmt.Errorf("writing %s in cpu.rt_multi_runtime_us, path %s: %v", str, filePath, err)
	}
	return nil
}

//
func writeRtFile(cgroupFs string, value int64) error {

	if err := os.MkdirAll(filepath.Dir(cgroupFs), os.ModePerm); err != nil {
		return fmt.Errorf("creating the container cgroupFs %s: %v", cgroupFs, err)
	}

	str := strconv.FormatInt(value, 10)

	if err := ioutil.WriteFile(cgroupFs, []byte(str), os.ModePerm); err != nil {
		return fmt.Errorf("writing %s in cpu.rt_multi_runtime_us, path %s: %v", str, value, err)
	}
	return nil
}

//
func readCpuRtMultiRuntimeFile(cgroupFs string) ([]int64, error) {
	const (
		CpuRtMultiRuntimeFile = "cpu.rt_multi_runtime_us"
	)

	filePath := filepath.Join(cgroupFs, CpuRtMultiRuntimeFile)
	buf, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	runtimeStrings := strings.Split(string(buf), " ")
	runtimeStrings = runtimeStrings[:len(runtimeStrings)-2]

	runtimes := make([]int64, 0, len(runtimeStrings))
	for _, runtimeStr := range runtimeStrings {
		v, err := strconv.ParseInt(runtimeStr, 10, 32)
		if err != nil {
			panic(fmt.Errorf("error parsing runtime %s in file %s: %v", runtimeStr, filePath, err))
		}
		runtimes = append(runtimes, v)
	}
	return runtimes, nil
}

//
func (i *internalContainerLifecycleImpl) ensureCpuRtMultiRuntime(pod *v1.Pod, container *v1.Container, containerID string) error {
	cpuSet, _ := i.cpuManager.State().GetCPUSet(containerID)
	cpuRtRuntime := container.Resources.Requests.CpuRtRuntime()
	cpuRtPeriod := container.Resources.Requests.CpuRtPeriod()

	CpuSubsystemMountPoint, ok := i.cm.GetMountedSubsystems().MountPoints["cpu"]
	if !ok {
		panic("cpu subsystem unmounted")
	}

	pcm := i.cm.NewPodContainerManager()
	_, podCgroupFs := pcm.GetPodContainerName(pod)
	podCgroupFs = filepath.Join(CpuSubsystemMountPoint, podCgroupFs)
	// pod period
	if err := writeRtFile(filepath.Join(podCgroupFs, "cpu.rt_period_us"), cpuRtPeriod.Value()); err != nil {
		return err
	}
	// pod runtime
	if err := writeCpuRtMultiRuntimeFile(podCgroupFs, cpuSet, cpuRtRuntime.Value()); err != nil {
		return err
	}
	// container Cgroup
	containerCgroupfs := filepath.Join(podCgroupFs, containerID)
	// container period
	if err := writeRtFile(filepath.Join(containerCgroupfs, "cpu.rt_period_us"), cpuRtPeriod.Value()); err != nil {
		return err
	}
	//if err := writeRtFile(filepath.Join(containerCgroupfs, "cpu.rt_runtime_us"), cpuRtRuntime.Value()); err != nil {
	//	return err
	//}
	// container runtime
	if err := writeCpuRtMultiRuntimeFile(containerCgroupfs, cpuSet, cpuRtRuntime.Value()); err != nil {
		return err
	}

	return nil
}
