package state

import (
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

type RtState struct {
	State
	containerToUtil map[string]float64
	cpuToUtil       map[int]float64
}

//
func NewRtState(s State) *RtState {
	rts := &RtState{
		State:           s,
		containerToUtil: make(map[string]float64),
	}

	rts.cpuToUtil = make(map[int]float64, s.GetDefaultCPUSet().Size())
	for _, cpu := range s.GetDefaultCPUSet().ToSliceNoSort() {
		rts.cpuToUtil[cpu] = 0
	}

	return rts
}

//
func (s RtState) GetRtCPUSetAndUtilOfContainer(containerID string) (cpuset.CPUSet, float64, bool) {
	cpuSet, ok := s.GetCPUSet(containerID)
	if !ok {
		return cpuset.CPUSet{}, 0, false
	}

	util, ok := s.containerToUtil[containerID]
	if !ok {
		return cpuset.CPUSet{}, 0, false
	}

	return cpuSet, util, true
}

//
func (s *RtState) SetRtCPUSetAndUtilOfContainer(containerID string, set cpuset.CPUSet, util float64) {

	oldUtil, ok := s.containerToUtil[containerID]
	if ok {
		// container was already set, we must first clean
		oldSet, ok := s.GetCPUSet(containerID)
		if !ok {
			panic("found utilization but not cpuset")
		}
		for _, cpu := range oldSet.ToSliceNoSort() {
			s.cpuToUtil[cpu] -= oldUtil
		}
	}

	s.SetCPUSet(containerID, set)
	s.containerToUtil[containerID] = util

	for _, cpu := range set.ToSliceNoSort() {
		s.cpuToUtil[cpu] += util
	}
}

//
func (s *RtState) Delete(containerID string) {

	cpuSet, ok := s.GetCPUSet(containerID)
	if !ok {
		panic("manage this error")
	}

	cpuSet, containerUtil, ok := s.GetRtCPUSetAndUtilOfContainer(containerID)
	if !ok {
		// it wasn't assigned using SetRt
		s.State.Delete(containerID)
		return
	}

	for _, cpu := range cpuSet.ToSliceNoSort() {
		s.cpuToUtil[cpu] -= containerUtil
		if s.cpuToUtil[cpu] < 0 {
			s.cpuToUtil[cpu] = 0
		}
	}
	delete(s.containerToUtil, containerID)

	s.State.Delete(containerID)
}

//
func (s *RtState) CpuToUtilMap() map[int]float64 {
	cpuToUtilMap := make(map[int]float64, len(s.cpuToUtil))
	for key, v := range s.cpuToUtil {
		cpuToUtilMap[key] = v
	}
	return cpuToUtilMap
}

//
func (s *RtState) SetDefaultCPUSet(set cpuset.CPUSet) {
	s.State.SetDefaultCPUSet(set)

	s.cpuToUtil = make(map[int]float64, set.Size())
	for _, cpu := range set.ToSliceNoSort() {
		s.cpuToUtil[cpu] = 0
	}
}
