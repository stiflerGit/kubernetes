package cpumanager

import (
	"fmt"
	"sort"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
)

// PolicyStatic is the name of the static policy
const PolicyRealTime policyName = "real-time"

type RtState interface {
	state.State
	GetRtCPUSetAndUtilOfContainer(containerID string) (cpuset.CPUSet, float64, bool)
	SetRtCPUSetAndUtilOfContainer(containerID string, set cpuset.CPUSet, util float64)
	CpuToUtilMap() map[int]float64
}

//
type realTimePolicy struct {
	topology *topology.CPUTopology
	// allocable utilization
	allocableRtUtil float64
	// number of reserved cpus
	numReservedCpus int
	// unassignable cpus
	reservedCpus cpuset.CPUSet
}

// Ensure realTimePolicy implements Policy interface
var _ Policy = &realTimePolicy{}

//
func NewRealTimePolicy(topology *topology.CPUTopology, numReservedCPUs int, reservedCPUs cpuset.CPUSet, allocableRtUtil float64) Policy {
	allCPUs := topology.CPUDetails.CPUs()
	var reserved cpuset.CPUSet
	if reservedCPUs.Size() > 0 {
		reserved = reservedCPUs
	} else {
		// takeByTopology allocates CPUs associated with low-numbered cores from
		// allCPUs.
		//
		// For example: Given a system with 8 CPUs available and HT enabled,
		// if numReservedCPUs=2, then reserved={0,4}
		reserved, _ = takeByTopology(topology, allCPUs, numReservedCPUs)
	}

	if reserved.Size() != numReservedCPUs {
		panic(fmt.Sprintf("[cpumanager] unable to reserve the required amount of CPUs (size of %s did not equal %d)", reserved, numReservedCPUs))
	}

	return &realTimePolicy{
		topology:        topology,
		numReservedCpus: numReservedCPUs,
		reservedCpus:    reservedCPUs,
		allocableRtUtil: allocableRtUtil,
	}
}

func (p realTimePolicy) Name() string {
	return string(PolicyRealTime)
}

func (p *realTimePolicy) Start(s state.State) {
	if err := p.initState(s); err != nil {
		klog.Errorf("[cpumanager] real-time policy invalid state: %s\n", err.Error())
		panic("[cpumanager] - please drain node and remove policy state file")
	}
}

func (p *realTimePolicy) initState(s state.State) error {
	assignments := s.GetCPUAssignments()

	allCPUs := p.topology.CPUDetails.CPUs()
	s.SetDefaultCPUSet(allCPUs)

	for containerID := range assignments {
		s.Delete(containerID)
	}

	return nil
}

func (p *realTimePolicy) AddContainer(s state.State, pod *v1.Pod, container *v1.Container, containerID string) error {

	rtState := s.(RtState)

	reqPeriod, reqRuntime, reqCpus := rtRequests(container)
	reqUtil := float64(0)
	if reqPeriod != 0 {
		reqUtil = float64(reqRuntime) / float64(reqPeriod)
	}

	if reqUtil == 0 {
		// no cpu management
		return nil
	}

	if _, _, ok := rtState.GetRtCPUSetAndUtilOfContainer(containerID); ok {
		klog.Infof("[cpumanager] real-time policy: container already assigned to cpus, skipping (container: %s, container id: %s)", container.Name, containerID)
		return nil
	}

	cpus := p.worstFit(rtState.CpuToUtilMap(), reqUtil, reqCpus)
	if int64(len(cpus)) < reqCpus {
		err := fmt.Errorf("container %s doesn't fit", containerID)
		klog.Errorf("[cpumanager] unable to allocate %d CPUs (container id: %s, error: %v)", reqCpus, containerID, err)
		return err
	}
	fittingCpusSet := cpuset.NewCPUSet(cpus...)

	rtState.SetRtCPUSetAndUtilOfContainer(containerID, fittingCpusSet, reqUtil)

	return nil
}

func (p *realTimePolicy) RemoveContainer(s state.State, containerID string) error {
	klog.Infof("[cpumanager] real-time policy: RemoveContainer (container id: %s)", containerID)
	rtState := s.(RtState)

	_, _, ok := rtState.GetRtCPUSetAndUtilOfContainer(containerID)
	if !ok {
		// container not assigned by real-time policy
		return nil
	}

	s.Delete(containerID)

	return nil
}

func (p realTimePolicy) GetTopologyHints(s state.State, pod v1.Pod, container v1.Container) map[string][]topologymanager.TopologyHint {
	panic("implement me")
}

// firstFit assign the requests to the first admittable cpus it find
func (p *realTimePolicy) firstFit(cpuToUtil map[int]float64, reqUtil float64, reqCpus int64) []int {
	var fittingCpus []int
	for cpu, util := range cpuToUtil {
		if util+reqUtil < p.allocableRtUtil {
			fittingCpus = append(fittingCpus, cpu)
			if int64(len(fittingCpus)) == reqCpus {
				break
			}
		}
	}

	if int64(len(fittingCpus)) < reqCpus {
		return nil
	}

	return fittingCpus
}

// worstFit assign the requests to the most free cpus.
func (p *realTimePolicy) worstFit(cpuToUtil map[int]float64, reqUtil float64, reqCpus int64) []int {
	type scoredCpu struct {
		cpu   int
		score float64
	}

	var scoredCpus []scoredCpu
	for cpu, util := range cpuToUtil {
		score := p.allocableRtUtil - util - reqUtil
		if score > 0 {
			scoredCpus = append(scoredCpus, scoredCpu{
				cpu:   cpu,
				score: score,
			})
		}
	}

	if int64(len(scoredCpus)) < reqCpus {
		return nil
	}

	sort.SliceStable(scoredCpus, func(i, j int) bool {
		if scoredCpus[i].score > scoredCpus[j].score {
			return true
		}
		return false
	})

	var fittingCpus []int
	for i := int64(0); i < reqCpus; i++ {
		fittingCpus = append(fittingCpus, scoredCpus[i].cpu)
	}

	return fittingCpus
}

//
func (p *realTimePolicy) bestFit(cpuToUtil map[int]float64, reqUtil float64, reqCpus int64) []int {
	type scoredCpu struct {
		cpu   int
		score float64
	}

	var scoredCpus []scoredCpu
	for cpu, util := range cpuToUtil {
		score := p.allocableRtUtil - util - reqUtil
		if score > 0 {
			scoredCpus = append(scoredCpus, scoredCpu{
				cpu:   cpu,
				score: score,
			})
		}
	}

	if int64(len(scoredCpus)) < reqCpus {
		return nil
	}

	sort.SliceStable(scoredCpus, func(i, j int) bool {
		if scoredCpus[i].score < scoredCpus[j].score {
			return true
		}
		return false
	})

	var fittingCpus []int
	for i := int64(0); i < reqCpus; i++ {
		fittingCpus = append(fittingCpus, scoredCpus[i].cpu)
	}

	return fittingCpus
}

//
func rtRequests(container *v1.Container) (int64, int64, int64) {
	return container.Resources.Requests.CpuRtPeriod().Value(),
		container.Resources.Requests.CpuRtRuntime().Value(),
		container.Resources.Requests.CpuRt().Value()
}

// assignableCPUs returns the set of unassigned CPUs minus the reserved set.
func (p *realTimePolicy) assignableCPUs(s state.State) cpuset.CPUSet {
	return s.GetDefaultCPUSet().Difference(p.reservedCpus)
}
