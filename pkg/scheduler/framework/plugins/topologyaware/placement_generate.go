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
	"context"
	"fmt"
	"sort"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
)

const Name = names.TopologyPlacementGenerator

const scheduledTopologyStateKey fwk.StateKey = "ScheduledTopologyStateKey"

type TopologyPlacement struct {
	handle fwk.Handle
}

var _ fwk.PlacementGeneratorPlugin = &TopologyPlacement{}

func New(_ context.Context, _ runtime.Object, fh fwk.Handle, fts feature.Features) (*TopologyPlacement, error) {
	return &TopologyPlacement{handle: fh}, nil
}

func (pl *TopologyPlacement) Name() string {
	return Name
}

type scheduledTopology struct {
	topologyDomain string
}

func (s *scheduledTopology) Clone() fwk.StateData {
	return &scheduledTopology{
		topologyDomain: s.topologyDomain,
	}
}

func (pl *TopologyPlacement) GeneratePlacements(ctx context.Context, state fwk.PodGroupCycleState, podGroup fwk.PodGroupInfo, parentPlacement *fwk.PlacementInfo) (*fwk.GeneratePlacementsResult, *fwk.Status) {
	topologyKey, ok := pl.getTopologyKey(podGroup)
	if !ok {
		return &fwk.GeneratePlacementsResult{Placements: []*fwk.Placement{{}}}, nil
	}

	topologyDomains := sets.New[string]()
	if stateData, err := state.Read(scheduledTopologyStateKey); err == nil {
		topologyDomains.Insert(stateData.(*scheduledTopology).topologyDomain)
	} else {
		scheduledPods := pl.getScheduledPods(podGroup)
		if len(scheduledPods) > 0 {
			domain, err := pl.getScheduledPodsTopologyDomain(topologyKey, scheduledPods)
			if err != nil {
				return nil, fwk.AsStatus(err)
			}
			state.Write(scheduledTopologyStateKey, &scheduledTopology{topologyDomain: domain})
			topologyDomains.Insert(domain)
		} else {
			for _, node := range parentPlacement.PlacementNodes {
				if domain, ok := node.Node().Labels[topologyKey]; ok {
					topologyDomains.Insert(domain)
				}
			}
		}
	}

	placements := []*fwk.Placement{}
	sortedDomains := topologyDomains.UnsortedList()
	sort.Strings(sortedDomains)
	for _, domain := range sortedDomains {
		placements = append(placements, &fwk.Placement{
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
		})
	}
	return &fwk.GeneratePlacementsResult{Placements: placements}, nil
}

func (pl *TopologyPlacement) getScheduledPodsTopologyDomain(topologyKey string, scheduledPods []*v1.Pod) (string, error) {
	topologyDomains := sets.New[string]()
	for _, pod := range scheduledPods {
		nodeName := pod.Spec.NodeName
		node, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
		if err != nil {
			return "", fmt.Errorf("getting node for pod %v: %w", klog.KObj(pod), err)
		}
		if domain, ok := node.Node().Labels[topologyKey]; ok {
			topologyDomains.Insert(domain)
			if topologyDomains.Len() > 1 {
				return "", fmt.Errorf("more than 1 domain found for pod group: %v", topologyDomains)
			}
		} else {
			return "", fmt.Errorf("no topology domain found for pod %v", klog.KObj(pod))
		}
	}
	// guaranteed to be a single element here
	return topologyDomains.UnsortedList()[0], nil
}

func (pl *TopologyPlacement) getTopologyKey(podGroup fwk.PodGroupInfo) (string, bool) {
	podGroupResource, err := pl.handle.SharedInformerFactory().Scheduling().V1alpha2().PodGroups().Lister().PodGroups(podGroup.GetNamespace()).Get(podGroup.GetName())
	if err != nil {
		// DONOTMERGE: handle this as error
		return "", false
	}
	schedulingConstraints := podGroupResource.Spec.SchedulingConstraints
	if schedulingConstraints == nil || len(schedulingConstraints.TopologyConstraints) < 1 {
		return "", false
	}
	return schedulingConstraints.TopologyConstraints[0].TopologyKey, true
}

func (pl *TopologyPlacement) getScheduledPods(podGroup fwk.PodGroupInfo) []*v1.Pod {
	name := podGroup.GetName()
	podGroupState, err := pl.handle.PodGroupManager().PodGroupState(podGroup.GetNamespace(), &v1.PodSchedulingGroup{PodGroupName: &name})
	if err != nil {
		// DONOTMERGE: handle this as error
		return nil
	}

	return podGroupState.ScheduledPods()
}
