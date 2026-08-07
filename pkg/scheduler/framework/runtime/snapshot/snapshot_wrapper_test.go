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
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func newTestPodInfo(uid, name string) fwk.PodInfo {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:  types.UID(uid),
			Name: name,
		},
	}
	pi, _ := framework.NewPodInfo(pod)
	return pi
}

func TestCOW_PodDeltas_EnsureWritable(t *testing.T) {
	pod := newTestPodInfo("uid-1", "pod-1")

	var orig podDeltas
	orig.recordDelta(pod, "node-1", 1)

	// Mark original as read-only (as stored on a versionNode moveDeltas)
	ro := orig.asReadOnly()

	if !ro.readOnly {
		t.Fatalf("expected ro to be readOnly")
	}
	if !orig.readOnly {
		t.Fatalf("expected orig to be marked readOnly as well")
	}

	// Fork via COW when calling recordDelta on a read-only instance
	var fork podDeltas = ro
	fork.recordDelta(pod, "node-1", 2)

	if fork.readOnly {
		t.Errorf("expected fork to be writable after modification")
	}

	// Verify original contents were not mutated
	if got := orig.items["uid-1"].nodeDeltas["node-1"]; got != 1 {
		t.Errorf("expected original delta to remain 1, got %d", got)
	}

	// Verify fork contents received the delta (1 + 2 = 3)
	if got := fork.items["uid-1"].nodeDeltas["node-1"]; got != 3 {
		t.Errorf("expected fork delta to be 3, got %d", got)
	}

	// Also verify mutating orig triggers COW and does not mutate ro
	orig.recordDelta(pod, "node-1", 10)
	if got := ro.items["uid-1"].nodeDeltas["node-1"]; got != 1 {
		t.Errorf("expected ro delta to remain 1 after mutating orig, got %d", got)
	}
}

func TestCOW_PodDeltas_Merge(t *testing.T) {
	pod1 := newTestPodInfo("uid-1", "pod-1")
	pod2 := newTestPodInfo("uid-2", "pod-2")

	var base podDeltas
	base.recordDelta(pod1, "node-1", 1)

	baseRO := base.asReadOnly()

	var other podDeltas
	other.recordDelta(pod2, "node-2", 1)

	// Merge into read-only instance should trigger COW copy first
	var merged podDeltas = baseRO
	merged.merge(other)

	if merged.readOnly {
		t.Errorf("expected merged instance to be writable")
	}

	// Base should still only have pod1
	if _, ok := base.items["uid-2"]; ok {
		t.Errorf("base map was mutated during merge into copy")
	}

	// Merged instance should have both pod1 and pod2
	if _, ok := merged.items["uid-1"]; !ok {
		t.Errorf("merged map missing uid-1")
	}
	if _, ok := merged.items["uid-2"]; !ok {
		t.Errorf("merged map missing uid-2")
	}
}

func TestComputeDeltas_FastPath_COW(t *testing.T) {
	pod := newTestPodInfo("uid-1", "pod-1")

	var moveDeltas1 podDeltas
	moveDeltas1.recordDelta(pod, "node-1", 1)

	var moveDeltas2 podDeltas
	moveDeltas2.recordDelta(pod, "node-2", -1)

	v0 := &versionNode{
		depth:      0,
		op:         Operation{id: 10},
		moveDeltas: moveDeltas1.asReadOnly(),
	}
	v1 := &versionNode{
		depth:      1,
		op:         Operation{id: 10},
		moveDeltas: moveDeltas2.asReadOnly(),
	}
	v2 := &versionNode{
		depth: 2,
		op:    Operation{id: 10},
	}

	v0.movedTo = v1
	v1.movedTo = v2

	// Single-hop move from v0 to v1 should return moveDeltas1 without copy/merge allocation
	res1, ok := computeDeltas(v0, v1)
	if !ok {
		t.Fatalf("expected computeDeltas fast path to succeed")
	}
	if res1.items["uid-1"].nodeDeltas["node-1"] != 1 {
		t.Errorf("unexpected delta for single-hop move: %v", res1)
	}

	// Multi-hop move from v0 to v2 should merge both hop deltas cleanly without mutating v0's moveDeltas
	res2, ok := computeDeltas(v0, v2)
	if !ok {
		t.Fatalf("expected computeDeltas fast path multi-hop to succeed")
	}
	if got := res2.items["uid-1"].nodeDeltas["node-1"]; got != 1 {
		t.Errorf("expected node-1 delta 1, got %d", got)
	}
	if got := res2.items["uid-1"].nodeDeltas["node-2"]; got != -1 {
		t.Errorf("expected node-2 delta -1, got %d", got)
	}

	// Verify v0's moveDeltas was untouched
	if len(v0.moveDeltas.items["uid-1"].nodeDeltas) != 1 {
		t.Errorf("v0 moveDeltas was unexpectedly modified")
	}
}

func TestSnapshotWrapper_RemovePod_RestorePod(t *testing.T) {
	ctx := context.Background()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID("pod-1"),
			Namespace: "default",
			Name:      "pod-1",
		},
		Spec: v1.PodSpec{
			NodeName: "node-1",
		},
	}
	podInfo, err := framework.NewPodInfo(pod)
	if err != nil {
		t.Fatalf("failed to create PodInfo: %v", err)
	}

	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	snap := cache.NewSnapshot([]*v1.Pod{pod}, []*v1.Node{node})
	sw := NewSnapshotWrapper(nil, snap)

	// Test RemovePod
	if err := sw.RemovePod(ctx, podInfo); err != nil {
		t.Fatalf("RemovePod failed: %v", err)
	}
	if sw.head.op.name != Remove {
		t.Errorf("expected op Remove, got %v", sw.head.op.name)
	}
	if sw.head.op.target != podInfo {
		t.Errorf("expected target to be podInfo, got %v", sw.head.op.target)
	}
	if sw.head.op.nodeName != "node-1" {
		t.Errorf("expected nodeName node-1, got %v", sw.head.op.nodeName)
	}

	// Test RestorePod
	if err := sw.RestorePod(ctx, podInfo); err != nil {
		t.Fatalf("RestorePod failed: %v", err)
	}
	if sw.head.op.name != Add {
		t.Errorf("expected op Add, got %v", sw.head.op.name)
	}
	if sw.head.op.target != podInfo {
		t.Errorf("expected target to be podInfo, got %v", sw.head.op.target)
	}
	if sw.head.op.nodeName != "node-1" {
		t.Errorf("expected nodeName node-1, got %v", sw.head.op.nodeName)
	}

	// Test nil pod validation
	if err := sw.RemovePod(ctx, nil); err == nil {
		t.Errorf("expected error for nil podInfo on RemovePod")
	}
	if err := sw.RestorePod(ctx, nil); err == nil {
		t.Errorf("expected error for nil podInfo on RestorePod")
	}

	// Test pod without nodeName validation
	podNoNode := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-no-node", Namespace: "default"}}
	podInfoNoNode, _ := framework.NewPodInfo(podNoNode)
	if err := sw.RemovePod(ctx, podInfoNoNode); err == nil {
		t.Errorf("expected error for pod with empty nodeName on RemovePod")
	}
	if err := sw.RestorePod(ctx, podInfoNoNode); err == nil {
		t.Errorf("expected error for pod with empty nodeName on RestorePod")
	}
}

