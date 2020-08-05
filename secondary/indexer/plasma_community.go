// +build community

package indexer

// Copyright (c) 2014 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

import (
	"github.com/couchbase/indexing/secondary/common"
)

var errStorageCorrupted = fmt.Errorf("Storage corrupted and unrecoverable")

func NewPlasmaSlice(storage_dir string, log_dir string, path string, sliceId SliceId, idxDefn common.IndexDefn,
	idxInstId common.IndexInstId, partitionId common.PartitionId, isPrimary bool, numPartitions int,
	sysconf common.Config, idxStats *IndexStats, indexerStats *IndexerStats) (Slice, error) {
	panic("Plasma is only supported in Enterprise Edition")
}

func deleteFreeWriters(instId common.IndexInstId) {
	// do nothing
}

func DestroyPlasmaSlice(path string) error {
	// do nothing
	return nil
}

func ListPlasmaSlices() ([]string, error) {
	// do nothing
	return nil, nil
}

func BackupCorruptedPlasmaSlice(string, func(string) (string, error), func(string)) error {
	// do nothing
	return nil
}
