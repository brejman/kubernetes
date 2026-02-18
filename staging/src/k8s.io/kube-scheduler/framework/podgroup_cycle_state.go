/*
Copyright 2025 The Kubernetes Authors.

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

package framework

// PodGroupCycleState provides a mechanism for plugins that operate on pod groups to store and retrieve arbitrary data.
// StateData stored by one plugin can be read, altered, or deleted by another plugin that operates on a pod group.
// PodGroupCycleState does not provide any data protection, as all plugins are assumed to be
// trusted.
type PodGroupCycleState interface {
	// ShouldRecordPluginMetrics returns whether metrics.PluginExecutionDuration metrics
	// should be recorded.
	// This function is mostly for the scheduling framework runtime, plugins usually don't have to use it.
	ShouldRecordPluginMetrics() bool
	// Read retrieves data with the given "key" from PodGroupCycleState. If the key is not
	// present, ErrNotFound is returned.
	//
	// See PodGroupCycleState for notes on concurrency.
	Read(key StateKey) (StateData, error)
	// Write stores the given "val" in PodGroupCycleState with the given "key".
	//
	// See PodGroupCycleState for notes on concurrency.
	Write(key StateKey, val StateData)
	// Delete deletes data with the given key from PodGroupCycleState.
	//
	// See PodGroupCycleState for notes on concurrency.
	Delete(key StateKey)
}
