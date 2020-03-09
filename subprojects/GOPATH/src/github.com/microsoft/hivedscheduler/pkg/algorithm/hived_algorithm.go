// MIT License
//
// Copyright (c) Microsoft Corporation. All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE

package algorithm

import (
	"fmt"
	"github.com/microsoft/hivedscheduler/pkg/api"
	"github.com/microsoft/hivedscheduler/pkg/common"
	"github.com/microsoft/hivedscheduler/pkg/internal"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
	"math"
	"math/rand"
	"strings"
	"sync"
)

// HivedAlgorithm implements an internal.SchedulerAlgorithm. It schedules pods using the algorithm of HiveD.
// Note that the topologyAwareScheduler used in this struct is not another implementation of SchedulerAlgorithm;
// that is a specific algorithm for pod placement, used in intra-VC scheduling and opportunistic pod scheduling.
type HivedAlgorithm struct {
	// scheduler in each VC
	vcSchedulers map[api.VirtualClusterName]intraVCScheduler
	// scheduler for opportunistic pods
	opportunisticSchedulers map[CellChain]*topologyAwareScheduler
	// ChainCellLists of physical cells of each cell chain
	fullCellList map[CellChain]ChainCellList
	// ChainCellLists of free physical cells of each cell chain (used in buddy alloc)
	freeCellList map[CellChain]ChainCellList
	// map each GPU type to all chains that contain this type
	chains map[string][]CellChain
	// map each level in a chain to the specific cell type name
	cellTypes map[CellChain]map[CellLevel]api.CellType
	// all affinity groups that have been allocated cells
	allocatedAffinityGroups map[string]*AlgoAffinityGroup
	// all reserved physical cells (VC -> reservation ID -> cells)
	reservedCells map[api.VirtualClusterName]map[api.ReservationId]*PhysicalCell
	// lock
	algorithmLock sync.RWMutex
}

// NewHivedAlgorithm initializes a HivedAlgorithm from the config file
func NewHivedAlgorithm(sConfig *api.Config) *HivedAlgorithm {
	pcl, gpuNums, gpuTypeToChain, cellLevelToType, nonReservedVcl, reservedVcl, reservedPc := ParseConfig(sConfig)
	h := &HivedAlgorithm{
		vcSchedulers:            make(map[api.VirtualClusterName]intraVCScheduler),
		opportunisticSchedulers: map[CellChain]*topologyAwareScheduler{},
		fullCellList:            pcl,
		freeCellList:            make(map[CellChain]ChainCellList),
		chains:                  gpuTypeToChain,
		cellTypes:               cellLevelToType,
		allocatedAffinityGroups: make(map[string]*AlgoAffinityGroup),
		reservedCells:           reservedPc,
	}
	for vc := range nonReservedVcl {
		// TODO: Support per-VC configurable intra VC scheduling algo.
		h.vcSchedulers[vc] = newDefaultIntraVCScheduler(nonReservedVcl[vc], reservedVcl[vc], gpuNums)
	}
	for chain, ccl := range h.fullCellList {
		h.opportunisticSchedulers[chain] = NewTopologyAwareScheduler(ccl, gpuNums[chain], false, true)
	}
	h.validateInitialAssignment()
	h.initFreeCellList()
	h.initReservations()
	return h
}

func (h *HivedAlgorithm) AddNode(node *core.Node) {
	// TODO
}

func (h *HivedAlgorithm) UpdateNode(oldNode, newNode *core.Node) {
	// TODO
}

func (h *HivedAlgorithm) DeleteNode(node *core.Node) {
	// TODO
}

func (h *HivedAlgorithm) Schedule(pod *core.Pod, suggestedNodes []string) internal.PodScheduleResult {
	h.algorithmLock.Lock()
	defer h.algorithmLock.Unlock()

	klog.Infof("[%v]: Scheduling pod...", internal.Key(pod))
	s := internal.ExtractPodSchedulingSpec(pod)
	// gpu number -> a set of pods -> a set of GPUs of each pod
	groupPhysicalPlacement := map[int32][]CellList{}
	groupVirtualPlacement := map[int32][]CellList{}
	podIndex := int32(0)
	suggestedNodeSet := common.NewSet()
	for _, n := range suggestedNodes {
		suggestedNodeSet.Add(n)
	}

	group := h.allocatedAffinityGroups[s.AffinityGroup.Name]
	if group == nil {
		klog.Infof("[%v]: Scheduling new affinity group %v", internal.Key(pod), s.AffinityGroup.Name)
		groupPhysicalPlacement, groupVirtualPlacement = h.scheduleNewAffinityGroup(pod, s, suggestedNodeSet)
	} else {
		klog.Infof("[%v]: Pod from existing affinity group: %v", internal.Key(pod), s.AffinityGroup.Name)
		groupPhysicalPlacement = group.physicalGpuPlacement
		groupVirtualPlacement = group.virtualGpuPlacement
		podIndex = -1
		for i, p := range group.allocatedPods[s.GpuNumber] {
			if p == nil {
				podIndex = int32(i)
				break
			}
		}
		if podIndex == -1 {
			panic(internal.NewBadRequestError(fmt.Sprintf(
				"Requesting more pods than the configured number for %v GPUs (%v pods) in affinity group %v",
				s.GpuNumber, group.totalPodNums[s.GpuNumber], s.AffinityGroup.Name)))
		}
	}
	return generatePodScheduleResult(
		groupPhysicalPlacement,
		groupVirtualPlacement,
		CellPriority(s.Priority),
		h.cellTypes,
		s.GpuNumber,
		podIndex,
		group,
		s.AffinityGroup.Name,
		suggestedNodeSet,
		s.VirtualCluster,
		pod)
}

func (h *HivedAlgorithm) AddAllocatedPod(pod *core.Pod) {
	h.algorithmLock.Lock()
	defer h.algorithmLock.Unlock()

	klog.Infof("[%v]: adding allocated pod...", internal.Key(pod))
	s := internal.ExtractPodSchedulingSpec(pod)
	info := internal.ExtractPodBindInfo(pod)
	klog.Infof("[%v]: adding to node %v, GPUs %v", internal.Key(pod), info.Node, info.GpuIsolation)

	podIndex := int32(0)
	if group := h.allocatedAffinityGroups[s.AffinityGroup.Name]; group == nil {
		h.createAllocatedAffinityGroup(pod, s, info)
	} else {
		for _, gms := range info.AffinityGroupBindInfo {
			if gpuNumber := int32(len(gms.PodPlacements[0].PhysicalGpuIndices)); gpuNumber == s.GpuNumber {
				podIndex = getPodIndex(gms.PodPlacements, info.Node, info.GpuIsolation[0])
				if podIndex == -1 {
					klog.Errorf("[%v]: pod placement not found in group %v: node %v, GPUs %v",
						internal.Key(pod), s.AffinityGroup.Name, info.Node, info.GpuIsolation)
					return
				}
				// if this pod was previously added, then deleted, and we are now re-adding it,
				// we should return the resources to the pod (i.e., re-execute h.confirmAllocatedGpu)
				for gpuIndex := int32(0); gpuIndex < int32(
					len(gms.PodPlacements[podIndex].PhysicalGpuIndices)); gpuIndex++ {
					pGpu, vGpu, _ := h.findAllocatedGpu(
						gpuIndex,
						gms.PodPlacements[podIndex].PhysicalGpuIndices,
						gms.PodPlacements[podIndex].PreassignedCellTypes,
						CellChain(info.CellChain), info.Node, false, s, group, pod)
					if pGpu == nil {
						break
					} else if pGpu.GetAffinityGroup() == nil {
						if vGpu != nil && vGpu.GetPhysicalCell() != nil {
							groupToPreempt := vGpu.GetPhysicalCell().GetAffinityGroup()
							h.lazyPreemptAffinityGroup(groupToPreempt, group.name)
						}
						h.confirmAllocatedGpu(pGpu, vGpu, CellPriority(s.Priority), group)
					}
				}
			}
			break
		}
	}
	h.allocatedAffinityGroups[s.AffinityGroup.Name].allocatedPods[s.GpuNumber][podIndex] = pod
}

func (h *HivedAlgorithm) DeleteAllocatedPod(pod *core.Pod) {
	h.algorithmLock.Lock()
	defer h.algorithmLock.Unlock()

	klog.Infof("[%v]: deleting allocated pod...", internal.Key(pod))
	s := internal.ExtractPodSchedulingSpec(pod)
	info := internal.ExtractPodBindInfo(pod)
	klog.Infof("[%v]: deleting from node %v, GPUs %v", internal.Key(pod), info.Node, info.GpuIsolation)

	if group := h.allocatedAffinityGroups[s.AffinityGroup.Name]; group == nil {
		klog.Errorf("[%v]: group %v not found when deleting pod", internal.Key(pod), s.AffinityGroup.Name)
		return
	} else {
		var podIndex int32
		for _, gms := range info.AffinityGroupBindInfo {
			if gpuNumber := int32(len(gms.PodPlacements[0].PhysicalGpuIndices)); gpuNumber == s.GpuNumber {
				podIndex = getPodIndex(gms.PodPlacements, info.Node, info.GpuIsolation[0])
				if podIndex == -1 {
					klog.Errorf("[%v]: pod placement not found in group %v: node %v, GPUs %v",
						internal.Key(pod), s.AffinityGroup.Name, info.Node, info.GpuIsolation)
					return
				}
			}
		}
		group.allocatedPods[s.GpuNumber][podIndex] = nil
		if !group.gangReleaseEnable {
			klog.Infof("[%v]: gang release NOT enabled for group %v, releasing resources for this pod",
				internal.Key(pod), s.AffinityGroup.Name)
			for _, gpu := range group.physicalGpuPlacement[s.GpuNumber][podIndex] {
				if gpu != nil {
					h.confirmReleasedGpu(gpu.(*PhysicalCell), group)
				}
			}
		}

		if allPodsReleased(group.allocatedPods) {
			if group.gangReleaseEnable {
				klog.Infof("[%v]: gang release enabled for group %v, releasing resources for all the pods",
					internal.Key(pod), s.AffinityGroup.Name)
				for _, podPlacements := range group.physicalGpuPlacement {
					for _, podPlacement := range podPlacements {
						for _, gpu := range podPlacement {
							if gpu != nil {
								h.confirmReleasedGpu(gpu.(*PhysicalCell), group)
							}
						}
					}
				}
			}
			delete(h.allocatedAffinityGroups, s.AffinityGroup.Name)
			klog.Infof("[%v]: All pods complete, affinity group deleted: %v", internal.Key(pod), s.AffinityGroup.Name)
		}
	}
}

func (h *HivedAlgorithm) GetAffinityGroups() api.AffinityGroupList {
	h.algorithmLock.RLock()
	defer h.algorithmLock.RUnlock()

	ags := api.AffinityGroupList{}
	for _, aag := range h.allocatedAffinityGroups {
		ags.Items = append(ags.Items, aag.ToAffinityGroup())
	}

	return ags
}

func (h *HivedAlgorithm) GetAffinityGroup(name string) api.AffinityGroup {
	h.algorithmLock.RLock()
	defer h.algorithmLock.RUnlock()

	if aag := h.allocatedAffinityGroups[name]; aag != nil {
		return aag.ToAffinityGroup()
	}

	panic(internal.NewBadRequestError(fmt.Sprintf(
		"Affinity group %v does not exist since it is not allocated",
		name)))
}

// validateInitialAssignment makes sure that the initial cell assignments
// to all VCs can be fit into the configured physical cells.
func (h *HivedAlgorithm) validateInitialAssignment() {
	totalQuota := map[CellChain]map[CellLevel]int32{}
	for vc, vcs := range h.vcSchedulers {
		for chain, ccl := range vcs.getNonReservedCellList() {
			if totalQuota[chain] == nil {
				totalQuota[chain] = map[CellLevel]int32{}
			}
			l := CellLevel(len(ccl))
			totalQuota[chain][l] += int32(len(ccl[l]))
		}
		for _, reserved := range h.reservedCells[vc] {
			reservedChain := reserved.GetChain()
			if totalQuota[reservedChain] == nil {
				totalQuota[reservedChain] = map[CellLevel]int32{}
			}
			totalQuota[reservedChain][reserved.GetLevel()]++
		}
	}
	for chain, chainQuota := range totalQuota {
		if ccl := h.fullCellList[chain]; ccl == nil {
			panic(fmt.Sprintf(
				"Chain %v does not exists in physical cluster", chain))
		} else {
			top := CellLevel(len(ccl))
			available := int32(len(ccl[top]))
			for l := top; l >= lowestLevel; l-- {
				left := available - chainQuota[l]
				if left < 0 {
					panic(fmt.Sprintf(
						"Insufficient physical cells at chain %v level %v: %v needed, %v available",
						chain, l, chainQuota[l], available))
				}
				if l > lowestLevel {
					available = left * int32(len(ccl[l][0].GetChildren()))
				}
			}
		}
	}
}

// initFreeCellList initializes the free cell list (i.e., top level cells in each chain).
func (h *HivedAlgorithm) initFreeCellList() {
	for chain, ccl := range h.fullCellList {
		topLevel := CellLevel(len(ccl))
		h.freeCellList[chain] = NewChainCellList(topLevel - 1)
		h.freeCellList[chain][topLevel] = make(CellList, len(ccl[topLevel]))
		copy(h.freeCellList[chain][topLevel], ccl[topLevel])
	}
}

// initReservations creates static bindings for the reserved cells, and removes the
// reserved physical cells from the free cell list.
func (h *HivedAlgorithm) initReservations() {
	for vc, vcReservation := range h.reservedCells {
		for rid, physical := range vcReservation {
			h.removeCellFromFreeList(physical)
			virtualList := h.vcSchedulers[vc].getReservedCellList()[rid]
			virtual := virtualList[CellLevel(len(virtualList))][0].(*VirtualCell)
			virtual.SetPhysicalCell(physical)
			physical.SetVirtualCell(virtual)
			klog.Infof("Cells bound: %v and %v (reservation)", virtual.GetName(), physical.GetName())
		}
	}
}

// scheduleNewAffinityGroup schedules each pod of a new affinity group to a set of GPUs
// (in both the physical cluster and the VC).
func (h *HivedAlgorithm) scheduleNewAffinityGroup(
	pod *core.Pod,
	s *api.PodSchedulingSpec,
	suggestedNodeSet common.Set) (map[int32][]CellList, map[int32][]CellList) {

	var (
		physicalPlacement map[int32][]CellList
		virtualPlacement  map[int32][]CellList
		priority          CellPriority
	)

	priority = CellPriority(s.Priority)
	sr := schedulingRequest{
		vc:                   s.VirtualCluster,
		reservationId:        s.ReservationId,
		priority:             priority,
		affinityGroupName:    s.AffinityGroup.Name,
		affinityGroupPodNums: map[int32]int32{},
	}
	for _, m := range s.AffinityGroup.Members {
		// we will merge group members with same GPU number
		sr.affinityGroupPodNums[m.GpuNumber] += m.PodNumber
	}
	h.validateSchedulingRequest(sr, pod)
	if sr.reservationId != "" {
		klog.Infof("Use reservation %v", s.ReservationId)
		sr.chain = h.reservedCells[sr.vc][sr.reservationId].GetChain()
		physicalPlacement, virtualPlacement = h.processSchedulingRequest(sr, suggestedNodeSet)
	} else {
		physicalPlacement, virtualPlacement = h.scheduleAffinityGroupForGpuType(sr, s.GpuType, pod, suggestedNodeSet)
	}
	if physicalPlacement != nil {
		klog.Infof("Succeeded in scheduling group %v", s.AffinityGroup.Name)
	} else {
		klog.Infof("Failed to schedule group %v", s.AffinityGroup.Name)
	}
	return physicalPlacement, virtualPlacement
}

// scheduleAffinityGroupForGpuType schedules an affinity group in a certain cell chain.
// If a GPU type is specified, it will be scheduled to a chain that contains this GPU type.
// Otherwise any GPU type will be tried.
func (h *HivedAlgorithm) scheduleAffinityGroupForGpuType(
	sr schedulingRequest,
	gpuType string,
	pod *core.Pod,
	suggestedNodeSet common.Set) (map[int32][]CellList, map[int32][]CellList) {

	if gpuType != "" {
		if chains := h.chains[gpuType]; chains == nil {
			panic(internal.NewBadRequestError(fmt.Sprintf(
				"[%v]: pod requesting GPU type %v which the whole cluster does not have",
				internal.Key(pod), gpuType)))
		} else {
			vcHasType := false
			for _, chain := range chains {
				if h.vcSchedulers[sr.vc].getNonReservedCellList()[chain] != nil {
					vcHasType = true
				}
				sr.chain = chain
				if physicalPlacement, virtualPlacement := h.processSchedulingRequest(sr, suggestedNodeSet); physicalPlacement != nil {
					return physicalPlacement, virtualPlacement
				}
			}
			if sr.priority >= minGuaranteedPriority && !vcHasType {
				panic(internal.NewBadRequestError(fmt.Sprintf(
					"[%v]: pod requesting GPU type %v which VC %v does not have",
					internal.Key(pod), gpuType, sr.vc)))
			}
		}
	} else {
		for _, chains := range h.chains {
			for _, chain := range chains {
				sr.chain = chain
				if physicalPlacement, virtualPlacement := h.processSchedulingRequest(sr, suggestedNodeSet); physicalPlacement != nil {
					return physicalPlacement, virtualPlacement
				}
			}
		}
	}
	return nil, nil
}

// validateSchedulingRequest checks the existence of VC and reservation ID, and the legality of priority.
func (h *HivedAlgorithm) validateSchedulingRequest(sr schedulingRequest, pod *core.Pod) {
	var message string
	if h.vcSchedulers[sr.vc] == nil {
		message = fmt.Sprintf("VC %v does not exists!", sr.vc)
	} else if sr.reservationId != "" {
		if h.vcSchedulers[sr.vc].getReservedCellList()[sr.reservationId] == nil {
			message = fmt.Sprintf("VC %v does not have reservation %v", sr.vc, sr.reservationId)
		} else if sr.priority == opportunisticPriority {
			message = fmt.Sprintf("opportunistic pod not supported to use reservation %v", sr.reservationId)
		}
	}
	if message != "" {
		panic(internal.NewBadRequestError(fmt.Sprintf("[%v]: %v", internal.Key(pod), message)))
	}
}

// processSchedulingRequest feeds a request to a VC scheduler
// or the opportunistic scheduler according to its priority.
func (h *HivedAlgorithm) processSchedulingRequest(
	sr schedulingRequest,
	suggestedNodeSet common.Set) (map[int32][]CellList, map[int32][]CellList) {

	if sr.priority >= minGuaranteedPriority {
		return h.scheduleGuaranteedAffinityGroup(sr, suggestedNodeSet)
	} else {
		return h.scheduleOpportunisticAffinityGroup(sr, suggestedNodeSet), nil
	}
}

// scheduleGuaranteedAffinityGroup schedules an affinity group in its VC, and
// then maps the placement in VC to the physical cluster.
func (h *HivedAlgorithm) scheduleGuaranteedAffinityGroup(
	sr schedulingRequest,
	suggestedNodeSet common.Set) (map[int32][]CellList, map[int32][]CellList) {

	// schedule in VC
	virtualPlacement := h.vcSchedulers[sr.vc].schedule(sr)
	if virtualPlacement == nil {
		return nil, nil
	}
	// map the vc placement to the physical cluster
	gpuNums := make([]int32, len(sr.affinityGroupPodNums))
	i := 0
	for n := range sr.affinityGroupPodNums {
		gpuNums[i] = n
		i++
	}
	common.SortInt32(gpuNums)
	physicalPlacement := map[int32][]CellList{}
	for _, podGpuNum := range gpuNums {
		podPlacements := virtualPlacement[podGpuNum]
		physicalPlacement[podGpuNum] = make([]CellList, len(podPlacements))
		for i, podGpus := range podPlacements {
			physicalPlacement[podGpuNum][i] = make(CellList, len(podGpus))
			for j, gpu := range podGpus {
				vGpu := gpu.(*VirtualCell)
				if vGpu.GetPhysicalCell() != nil {
					if groupToPreempt := vGpu.GetPhysicalCell().GetAffinityGroup(); groupToPreempt.lazyPreemptionEnable {
						h.lazyPreemptAffinityGroup(groupToPreempt, sr.affinityGroupName)
					}
				}
				pac := vGpu.GetPreAssignedCell()
				// check if the preassigned cell has been (temporarily) bound to a physical cell
				preassignedPhysical := pac.GetPhysicalCell()
				if preassignedPhysical == nil {
					preassignedPhysical = pac.GetPreBoundPhysicalCell()
				}
				if preassignedPhysical == nil {
					// allocate a new physical cell to the preassigned cell. input a copy of the free cell list
					// because during the scheduling we should not make in-place change to the data structures
					c := buddyAlloc(h.getTmpFreeCellList(sr.chain), pac.GetLevel(), suggestedNodeSet)
					if c == nil {
						panic(fmt.Sprintf(
							"VC Safety Broken: Cannot find physical cell for a VC cell: %v", pac.GetName()))
					} else {
						preassignedPhysical = c
						// create binding (which is temporary and will be cleared after the scheduling,
						// same reason as above)
						pac.SetPreBoundPhysicalCell(preassignedPhysical)
						preassignedPhysical.SetPreBoundVirtualCell(pac)
					}
				}
				physicalPlacement[podGpuNum][i][j] = mapNonPreassignedCellToPhysical(vGpu, suggestedNodeSet)
			}
		}
	}
	clearPreBindings(virtualPlacement)
	return physicalPlacement, virtualPlacement
}

// scheduleOpportunisticAffinityGroup calls the opportunistic pod scheduler to schedule an affinity group.
func (h *HivedAlgorithm) scheduleOpportunisticAffinityGroup(
	sr schedulingRequest,
	suggestedNodeSet common.Set) map[int32][]CellList {

	placement := h.opportunisticSchedulers[sr.chain].Schedule(
		sr.affinityGroupPodNums, opportunisticPriority, suggestedNodeSet)
	if placement == nil {
		klog.Infof("Insufficient capacity in PC for scheduling request: GPU numbers %v, priority %v",
			sr.affinityGroupPodNums, sr.priority)
	} else {
		klog.Infof("Succeeded in scheduling in PC for scheduling request: GPU numbers %v, priority %v",
			sr.affinityGroupPodNums, sr.priority)
	}
	return placement
}

// getTmpFreeCellList returns a copy of the free cell list.
func (h *HivedAlgorithm) getTmpFreeCellList(chain CellChain) ChainCellList {
	ccl := ChainCellList{}
	for l := CellLevel(1); l <= CellLevel(len(h.freeCellList[chain])); l++ {
		ccl[l] = make(CellList, len(h.freeCellList[chain][l]))
		copy(ccl[l], h.freeCellList[chain][l])
	}
	return ccl
}

// createAllocatedAffinityGroup creates a new affinity group, and confirms the allocated resources.
func (h *HivedAlgorithm) createAllocatedAffinityGroup(pod *core.Pod, s *api.PodSchedulingSpec, info *api.PodBindInfo) {
	newGroup := newAlgoAffinityGroup(s.AffinityGroup, s.GangReleaseEnable, s.LazyPreemptionEnable)
	shouldLazyPreempt := false
	for _, gms := range info.AffinityGroupBindInfo {
		gpuNumber := int32(len(gms.PodPlacements[0].PhysicalGpuIndices))
		for podIndex := int32(0); podIndex < int32(len(gms.PodPlacements)); podIndex++ {
			node := gms.PodPlacements[podIndex].PhysicalNode
			for gpuIndex := int32(0); gpuIndex < int32(
				len(gms.PodPlacements[podIndex].PhysicalGpuIndices)); gpuIndex++ {
				pGpu, vGpu, lazyPreempt := h.findAllocatedGpu(
					gpuIndex,
					gms.PodPlacements[podIndex].PhysicalGpuIndices,
					gms.PodPlacements[podIndex].PreassignedCellTypes,
					CellChain(info.CellChain), node, shouldLazyPreempt, s, newGroup, pod)
				if pGpu == nil {
					break
				} else {
					newGroup.physicalGpuPlacement[gpuNumber][podIndex][gpuIndex] = pGpu
					if lazyPreempt == nil {
						newGroup.virtualGpuPlacement = nil
					} else if vGpu != nil {
						newGroup.virtualGpuPlacement[gpuNumber][podIndex][gpuIndex] = vGpu
						if vGpu.GetPhysicalCell() != nil {
							groupToPreempt := vGpu.GetPhysicalCell().GetAffinityGroup()
							h.lazyPreemptAffinityGroup(groupToPreempt, newGroup.name)
						}
					} else {
						shouldLazyPreempt = shouldLazyPreempt || *lazyPreempt
					}
					h.confirmAllocatedGpu(pGpu, vGpu, CellPriority(s.Priority), newGroup)
				}
			}
		}
	}
	if shouldLazyPreempt {
		h.lazyPreemptAffinityGroup(newGroup, newGroup.name)
	}
	h.allocatedAffinityGroups[s.AffinityGroup.Name] = newGroup
	klog.Infof("[%v]: New affinity group created: %v", internal.Key(pod), s.AffinityGroup.Name)
}

// findAllocatedGpu finds the physical and virtual GPUs in the full cell lists for an allocate pod.
// The boolean return value indicates whether the affinity group should be lazy-preempted.
// The bool being nil means the group is OT and has no virtual placement.
func (h *HivedAlgorithm) findAllocatedGpu(
	index int32,
	physicalGpuIndices []int32,
	preassignedCellTypes []api.CellType,
	chain CellChain,
	node string,
	lazyPreempted bool,
	s *api.PodSchedulingSpec,
	group *AlgoAffinityGroup,
	pod *core.Pod) (*PhysicalCell, *VirtualCell, *bool) {

	priority := CellPriority(s.Priority)
	physicalGpuIndex := physicalGpuIndices[index]
	if pGpu := h.findPhysicalGpu(chain, node, physicalGpuIndex); pGpu == nil {
		klog.Warningf(
			"[%v]: cannot find GPU %v on node %v: not found in the spec. pod ignored",
			internal.Key(pod), physicalGpuIndex, node)
		return nil, nil, common.PtrBool(false)
	} else {
		var vGpu *VirtualCell
		if preassignedCellTypes == nil {
			klog.Warningf("[%v]: cannot find virtual cell: preassigned cell not found in pod bind info", internal.Key(pod))
			return pGpu, nil, common.PtrBool(true)
		}
		if group.virtualGpuPlacement != nil && !lazyPreempted {
			preassignedType := preassignedCellTypes[index]
			if preassignedType != "" {
				var preassignedLevel CellLevel
				typeFound := false
				for l, t := range h.cellTypes[pGpu.GetChain()] {
					if t == preassignedType {
						preassignedLevel = l
						typeFound = true
					}
				}
				var message string
				if !typeFound {
					message = fmt.Sprintf("preassigned cell type %v not found in chain %v", preassignedType, pGpu.GetChain())
				} else if vcs := h.vcSchedulers[s.VirtualCluster]; vcs == nil {
					message = fmt.Sprintf("VC %v not found", s.VirtualCluster)
				} else {
					vccl := vcs.getNonReservedCellList()[pGpu.GetChain()]
					str := string(pGpu.GetChain())
					if s.ReservationId != "" {
						vccl = vcs.getReservedCellList()[s.ReservationId]
						str = string(s.ReservationId)
					}
					if vccl == nil {
						message = fmt.Sprintf("VC %v has no cell for %v", s.VirtualCluster, str)
					} else {
						vGpu, message = mapNonPreassignedCellToVirtual(pGpu, vccl, preassignedLevel, priority)
					}
				}
				if vGpu == nil {
					klog.Warningf("[%v]: cannot find virtual cell: %v", internal.Key(pod), message)
					return pGpu, nil, common.PtrBool(true)
				} else {
					return pGpu, vGpu, common.PtrBool(false)
				}
			} else {
				return pGpu, nil, nil
			}
		} else {
			return pGpu, nil, common.PtrBool(false)
		}
	}
}

// confirmAllocatedGpu creates the cell bindings, removes the physical cell from the free list
// (if necessary), and sets the priority.
func (h *HivedAlgorithm) confirmAllocatedGpu(
	pGpu *PhysicalCell,
	vGpu *VirtualCell,
	p CellPriority,
	g *AlgoAffinityGroup) {

	physicalPriority := p
	if vGpu != nil {
		preassignedNewlyBound := vGpu.GetPreAssignedCell().GetPhysicalCell() == nil
		bindCell(pGpu, vGpu)
		if preassignedNewlyBound {
			// remove the allocated cell from the free list (possibly splitting cells)
			h.removeCellFromFreeList(vGpu.GetPreAssignedCell().GetPhysicalCell())
		}
		setPriority(vGpu, p)
		updateUsedGpuNumAtPriority(vGpu, p, true)
	} else {
		physicalPriority = opportunisticPriority
	}
	setPriority(pGpu, physicalPriority)
	updateUsedGpuNumAtPriority(pGpu, physicalPriority, true)
	pGpu.AddAffinityGroup(g)
}

// getPodIndex finds the index of a pod in its group according to its placement.
func getPodIndex(podPlacements []api.PodPlacementInfo, node string, gpuIndex int32) int32 {
	for podIndex, placement := range podPlacements {
		if placement.PhysicalNode == node && common.Int32SliceContains(placement.PhysicalGpuIndices, gpuIndex) {
			return int32(podIndex)
		}
	}
	return -1
}

// confirmReleasedGpu destroys the cell bindings, adds the physical cell back to the free list
// (if necessary), and resets the priority.
func (h *HivedAlgorithm) confirmReleasedGpu(pGpu *PhysicalCell, g *AlgoAffinityGroup) {
	if vGpu := pGpu.GetVirtualCell(); vGpu != nil {
		preassignedPhysical := vGpu.GetPreAssignedCell().GetPhysicalCell()
		unbindCell(pGpu)
		if vGpu.GetPreAssignedCell().GetPhysicalCell() == nil {
			// add the released cell back to the free list (possibly merging cells)
			h.addCellToFreeList(preassignedPhysical)
		}
		updateUsedGpuNumAtPriority(vGpu, vGpu.GetPriority(), false)
		setPriority(vGpu, freePriority)
	}
	updateUsedGpuNumAtPriority(pGpu, pGpu.GetPriority(), false)
	setPriority(pGpu, freePriority)
	pGpu.DeleteAffinityGroup(g)
}

// lazyPreemptAffinityGroup removes an affinity group from its VC, clears it virtual placement,
// and exposes this decision.
func (h *HivedAlgorithm) lazyPreemptAffinityGroup(
	victim *AlgoAffinityGroup, preemptor string) {

	for _, podVirtualPlacements := range victim.virtualGpuPlacement {
		for _, podVirtualPlacement := range podVirtualPlacements {
			for _, gpu := range podVirtualPlacement {
				if gpu != nil {
					vGpu := gpu.(*VirtualCell)
					pGpu := vGpu.GetPhysicalCell()
					h.confirmReleasedGpu(pGpu, victim)
					h.confirmAllocatedGpu(pGpu, nil, opportunisticPriority, victim)
				}
			}
		}
	}
	victim.virtualGpuPlacement = nil
	victim.lazyPreemptionStatus = &api.LazyPreemptionStatus{
		Preemptor:      preemptor,
		PreemptionTime: meta.Now(),
	}
	klog.Infof("Affinity group %v is lazy preempted from VC by %v", victim.name, preemptor)
}

// removeCellFromFreeList removes a cell from the free cell list and splits its parent recursively if needed.
func (h *HivedAlgorithm) removeCellFromFreeList(c *PhysicalCell) {
	chain := c.GetChain()
	terminate := false
	for {
		l := c.GetLevel()
		parent := c.GetParent()
		if parent != nil {
			pp := parent.(*PhysicalCell)
			if pp.IsSplit() {
				terminate = true
			} else {
				h.freeCellList[chain][l] = append(h.freeCellList[chain][l], pp.GetChildren()...)
				pp.SetSplit(true)
			}
		} else {
			terminate = true
		}
		h.freeCellList[chain].remove(c, l)
		if terminate {
			break
		} else {
			c = parent.(*PhysicalCell)
		}
	}
}

// addCellToFreeList adds a cell to the free cell list and merges its buddies recursively if needed.
func (h *HivedAlgorithm) addCellToFreeList(c *PhysicalCell) {
	chain := c.GetChain()
	terminate := false
	for {
		l := c.GetLevel()
		parent := c.GetParent()
		if parent != nil {
			allBuddyFree := true
			for _, buddy := range parent.GetChildren() {
				if buddy.(*PhysicalCell).GetVirtualCell() != nil {
					allBuddyFree = false
					break
				}
			}
			if !allBuddyFree {
				terminate = true
			} else {
				for _, buddy := range parent.GetChildren() {
					if !CellEqual(buddy, c) {
						h.freeCellList[chain].remove(buddy, l)
					}
				}
				parent.(*PhysicalCell).SetSplit(false)
			}
		} else {
			terminate = true
		}
		if terminate {
			h.freeCellList[chain][l] = append(h.freeCellList[chain][l], c)
			break
		} else {
			c = parent.(*PhysicalCell)
		}
	}
}

// findPhysicalGpu finds a physical GPU cell in the full list. If the GPU is not found in the chain specified
// in the PodBindInfo (due to reconfiguration), we will try to search in the other chains.
func (h *HivedAlgorithm) findPhysicalGpu(
	chain CellChain,
	node string,
	gpuIndex int32) *PhysicalCell {

	if g := h.findPhysicalGpuInChain(chain, node, gpuIndex); g == nil {
		for c := range h.fullCellList {
			if c != chain {
				if g = h.findPhysicalGpuInChain(c, node, gpuIndex); g != nil {
					klog.Warningf("GPU %v on node %v has been moved to chain %v", gpuIndex, node, c)
					return g
				}
			}
		}
		return nil
	} else {
		return g
	}
}

// findPhysicalGpuInChain finds a physical GPU cell in the full list of a given chain. This search is based on
// *one* node and *one* GPU index, assuming there is no resource overlapping among cells at the same level.
func (h *HivedAlgorithm) findPhysicalGpuInChain(
	chain CellChain,
	node string,
	gpuIndex int32) *PhysicalCell {

	for _, c := range h.fullCellList[chain][1] {
		success := false
		cc := c.(*PhysicalCell)
		nodes, gpuIndices := cc.GetPhysicalPlacement()
		for _, n := range nodes {
			if n == node {
				success = true
				break
			}
		}
		if success {
			if gpuIndex < 0 {
				return cc
			} else {
				for _, g := range gpuIndices {
					if g == gpuIndex {
						return cc
					}
				}
			}
		}
	}
	return nil
}

// generatePodScheduleResult writes the scheduling result into a PodScheduleResult.
func generatePodScheduleResult(
	groupPhysicalPlacement map[int32][]CellList,
	groupVirtualPlacement map[int32][]CellList,
	priority CellPriority,
	cellLevelToType map[CellChain]map[CellLevel]api.CellType,
	currentGpuNum int32,
	currentPodIndex int32,
	group *AlgoAffinityGroup,
	groupName string,
	suggestedNodeSet common.Set,
	vc api.VirtualClusterName,
	pod *core.Pod) internal.PodScheduleResult {

	var allSuggestedNodes []string
	for node := range suggestedNodeSet.Items() {
		allSuggestedNodes = append(allSuggestedNodes, node.(string))
	}

	klog.Infof("[SNODED]: All suggested nodes: %v", strings.Join(allSuggestedNodes, ", "))
	klog.Infof("[SNODED]: groupPhysicalPlacement: %v", common.ToJson(groupPhysicalPlacement))
	klog.Infof("[SNODED]: groupVirtualPlacement: %v", common.ToJson(groupVirtualPlacement))

	preemptionVictims, nodesHaveVictims := collectPreemptionVictims(groupPhysicalPlacement, priority, groupName)
	if len(preemptionVictims) > 0 {
		// we collect victims on a random node, as K8S preempts victims from only one node once.
		// random is to let different pods preempt victims on different nodes
		// (note that this randomness is not necessary for the eventual-completeness of preemption).
		nodeToPreempt := nodesHaveVictims[rand.Int31n(int32(len(nodesHaveVictims)))]
		var victimPods []*core.Pod
		var victimNames []string
		for v := range preemptionVictims[nodeToPreempt].Items() {
			victimPods = append(victimPods, v.(*core.Pod))
			victimNames = append(victimNames, internal.Key(v.(*core.Pod)))
		}
		klog.Infof("[SNODED]: [%v]: need to preempt pods %v", internal.Key(pod), strings.Join(victimNames, ", "))
		return internal.PodScheduleResult{
			PodPreemptInfo: &internal.PodPreemptInfo{VictimPods: victimPods},
		}
	} else {
		// we find the selected node after the preemption is done, otherwise the preemption victims
		// may cause the selected node to be excluded from the suggested nodes
		affinityGroupBindInfo, selectedNode, selectedGpuIndices, cellChain, physicalPlacementString, virtualPlacementString := generateAffinityGroupBindInfo(
			groupPhysicalPlacement, groupVirtualPlacement, cellLevelToType, currentGpuNum, currentPodIndex, group, groupName, suggestedNodeSet)
		klog.Infof("[SNODED]: NoPreempt Physical placement: %v", physicalPlacementString)
		klog.Infof("[SNODED]: NoPreempt Virtual placement: %v", virtualPlacementString)
		var waitReason string
		if affinityGroupBindInfo == nil {
			waitReason = "insufficient capacity in physical cluster"
			if priority >= minGuaranteedPriority {
				waitReason = fmt.Sprintf("insufficient quota in VC %v", vc)
			}
		} else if selectedNode == "" {
			waitReason = "cannot find a K8s candidate node within physical cluster"
			if priority >= minGuaranteedPriority {
				waitReason = fmt.Sprintf("cannot find a K8s candidate node within VC %v's quota", vc)
			}
		}
		if waitReason != "" {
			return internal.PodScheduleResult{PodWaitInfo: &internal.PodWaitInfo{Reason: waitReason}}
		}
		klog.Infof("[%v]: scheduled to node %v, GPUs %v",
			internal.Key(pod), selectedNode, selectedGpuIndices)
		return internal.PodScheduleResult{
			PodBindInfo: &api.PodBindInfo{
				Node:                  selectedNode,
				GpuIsolation:          selectedGpuIndices,
				CellChain:             cellChain,
				AffinityGroupBindInfo: affinityGroupBindInfo,
			},
		}
	}
}

// generateAffinityGroupBindInfo writes the physical and virtual placements of an affinity group
// into a a series of AffinityGroupMemberBindInfos, and returns the allocated node and GPU addresses
// of the current pod.
func generateAffinityGroupBindInfo(
	groupPhysicalPlacement map[int32][]CellList,
	groupVirtualPlacement map[int32][]CellList,
	cellLevelToType map[CellChain]map[CellLevel]api.CellType,
	currentGpuNum int32,
	currentPodIndex int32,
	group *AlgoAffinityGroup,
	groupName string,
	suggestedNodeSet common.Set) ([]api.AffinityGroupMemberBindInfo, string, []int32, string, string, string) {

	if groupPhysicalPlacement == nil {
		return nil, "", nil, "", "", ""
	}
	physicalPlacementStrings := map[string][]string{}
	var virtualPlacementStrings []string
	affinityGroupBindInfo := make([]api.AffinityGroupMemberBindInfo, len(groupPhysicalPlacement))
	var selectedNode string
	var selectedGpuIndices []int32
	var chain string
	groupMemberIndex := 0
	for podGpuNum, podPhysicalPlacements := range groupPhysicalPlacement {
		mbi := api.AffinityGroupMemberBindInfo{
			PodPlacements: make([]api.PodPlacementInfo, len(podPhysicalPlacements)),
		}
		for podIndex := int32(0); podIndex < int32(len(podPhysicalPlacements)); podIndex++ {
			mbi.PodPlacements[podIndex].PhysicalGpuIndices = make([]int32, podGpuNum)
			mbi.PodPlacements[podIndex].PreassignedCellTypes = make([]api.CellType, podGpuNum)
			for gpuIndex := int32(0); gpuIndex < podGpuNum; gpuIndex++ {
				pGpu := podPhysicalPlacements[podIndex][gpuIndex]
				if pGpu == nil {
					if group == nil {
						panic(fmt.Sprintf("The first pod in group %v was allocated invalid resource", groupName))
					}
					// if the physical placement of this pod is not found (e.g., removed due to reconfiguration),
					// we will insist the decision by retrieving it from other pods
					mbi.PodPlacements[podIndex], chain = retrieveMissingPodPlacement(group, podGpuNum, podIndex)
					klog.Warningf(
						"pod placement has been invalid and is retrieved from annotation of other pods: node %v, GPU %v",
						mbi.PodPlacements[podIndex].PhysicalNode, mbi.PodPlacements[podIndex].PhysicalGpuIndices[gpuIndex])
				} else {
					nodes, gpuIndices := pGpu.(*PhysicalCell).GetPhysicalPlacement()
					// here each cell (i.e., pGpu) is only one GPU, hence we takes the first element
					// in its "nodes" and "gpuIndices" as the node and GPU address
					if _, ok := physicalPlacementStrings[nodes[0]]; !ok {
						physicalPlacementStrings[nodes[0]] = []string{}
					}
					physicalPlacementStrings[nodes[0]] = append(physicalPlacementStrings[nodes[0]], common.Int32ToString(gpuIndices[0]))
					if mbi.PodPlacements[podIndex].PhysicalNode == "" {
						mbi.PodPlacements[podIndex].PhysicalNode = nodes[0]
					}
					mbi.PodPlacements[podIndex].PhysicalGpuIndices[gpuIndex] = gpuIndices[0]
					if groupVirtualPlacement != nil {
						vGpu := groupVirtualPlacement[podGpuNum][podIndex][gpuIndex].(*VirtualCell)
						mbi.PodPlacements[podIndex].PreassignedCellTypes[gpuIndex] =
							cellLevelToType[vGpu.GetChain()][vGpu.GetPreAssignedCell().GetLevel()]
						virtualPlacementStrings = append(virtualPlacementStrings, vGpu.GetName())
					} else {
						mbi.PodPlacements[podIndex].PreassignedCellTypes[gpuIndex] = ""
					}
				}
			}
		}
		if podGpuNum == currentGpuNum &&
			(group != nil || suggestedNodeSet.Contains(mbi.PodPlacements[currentPodIndex].PhysicalNode)) {
			selectedNode = mbi.PodPlacements[currentPodIndex].PhysicalNode
			selectedGpuIndices = mbi.PodPlacements[currentPodIndex].PhysicalGpuIndices
			if pGpu := groupPhysicalPlacement[currentGpuNum][currentPodIndex][0]; pGpu != nil {
				chain = string(pGpu.GetChain())
			}
		}
		affinityGroupBindInfo[groupMemberIndex] = mbi
		groupMemberIndex++
	}
	var physicalPlacementString string
	for node, gpuIndices := range physicalPlacementStrings {
		physicalPlacementString += fmt.Sprintf("%v: %v, ", node, strings.Join(gpuIndices, ","))
	}
	return affinityGroupBindInfo, selectedNode, selectedGpuIndices, chain, physicalPlacementString, strings.Join(virtualPlacementStrings, ", ")
}

// collectPreemptionVictims collects preemption victims of an affinity group.
// If any of the GPUs allocated for the whole group is still used by a pod,
// we will wait for the preemption, as the group is gang-scheduled.
func collectPreemptionVictims(
	groupPhysicalPlacement map[int32][]CellList,
	priority CellPriority,
	groupName string) (map[string]common.Set, []string) {

	preemptionVictims := map[string]common.Set{}
	var nodesHaveVictims []string
	for gpuNum := range groupPhysicalPlacement {
		for podIndex := range groupPhysicalPlacement[gpuNum] {
			for _, gpu := range groupPhysicalPlacement[gpuNum][podIndex] {
				if gpu == nil {
					continue
				}
				pGpu := gpu.(*PhysicalCell)
				if victimGroup := pGpu.GetAffinityGroup(); victimGroup != nil && victimGroup.name != groupName {
					// there are two cases of finding a running pod on the allocated resources:
					// 1. the running pod is a preemption victim.
					// 2. the running pod used resource partially released by the current group,
					// but the group wants to schedule a pod again.
					// our principle is we allow preemption if the running pod's priority is lower than that
					// of the group to be scheduled (the 2nd case may also satisfy this condition, and we
					// allow such preemption). otherwise the running pod cannot be preempted, and the pod
					// to be scheduled will wait.
					if pGpu.GetPriority() >= priority {
						panic(fmt.Sprintf(
							"Resources previously allocated (%v) has been allocated to "+
								"another non-preemptible group %v; pod should wait",
							pGpu.GetPhysicalPlacementString(), victimGroup.name))
					}
					// for any victim pod, gang-preempt all the other pods from the same affinity group
					for _, victims := range victimGroup.allocatedPods {
						for _, v := range victims {
							if v != nil {
								if _, ok := preemptionVictims[v.Spec.NodeName]; !ok {
									preemptionVictims[v.Spec.NodeName] = common.NewSet()
									nodesHaveVictims = append(nodesHaveVictims, v.Spec.NodeName)
								}
								preemptionVictims[v.Spec.NodeName].Add(v)
							}
						}
					}
				}
			}
		}
	}
	return preemptionVictims, nodesHaveVictims
}

// retrieveMissingPodPlacement finds the placement of a pod from the annotation of other pods in the same group
// when the pod's placement has been invalid (i.e., not found in the spec).
func retrieveMissingPodPlacement(group *AlgoAffinityGroup, gpuNum int32, podIndex int32) (api.PodPlacementInfo, string) {
	for _, pods := range group.allocatedPods {
		for _, p := range pods {
			if p != nil {
				info := internal.ExtractPodBindInfo(p)
				for _, mbi := range info.AffinityGroupBindInfo {
					if gpuNum == int32(len(mbi.PodPlacements[0].PhysicalGpuIndices)) {
						return mbi.PodPlacements[podIndex], info.CellChain
					}
				}
			}
		}
	}
	panic(fmt.Sprintf(
		"No allocated pod found in an allocated group %v when retrieving placement for pod %v with GPU number %v", group.name, podIndex, gpuNum))
}

// buddyAlloc allocates a free cell at a certain level from a free list.
// It splits a higher-level cell when there is no free cell at the current level.
// As the input cell list is a copy of the real free list and hence is one-off,
// we won't remove a returned cell from it.
func buddyAlloc(freeList ChainCellList, level CellLevel, suggestedNodeSet common.Set) *PhysicalCell {
	if len(freeList[level]) == 0 && level < CellLevel(len(freeList)) {
		higherCell := buddyAlloc(freeList, level+1, suggestedNodeSet)
		if higherCell != nil {
			freeList[level] = append(freeList[level], higherCell.GetChildren()...)
		}
	}
	if len(freeList[level]) == 0 {
		return nil
	}
	return getFewestOpporPhysicalCell(freeList[level], suggestedNodeSet)
}

// getFewestOpporPhysicalCell selects a physical cell with the minimum number of opportunistic pods from a cell list.
func getFewestOpporPhysicalCell(cl CellList, suggestedNodeSet common.Set) *PhysicalCell {
	fewestOpporNum := int32(math.MaxInt32)
	fewestOpporNumSuggested := int32(math.MaxInt32)
	var fewestOpporCell *PhysicalCell
	var fewestOpporSuggested *PhysicalCell
	for _, c := range cl {
		if pc := c.(*PhysicalCell); pc.GetVirtualCell() == nil && pc.GetPreBoundVirtualCell() == nil {
			numOppor := pc.GetUsedGpuNumAtPriorities()[opportunisticPriority]
			if numOppor < fewestOpporNum {
				fewestOpporNum = numOppor
				fewestOpporCell = pc
			}
			allNodesInSuggested := true
			nodes, _ := pc.GetPhysicalPlacement()
			for _, n := range nodes {
				if !suggestedNodeSet.Contains(n) {
					allNodesInSuggested = false
					break
				}
			}
			if allNodesInSuggested && numOppor < fewestOpporNumSuggested {
				fewestOpporNumSuggested = numOppor
				fewestOpporSuggested = pc
			}
		}
	}
	if fewestOpporSuggested == nil {
		if fewestOpporCell != nil {
			klog.Infof("[SNODED]: Returning a cell NOT within suggested nodes: %v", fewestOpporCell.nodes[0])
		}
		return fewestOpporCell
	} else {
		klog.Infof("[SNODED]: Returning a cell within suggested nodes: %v", fewestOpporSuggested.nodes[0])
		return fewestOpporSuggested
	}
}

// mapNonPreassignedCellToPhysical maps a virtual cell (possibly inside a preassigned one) to the
// physical cell of the preassigned cell. This operation keeps the inner-cell topology equivalent,
// by recursively binding the cells inside the preassigned one.
func mapNonPreassignedCellToPhysical(c *VirtualCell, suggestedNodeSet common.Set) *PhysicalCell {
	if c.GetPhysicalCell() != nil {
		return c.GetPhysicalCell()
	} else if c.GetPreBoundPhysicalCell() != nil {
		return c.GetPreBoundPhysicalCell()
	} else {
		parentPhysical := mapNonPreassignedCellToPhysical(c.GetParent().(*VirtualCell), suggestedNodeSet)
		pc := getFewestOpporPhysicalCell(parentPhysical.GetChildren(), suggestedNodeSet)
		if pc == nil || pc.GetPriority() > opportunisticPriority {
			panic(fmt.Sprintf("VC Safety Broken: Cannot find physical cell for %v", c.GetName()))
		}
		c.SetPreBoundPhysicalCell(pc)
		pc.SetPreBoundVirtualCell(c)
		return pc
	}
}

// mapNonPreassignedCellToVirtual maps a physical cell (possibly allocated to a non-preassigned virtual cell)
// to the corresponding virtual cell. This can be viewed as an inverse operation of mapNonPreassignedCellToPhysical,
// used for finding the virtual cell when adding an allocated pod.
func mapNonPreassignedCellToVirtual(
	c *PhysicalCell,
	ccl ChainCellList,
	preassignedLevel CellLevel,
	p CellPriority) (*VirtualCell, string) {

	if c.GetVirtualCell() != nil {
		return c.GetVirtualCell(), ""
	} else if c.GetLevel() == preassignedLevel {
		if preassignedVirtual := getLowestPriorityCell(ccl[preassignedLevel], p); preassignedVirtual == nil {
			return nil, fmt.Sprintf("insufficient quota in the VC at the preassigned level (%v)", preassignedLevel)
		} else {
			return preassignedVirtual.(*VirtualCell), ""
		}
	} else if c.GetParent() == nil {
		return nil, fmt.Sprintf(
			"physical and virtual cell hierarchies not match (cannot reach the preassigned level %v in physical)",
			preassignedLevel)
	} else {
		parentVirtual, message := mapNonPreassignedCellToVirtual(c.GetParent().(*PhysicalCell), ccl, preassignedLevel, p)
		if parentVirtual == nil {
			return nil, message
		} else {
			return getLowestPriorityCell(parentVirtual.GetChildren(), p).(*VirtualCell), ""
		}
	}
}

// getLowestPriorityCell returns a cell with the lowest priority among the cells
// whose priorities are lower than the given priority (p).
func getLowestPriorityCell(cl CellList, p CellPriority) Cell {
	lowestPriority := maxGuaranteedPriority
	var lowestPriorityCell Cell
	for _, c := range cl {
		pp := c.GetPriority()
		if pp == freePriority {
			return c
		} else if pp < p && pp < lowestPriority {
			lowestPriority = pp
			lowestPriorityCell = c
		}
	}
	return lowestPriorityCell
}

// clearPreBindings clears the temporary bindings created during scheduling.
func clearPreBindings(virtualPlacement map[int32][]CellList) {
	for _, podPlacements := range virtualPlacement {
		for _, podGpus := range podPlacements {
			for _, gpu := range podGpus {
				for gpu != nil {
					vGpu := gpu.(*VirtualCell)
					if pGpu := vGpu.GetPreBoundPhysicalCell(); pGpu != nil {
						pGpu.SetPreBoundVirtualCell(nil)
						vGpu.SetPreBoundPhysicalCell(nil)
						gpu = gpu.GetParent()
					} else {
						break
					}
				}
			}
		}
	}
}

// bindCell binds a virtual cell to a physical cell and its parent recursively.
func bindCell(pc *PhysicalCell, vc *VirtualCell) {
	for vc.GetPhysicalCell() == nil {
		vc.SetPhysicalCell(pc)
		pc.SetVirtualCell(vc)
		klog.Infof("Cells bound: %v and %v", vc.GetName(), pc.GetName())
		if vc.GetParent() == nil {
			break
		}
		vc = vc.GetParent().(*VirtualCell)
		pc = pc.GetParent().(*PhysicalCell)
	}
}

// unbindCell unbinds a virtual cell with a physical cell and its parent recursively.
func unbindCell(c *PhysicalCell) {
	boundVirtual := c.GetVirtualCell()
	for !boundVirtual.GetPhysicalCell().IsReserved() {
		boundPhysical := boundVirtual.GetPhysicalCell()
		klog.Infof("Cells unbound: %v and %v", boundVirtual.GetName(), boundPhysical.GetName())
		boundPhysical.SetVirtualCell(nil)
		boundVirtual.SetPhysicalCell(nil)
		if boundVirtual.GetParent() == nil {
			break
		} else {
			unbindParent := true
			for _, cc := range boundVirtual.GetParent().GetChildren() {
				if child := cc.(*VirtualCell); child.GetPhysicalCell() != nil {
					unbindParent = false
					break
				}
			}
			if !unbindParent {
				break
			}
			boundVirtual = boundVirtual.GetParent().(*VirtualCell)
		}
	}
}

// setPriority sets priority for a cell and its parent recursively, guaranteeing that
// the priority of a cell is the max of those of its children.
func setPriority(c Cell, p CellPriority) {
	originalPriority := c.GetPriority()
	c.SetPriority(p)
	if parent := c.GetParent(); parent != nil {
		if p > parent.GetPriority() {
			setPriority(parent, p)
		} else if originalPriority == parent.GetPriority() && p < originalPriority {
			maxBuddyPriority := freePriority
			for _, buddy := range parent.GetChildren() {
				if buddy.GetPriority() > maxBuddyPriority {
					maxBuddyPriority = buddy.GetPriority()
				}
			}
			setPriority(parent, maxBuddyPriority)
		}
	}
}

// updateUsedGpuNumAtPriority updates the number of used GPUs at a priority for a cell
// and its parent recursively.
func updateUsedGpuNumAtPriority(c Cell, p CellPriority, increase bool) {
	for c != nil {
		delta := int32(-1)
		if increase {
			delta = 1
		}
		c.IncreaseUsedGpuNumAtPriority(p, delta)
		c = c.GetParent()
	}
}

// allPodsReleased checks if all the pods of an affinity group were released.
func allPodsReleased(allocatedPods map[int32][]*core.Pod) bool {
	for _, pods := range allocatedPods {
		for _, p := range pods {
			if p != nil {
				return false
			}
		}
	}
	return true
}
