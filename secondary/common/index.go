// Copyright (c) 2014 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package common

type IndexKey [][]byte

type IndexDefnId uint64
type IndexInstId uint64

type ExprType string

const (
	JavaScript ExprType = "javascript"
	N1QL                = "n1ql"
)

type PartitionScheme string

const (
	KEY   PartitionScheme = "KEY"
	HASH                  = "HASH"
	RANGE                 = "RANGE"
)

type IndexType string

const (
	View     IndexType = "view"
	LevelDB            = "leveldb"
	Llrb               = "llrb"
	ForestDB           = "forestdb"
)

type IndexState int

const (
	INDEX_STATE_INITIAL IndexState = 0
	INDEX_STATE_PENDING            = 1
	INDEX_STATE_LOADING            = 2
	INDEX_STATE_ACTIVE             = 3
	INDEX_STATE_DELETED            = 4
)

//IndexDefn represents the index definition as specified
//during CREATE INDEX
type IndexDefn struct {
	DefnId          IndexDefnId
	Name            string    // Name of the index
	Using           IndexType // indexing algorithm
	Bucket          string    // bucket name
	IsPrimary       bool
	OnExprList      []string // expression list
	Exprtype        ExprType
	PartitionScheme PartitionScheme
	PartitionKey    string
}

//IndexInst is an instance of an Index(aka replica)
type IndexInst struct {
	InstId IndexInstId
	Defn   IndexDefn
	State  IndexState
	Pc     PartitionContainer
}

//IndexInstMap is a map from IndexInstanceId to IndexInstance
type IndexInstMap map[IndexInstId]IndexInst
