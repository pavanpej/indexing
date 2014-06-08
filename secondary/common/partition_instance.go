// Copyright (c) 2014 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package common

type PartitionId int

//PartitionInst contains the partition definition and a SliceContainer
//to manage all the slices storing the partition's data
type PartitionInst struct {
	Defn PartitionDefn
	Sc   SliceContainer
}

//IndexPartnMap maps a IndexInstId to PartitionInstMap
type IndexPartnMap map[IndexInstId]PartitionInstMap

//PartitionInstMap maps a PartitionId to PartitionInst
type PartitionInstMap map[PartitionId]PartitionInst
