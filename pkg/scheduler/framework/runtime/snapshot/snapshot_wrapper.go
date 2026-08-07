/*
Copyright The Kubernetes Authors.

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

package snapshot

import (
	"context"
	"fmt"
	"maps"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type operationType int

const (
	Add operationType = iota
	Remove
	Assume
)

type podDelta struct {
	pod        fwk.PodInfo
	nodeDeltas map[string]int
}

type podDeltas struct {
	items    map[types.UID]*podDelta
	readOnly bool
}

func (d *podDeltas) ensureWritable() {
	if !d.readOnly {
		if d.items == nil {
			d.items = make(map[types.UID]*podDelta)
		}
		return
	}
	cloned := make(map[types.UID]*podDelta, len(d.items))
	for uid, pd := range d.items {
		cloned[uid] = &podDelta{
			pod:        pd.pod,
			nodeDeltas: maps.Clone(pd.nodeDeltas),
		}
	}
	d.items = cloned
	d.readOnly = false
}

func (d *podDeltas) asReadOnly() podDeltas {
	d.readOnly = true
	return *d
}

type Operation struct {
	id         int
	name       operationType
	target     fwk.PodInfo
	nodeName   string
	cycleState fwk.CycleState
}

type versionNode struct {
	depth      int
	op         Operation
	prev       *versionNode
	moveDeltas podDeltas
	movedTo    *versionNode
}

type snapshotMetadata struct {
	base              *versionNode
	assumeVersion     *versionNode
	deltasSinceAssume podDeltas
	originalId        int
}

func (psv *snapshotMetadata) Clone() fwk.StateData {
	return &snapshotMetadata{
		base:              psv.base,
		assumeVersion:     psv.assumeVersion,
		deltasSinceAssume: psv.deltasSinceAssume.asReadOnly(),
		originalId:        psv.originalId,
	}
}

type Savepoint = fwk.Savepoint

type SnapshotWrapper struct {
	head *versionNode

	handle   framework.Framework
	snapshot *cache.Snapshot
	nextID   int
}

var _ fwk.SnapshotWrapper = &SnapshotWrapper{}

func NewSnapshotWrapper(handle framework.Framework, snapshot *cache.Snapshot) *SnapshotWrapper {
	return &SnapshotWrapper{
		head:     &versionNode{depth: 0},
		handle:   handle,
		snapshot: snapshot,
	}
}

func (s *SnapshotWrapper) SetHandle(handle framework.Framework) {
	s.handle = handle
}

func (s *SnapshotWrapper) nextOpID() int {
	s.nextID++
	return s.nextID
}

func (s *SnapshotWrapper) storeAssumeVersion(cycleState fwk.CycleState, node *versionNode) {
	if cycleState == nil {
		return
	}
	if state, err := cycleState.Read(podSnapshotVersionKey); err == nil {
		if meta, ok := state.(*snapshotMetadata); ok && meta != nil {
			if meta.assumeVersion != nil && meta.assumeVersion.op.nodeName == node.op.nodeName {
				node.op.id = meta.assumeVersion.op.id
				meta.assumeVersion.moveDeltas = meta.deltasSinceAssume.asReadOnly()
				meta.assumeVersion.movedTo = node
			}
			meta.assumeVersion = node
			meta.deltasSinceAssume = podDeltas{}
		}
	}
}

const podSnapshotVersionKey = "podSnapshotVersionKey"

func (s *SnapshotWrapper) Init(ctx context.Context, pod *v1.Pod, cycleState fwk.CycleState, preFilterResult *fwk.PreFilterResult) *fwk.Status {
	state, err := cycleState.Read(podSnapshotVersionKey)
	if err != nil && err != fwk.ErrNotFound {
		return fwk.AsStatus(fmt.Errorf("error accessing state: %w", err))
	}
	if state != nil || err == nil {
		return fwk.AsStatus(fmt.Errorf("state is already initialized"))
	}

	cycleState.Write(podSnapshotVersionKey, &snapshotMetadata{
		base: s.head,
	})
	return nil
}

func (d *podDeltas) recordDelta(pod fwk.PodInfo, nodeName string, delta int) {
	d.ensureWritable()
	pd, ok := d.items[pod.GetPod().UID]
	if !ok {
		pd = &podDelta{pod: pod, nodeDeltas: map[string]int{}}
		d.items[pod.GetPod().UID] = pd
	}

	pd.nodeDeltas[nodeName] += delta
}

func (d *podDeltas) merge(other podDeltas) {
	if other.items == nil {
		return
	}
	d.ensureWritable()
	for _, otherDelta := range other.items {
		for nodeName, delta := range otherDelta.nodeDeltas {
			d.recordDelta(otherDelta.pod, nodeName, delta)
		}
	}
}

func computeDeltas(source *versionNode, target *versionNode) (res podDeltas, fastPathTaken bool) {
	if source.op.id == target.op.id {
		curr := source
		for curr != target && curr != nil {
			curr = curr.movedTo
		}
		if curr != nil {
			first := true
			curr = source
			for curr != target {
				if first {
					res = curr.moveDeltas
					first = false
				} else {
					res.merge(curr.moveDeltas)
				}
				curr = curr.movedTo
			}
			return res, true
		}
	}

	for source != target {
		if source == nil || target == nil {
			// technically invalid, caller should run PreFilter from scratch
			return res, false
		}

		sourceDepth := source.depth
		targetDepth := target.depth

		if sourceDepth >= targetDepth {
			// Both Add and Assume place pods onto nodes in the cache snapshot, so stepping
			// backwards past them removes pod membership on that node.
			switch source.op.name {
			case Add, Assume:
				res.recordDelta(source.op.target, source.op.nodeName, -1)
			case Remove:
				res.recordDelta(source.op.target, source.op.nodeName, 1)
			}
			source = source.prev
		}

		if sourceDepth <= targetDepth {
			switch target.op.name {
			case Add, Assume:
				res.recordDelta(target.op.target, target.op.nodeName, 1)
			case Remove:
				res.recordDelta(target.op.target, target.op.nodeName, -1)
			}
			target = target.prev
		}
	}
	return res, false
}

func (s *SnapshotWrapper) Sync(ctx context.Context, pod *v1.Pod, cycleState fwk.CycleState) *fwk.Status {
	state, err := cycleState.Read(podSnapshotVersionKey)
	if err == fwk.ErrNotFound {
		return fwk.AsStatus(fmt.Errorf("state is not initialized"))
	} else if err != nil {
		return fwk.AsStatus(fmt.Errorf("error accessing state: %w", err))
	}

	meta, ok := state.(*snapshotMetadata)
	if !ok || meta == nil {
		return fwk.AsStatus(fmt.Errorf("invalid snapshot metadata in cycle state"))
	}

	deltas, _ := computeDeltas(meta.base, s.head)

	toRemove := map[string][]fwk.PodInfo{}
	toAdd := map[string][]fwk.PodInfo{}

	for _, podDelta := range deltas.items {
		for nodeName, nodeDelta := range podDelta.nodeDeltas {
			if nodeDelta < 0 {
				toRemove[nodeName] = append(toRemove[nodeName], podDelta.pod)
			} else if nodeDelta > 0 {
				toAdd[nodeName] = append(toAdd[nodeName], podDelta.pod)
			}
		}
	}

	for nodeName, pods := range toRemove {
		nodeInfo, err := s.snapshot.Get(nodeName)
		if err != nil {
			return fwk.AsStatus(fmt.Errorf("failed to get node %q from snapshot: %w", nodeName, err))
		}
		for _, podToRemove := range pods {
			status := s.handle.RunPreFilterExtensionRemovePod(ctx, cycleState, pod, podToRemove, nodeInfo)
			if !status.IsSuccess() {
				return status
			}
		}
	}

	for nodeName, pods := range toAdd {
		nodeInfo, err := s.snapshot.Get(nodeName)
		if err != nil {
			return fwk.AsStatus(fmt.Errorf("failed to get node %q from snapshot: %w", nodeName, err))
		}
		for _, podToAdd := range pods {
			status := s.handle.RunPreFilterExtensionAddPod(ctx, cycleState, pod, podToAdd, nodeInfo)
			if !status.IsSuccess() {
				return status
			}
		}
	}

	if meta.assumeVersion != nil {
		if meta.deltasSinceAssume.items == nil {
			meta.deltasSinceAssume = deltas.asReadOnly()
		} else {
			meta.deltasSinceAssume.merge(deltas)
		}
	}
	meta.base = s.head

	return nil
}

func (s *SnapshotWrapper) RemovePod(ctx context.Context, podInfo fwk.PodInfo) error {
	logger := klog.FromContext(ctx)
	if podInfo == nil || podInfo.GetPod() == nil {
		return fmt.Errorf("pod cannot be nil")
	}
	pod := podInfo.GetPod()
	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		return fmt.Errorf("pod %s/%s does not have NodeName set", pod.Namespace, pod.Name)
	}

	if err := s.snapshot.RemovePod(logger, pod, nodeName); err != nil {
		return err
	}

	s.head = &versionNode{
		depth: s.head.depth + 1,
		op: Operation{
			id:       s.nextOpID(),
			name:     Remove,
			target:   podInfo,
			nodeName: nodeName,
		},
		prev: s.head,
	}
	return nil
}

func (s *SnapshotWrapper) RestorePod(ctx context.Context, podInfo fwk.PodInfo) error {
	if podInfo == nil || podInfo.GetPod() == nil {
		return fmt.Errorf("pod cannot be nil")
	}
	pod := podInfo.GetPod()
	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		return fmt.Errorf("pod %s/%s does not have NodeName set", pod.Namespace, pod.Name)
	}

	if err := s.snapshot.AddPod(podInfo, nodeName); err != nil {
		return err
	}

	s.head = &versionNode{
		depth: s.head.depth + 1,
		op: Operation{
			id:       s.nextOpID(),
			name:     Add,
			target:   podInfo,
			nodeName: nodeName,
		},
		prev: s.head,
	}
	return nil
}

func (s *SnapshotWrapper) ReservePod(ctx context.Context, podInfo fwk.PodInfo, cycleState fwk.CycleState, nodeName string) *fwk.Status {
	pod := podInfo.GetPod()
	_ = pod
	state, err := cycleState.Read(podSnapshotVersionKey)
	if err != nil {
		return fwk.AsStatus(fmt.Errorf("error reading cycle state: %w", err))
	}
	meta, ok := state.(*snapshotMetadata)
	if !ok || meta == nil {
		return fwk.AsStatus(fmt.Errorf("invalid snapshot metadata in cycle state"))
	}
	if meta.base != s.head {
		return fwk.AsStatus(fmt.Errorf("cycle state version does not match snapshot version"))
	}

	status := s.handle.RunReservePluginsReserve(ctx, cycleState, pod, nodeName)
	if !status.IsSuccess() {
		return status
	}

	if err := s.snapshot.AddPod(podInfo, nodeName); err != nil {
		s.handle.RunReservePluginsUnreserve(ctx, cycleState, pod, nodeName)
		return fwk.AsStatus(err)
	}

	newNode := &versionNode{
		depth: s.head.depth + 1,
		op: Operation{
			id:         s.nextOpID(),
			name:       Assume,
			target:     podInfo,
			nodeName:   nodeName,
			cycleState: cycleState,
		},
		prev: s.head,
	}
	s.head = newNode
	s.storeAssumeVersion(cycleState, newNode)
	return nil
}

func (s *SnapshotWrapper) GetSavepoint() fwk.Savepoint {
	return fwk.Savepoint(s.head)
}

func (s *SnapshotWrapper) RestoreSavepoint(sp fwk.Savepoint) {
	s.RestoreSavepointWithReverted(sp)
}

func (s *SnapshotWrapper) RestoreSavepointWithReverted(sp fwk.Savepoint) []Operation {
	targetNode, _ := sp.(*versionNode)
	var reverted []Operation
	logger := klog.Background()
	ctx := klog.NewContext(context.Background(), logger)

	curr := s.head
	for curr != targetNode && curr != nil {
		switch curr.op.name {
		case Add:
			pod := curr.op.target.GetPod()
			_ = s.snapshot.RemovePod(logger, pod, pod.Spec.NodeName)
		case Remove:
			pod := curr.op.target.GetPod()
			_ = s.snapshot.AddPod(curr.op.target, pod.Spec.NodeName)
		case Assume:
			pod := curr.op.target.GetPod()
			if s.handle != nil {
				s.handle.RunReservePluginsUnreserve(ctx, curr.op.cycleState, pod, curr.op.nodeName)
			}
			_ = s.snapshot.RemovePod(logger, pod, curr.op.nodeName)
		}
		reverted = append(reverted, curr.op)
		curr = curr.prev
	}

	s.head = targetNode
	return reverted
}

func (s *SnapshotWrapper) RecordAssumePod(ctx context.Context, podInfo fwk.PodInfo, cycleState fwk.CycleState, nodeName string) {
	newNode := &versionNode{
		depth: s.head.depth + 1,
		op: Operation{
			id:         s.nextOpID(),
			name:       Assume,
			target:     podInfo,
			nodeName:   nodeName,
			cycleState: cycleState,
		},
		prev: s.head,
	}
	s.head = newNode
	s.storeAssumeVersion(cycleState, newNode)
}

func (s *SnapshotWrapper) RecordForgetPod(ctx context.Context, pod *v1.Pod) {
	if s.head != nil && s.head.op.name == Assume && s.head.op.target.GetPod().UID == pod.UID {
		s.head = s.head.prev
	}
}
