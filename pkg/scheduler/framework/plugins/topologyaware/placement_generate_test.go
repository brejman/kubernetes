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

package topologyaware

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	schedulingapi "k8s.io/api/scheduling/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/backend/podgroupmanager"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"k8s.io/kubernetes/test/utils/ktesting"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestGeneratePlacements(t *testing.T) {
	tests := []struct {
		name                  string
		podGroup              *schedulingapi.PodGroup
		scheduledPodGroupPods []*v1.Pod
		placementNodes        []*v1.Node
		otherNodes            []*v1.Node
		cycleState            map[fwk.StateKey]fwk.StateData
		wantPlacements        []*fwk.Placement
		wantStatus            fwk.Code
	}{
		{
			name: "without constraint returns placement matching all nodes",
			podGroup: &schedulingapi.PodGroup{
				Spec: schedulingapi.PodGroupSpec{},
			},
			placementNodes: []*v1.Node{
				st.MakeNode().Name("node1").Obj(),
				st.MakeNode().Name("node2").Label("foo", "bar").Obj(),
			},
			wantPlacements: []*fwk.Placement{{}},
			wantStatus:     fwk.Success,
		},
		{
			name:     "with topology key constraint, returns placement for each topology domain",
			podGroup: makePodGroup("topology1"),
			placementNodes: []*v1.Node{
				st.MakeNode().Name("node0").Label("topology2", "d1").Obj(),
				st.MakeNode().Name("node1").Label("topology2", "d4").Obj(),
				st.MakeNode().Name("node2").Label("topology1", "d1").Obj(),
				st.MakeNode().Name("node3").Label("topology1", "d2").Obj(),
				st.MakeNode().Name("node4").Label("topology1", "d1").Obj(),
				st.MakeNode().Name("node5").Label("topology1", "d3").Obj(),
			},
			wantPlacements: []*fwk.Placement{
				makePlacement("topology1", "d1"),
				makePlacement("topology1", "d2"),
				makePlacement("topology1", "d3"),
			},
			wantStatus: fwk.Success,
		},
		{
			name:     "without matching topology label, returns empty",
			podGroup: makePodGroup("topology3"),
			placementNodes: []*v1.Node{
				st.MakeNode().Name("node0").Label("topology2", "d1").Obj(),
				st.MakeNode().Name("node1").Label("topology2", "d4").Obj(),
				st.MakeNode().Name("node2").Label("topology1", "d1").Obj(),
				st.MakeNode().Name("node3").Label("topology1", "d2").Obj(),
				st.MakeNode().Name("node4").Label("topology1", "d1").Obj(),
				st.MakeNode().Name("node5").Label("topology1", "d3").Obj(),
			},
			wantPlacements: []*fwk.Placement{},
			wantStatus:     fwk.Success,
		},
		{
			name:     "with pods already scheduled in a single domain, returns that domain",
			podGroup: makePodGroup("topology"),
			scheduledPodGroupPods: []*v1.Pod{
				st.MakePod().Name("pod1").Node("node2").Obj(),
				st.MakePod().Name("pod2").Node("node3").Obj(),
			},
			placementNodes: []*v1.Node{
				st.MakeNode().Name("node1").Label("topology", "d2").Obj(),
			},
			otherNodes: []*v1.Node{
				st.MakeNode().Name("node2").Label("topology", "d1").Obj(),
				st.MakeNode().Name("node3").Label("topology", "d1").Obj(),
			},
			wantPlacements: []*fwk.Placement{makePlacement("topology", "d1")},
			wantStatus:     fwk.Success,
		},
		{
			name:     "with pods already scheduled in conflicting domains, returns error",
			podGroup: makePodGroup("topology"),
			scheduledPodGroupPods: []*v1.Pod{
				st.MakePod().Name("pod1").Node("node2").Obj(),
				st.MakePod().Name("pod2").Node("node3").Obj(),
			},
			placementNodes: []*v1.Node{
				st.MakeNode().Name("node1").Label("topology", "d2").Obj(),
			},
			otherNodes: []*v1.Node{
				st.MakeNode().Name("node2").Label("topology", "d0").Obj(),
				st.MakeNode().Name("node3").Label("topology", "d1").Obj(),
			},
			wantStatus: fwk.Error,
		},
		{
			name:     "with already scheduled pod on node outside of snapshot, returns error",
			podGroup: makePodGroup("topology"),
			scheduledPodGroupPods: []*v1.Pod{
				st.MakePod().Name("pod1").Node("node2").Obj(),
				st.MakePod().Name("pod2").Node("node4").Obj(),
			},
			placementNodes: []*v1.Node{
				st.MakeNode().Name("node1").Label("topology", "d2").Obj(),
			},
			otherNodes: []*v1.Node{
				st.MakeNode().Name("node2").Label("topology", "d1").Obj(),
				st.MakeNode().Name("node3").Label("topology", "d1").Obj(),
			},
			wantStatus: fwk.Error,
		},
		{
			name:     "with already scheduled pod on node without topology label, returns error",
			podGroup: makePodGroup("topology"),
			scheduledPodGroupPods: []*v1.Pod{
				st.MakePod().Name("pod1").Node("node2").Obj(),
			},
			placementNodes: []*v1.Node{
				st.MakeNode().Name("node1").Label("topology", "d2").Obj(),
			},
			otherNodes: []*v1.Node{
				st.MakeNode().Name("node2").Label("foo", "bar").Obj(),
			},
			wantStatus: fwk.Error,
		},
		{
			name:     "with scheduled topology domain in cycle state, does not recompute the topology domain",
			podGroup: makePodGroup("topology"),
			scheduledPodGroupPods: []*v1.Pod{
				// referenced nodes are not mocked as they are not expected to be used.
				st.MakePod().Name("pod1").Node("node2").Obj(),
				st.MakePod().Name("pod2").Node("node4").Obj(),
			},
			placementNodes: []*v1.Node{
				st.MakeNode().Name("node1").Label("topology", "d2").Obj(),
			},
			otherNodes: []*v1.Node{},
			cycleState: map[fwk.StateKey]fwk.StateData{
				scheduledTopologyStateKey: &scheduledTopology{topologyDomain: "d1"},
			},
			wantPlacements: []*fwk.Placement{
				makePlacement("topology", "d1"),
			},
			wantStatus: fwk.Success,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, tCtx := ktesting.NewTestContext(t)
			nodeList := &v1.NodeList{}
			for _, node := range append(tt.placementNodes, tt.otherNodes...) {
				nodeList.Items = append(nodeList.Items, *node)
			}

			cs := clientsetfake.NewClientset(
				&schedulingapi.PodGroupList{Items: []schedulingapi.PodGroup{*tt.podGroup}},
				nodeList,
			)
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			informerFactory.Scheduling().V1alpha2().PodGroups().Informer().GetStore().Add(tt.podGroup)
			for _, node := range tt.placementNodes {
				informerFactory.Core().V1().Nodes().Informer().GetStore().Add(node)
			}

			snapshot := cache.NewSnapshot(nil, append(tt.placementNodes, tt.otherNodes...))
			podGroupManager := podgroupmanager.New(logger)
			fh, _ := runtime.NewFramework(tCtx, nil, nil,
				runtime.WithInformerFactory(informerFactory),
				runtime.WithPodGroupManager(podGroupManager),
				runtime.WithSnapshotSharedLister(snapshot),
			)

			informerFactory.Start(tCtx.Done())
			informerFactory.WaitForCacheSync(tCtx.Done())

			pl, err := New(tCtx, nil, fh, feature.Features{})

			if err != nil {
				t.Fatalf("failed when creating plugin: %v", err)
			}

			nis := []fwk.NodeInfo{}
			for _, node := range tt.placementNodes {
				ni := framework.NewNodeInfo()
				ni.SetNode(node)
				nis = append(nis, ni)
			}
			for _, scheduledPod := range tt.scheduledPodGroupPods {
				scheduledPod.ObjectMeta.Namespace = tt.podGroup.Namespace
				scheduledPod.Spec.SchedulingGroup = &v1.PodSchedulingGroup{
					PodGroupName: &tt.podGroup.Name,
				}
				scheduledPod.ObjectMeta.UID = types.UID(scheduledPod.ObjectMeta.Name)
				podGroupManager.AddPod(scheduledPod)
			}

			placement := &fwk.PlacementInfo{
				PlacementNodes: nis,
			}
			podGroupInfo := &framework.PodGroupInfo{
				Name:      tt.podGroup.Name,
				Namespace: tt.podGroup.Namespace,
			}

			cycleState := framework.NewCycleState()
			for k, v := range tt.cycleState {
				cycleState.Write(k, v)
			}

			result, status := pl.GeneratePlacements(tCtx, cycleState, podGroupInfo, placement)

			if status.Code() != tt.wantStatus {
				t.Fatalf("expected status %v, got %v", tt.wantStatus, status.Code())
			}

			if status.IsSuccess() {
				if diff := cmp.Diff(tt.wantPlacements, result.Placements, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("Unexpected placements (-want,+got):\n%s", diff)
				}
			}
		})
	}
}

func makePlacement(topologyKey, domain string) *fwk.Placement {
	return &fwk.Placement{
		NodeSelector: &v1.NodeSelector{
			NodeSelectorTerms: []v1.NodeSelectorTerm{
				{
					MatchExpressions: []v1.NodeSelectorRequirement{
						{
							Key:      topologyKey,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{domain},
						},
					},
				},
			},
		},
	}
}

func makePodGroup(topologyKey string) *schedulingapi.PodGroup {
	return &schedulingapi.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pg1",
			Namespace: "default",
		},
		Spec: schedulingapi.PodGroupSpec{
			SchedulingConstraints: &schedulingapi.PodGroupSchedulingConstraints{
				TopologyConstraints: []schedulingapi.TopologyConstraint{
					{
						TopologyKey: topologyKey,
					},
				},
			},
		},
	}
}
