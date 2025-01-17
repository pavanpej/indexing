// Copyright 2014-Present Couchbase, Inc.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
// in that file, in accordance with the Business Source License, use of this
// software will be governed by the Apache License, Version 2.0, included in
// the file licenses/APL2.txt.

package indexer

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/couchbase/indexing/secondary/common"
	forestdb "github.com/couchbase/indexing/secondary/fdb"
	"github.com/couchbase/indexing/secondary/logging"
)

var (
	ErrIndexRollback            = errors.New("Indexer rollback")
	ErrIndexRollbackOrBootstrap = errors.New("Indexer rollback or warmup")
)

type KeyspaceIdInstList map[string][]common.IndexInstId
type StreamKeyspaceIdInstList map[common.StreamId]KeyspaceIdInstList

type KeyspaceIdInstsPerWorker map[string][][]common.IndexInstId
type StreamKeyspaceIdInstsPerWorker map[common.StreamId]KeyspaceIdInstsPerWorker

//StorageManager manages the snapshots for the indexes and responsible for storing
//indexer metadata in a config database

const INST_MAP_KEY_NAME = "IndexInstMap"

type StorageManager interface {
}

type storageMgr struct {
	supvCmdch  MsgChannel //supervisor sends commands on this channel
	supvRespch MsgChannel //channel to send any async message to supervisor

	snapshotReqCh []MsgChannel // Channel to listen for snapshot requests from scan coordinator

	snapshotNotifych []chan IndexSnapshot

	indexInstMap  IndexInstMapHolder
	indexPartnMap IndexPartnMapHolder

	streamKeyspaceIdInstList       StreamKeyspaceIdInstListHolder
	streamKeyspaceIdInstsPerWorker StreamKeyspaceIdInstsPerWorkerHolder

	// Latest readable index snapshot for each index instance
	indexSnapMap IndexSnapMapHolder
	// List of waiters waiting for a snapshot to be created with expected
	// atleast-timestamp
	waitersMap SnapshotWaitersMapHolder

	dbfile *forestdb.File
	meta   *forestdb.KVStore // handle for index meta

	config common.Config

	stats IndexerStatsHolder

	muSnap sync.Mutex //lock to protect updates to snapMap and waitersMap

	statsLock sync.Mutex

	lastFlushDone int64
}

type snapshotWaiter struct {
	wch       chan interface{}
	ts        *common.TsVbuuid
	cons      common.Consistency
	idxInstId common.IndexInstId
	expired   time.Time
}

type PartnSnapMap map[common.PartitionId]PartitionSnapshot

func newSnapshotWaiter(idxId common.IndexInstId, ts *common.TsVbuuid,
	cons common.Consistency,
	ch chan interface{}, expired time.Time) *snapshotWaiter {

	return &snapshotWaiter{
		ts:        ts,
		cons:      cons,
		wch:       ch,
		idxInstId: idxId,
		expired:   expired,
	}
}

func (w *snapshotWaiter) Notify(is IndexSnapshot) {
	w.wch <- is
}

func (w *snapshotWaiter) Error(err error) {
	w.wch <- err
}

//NewStorageManager returns an instance of storageMgr or err message
//It listens on supvCmdch for command and every command is followed
//by a synchronous response of the supvCmdch.
//Any async response to supervisor is sent to supvRespch.
//If supvCmdch get closed, storageMgr will shut itself down.
func NewStorageManager(supvCmdch MsgChannel, supvRespch MsgChannel,
	indexPartnMap IndexPartnMap, config common.Config, snapshotNotifych []chan IndexSnapshot,
	snapshotReqCh []MsgChannel, stats *IndexerStats) (StorageManager, Message) {

	//Init the storageMgr struct
	s := &storageMgr{
		supvCmdch:        supvCmdch,
		supvRespch:       supvRespch,
		snapshotNotifych: snapshotNotifych,
		snapshotReqCh:    snapshotReqCh,
		config:           config,
	}
	s.indexInstMap.Init()
	s.indexPartnMap.Init()
	s.indexSnapMap.Init()
	s.waitersMap.Init()
	s.stats.Set(stats)

	s.streamKeyspaceIdInstList.Init()
	s.streamKeyspaceIdInstsPerWorker.Init()

	//if manager is not enabled, create meta file
	if config["enableManager"].Bool() == false {
		fdbconfig := forestdb.DefaultConfig()
		kvconfig := forestdb.DefaultKVStoreConfig()
		var err error

		if s.dbfile, err = forestdb.Open("meta", fdbconfig); err != nil {
			return nil, &MsgError{err: Error{cause: err}}
		}

		// Make use of default kvstore provided by forestdb
		if s.meta, err = s.dbfile.OpenKVStore("default", kvconfig); err != nil {
			return nil, &MsgError{err: Error{cause: err}}
		}
	}

	for i := 0; i < len(s.snapshotReqCh); i++ {
		go s.listenSnapshotReqs(i)
	}

	//start Storage Manager loop which listens to commands from its supervisor
	go s.run()

	return s, &MsgSuccess{}

}

//run starts the storage manager loop which listens to messages
//from its supervisor(indexer)
func (s *storageMgr) run() {

	//main Storage Manager loop
loop:
	for {
		select {

		case cmd, ok := <-s.supvCmdch:
			if ok {
				if cmd.GetMsgType() == STORAGE_MGR_SHUTDOWN {
					logging.Infof("StorageManager::run Shutting Down")
					for i := 0; i < len(s.snapshotNotifych); i++ {
						close(s.snapshotNotifych[i])
					}
					s.supvCmdch <- &MsgSuccess{}
					break loop
				}
				s.handleSupvervisorCommands(cmd)
			} else {
				//supervisor channel closed. exit
				break loop
			}

		}
	}
}

func (s *storageMgr) handleSupvervisorCommands(cmd Message) {

	switch cmd.GetMsgType() {

	case MUT_MGR_FLUSH_DONE:
		s.handleCreateSnapshot(cmd)

	case INDEXER_ROLLBACK:
		s.handleRollback(cmd)

	case UPDATE_INDEX_INSTANCE_MAP:
		s.handleUpdateIndexInstMap(cmd)

	case UPDATE_INDEX_PARTITION_MAP:
		s.handleUpdateIndexPartnMap(cmd)

	case UPDATE_KEYSPACE_STATS_MAP:
		s.handleUpdateKeyspaceStatsMap(cmd)

	case STORAGE_INDEX_SNAP_REQUEST:
		s.handleGetIndexSnapshot(cmd)

	case STORAGE_INDEX_STORAGE_STATS:
		s.handleGetIndexStorageStats(cmd)

	case STORAGE_INDEX_COMPACT:
		s.handleIndexCompaction(cmd)

	case STORAGE_STATS:
		s.handleStats(cmd)

	case STORAGE_INDEX_MERGE_SNAPSHOT:
		s.handleIndexMergeSnapshot(cmd)

	case STORAGE_INDEX_PRUNE_SNAPSHOT:
		s.handleIndexPruneSnapshot(cmd)

	case STORAGE_UPDATE_SNAP_MAP:
		s.handleUpdateIndexSnapMapForIndex(cmd)

	case INDEXER_ACTIVE:
		s.handleRecoveryDone()

	case CONFIG_SETTINGS_UPDATE:
		s.handleConfigUpdate(cmd)
	}
}

//handleCreateSnapshot will create the necessary snapshots
//after flush has completed
func (s *storageMgr) handleCreateSnapshot(cmd Message) {

	s.supvCmdch <- &MsgSuccess{}

	logging.Tracef("StorageMgr::handleCreateSnapshot %v", cmd)

	msgFlushDone := cmd.(*MsgMutMgrFlushDone)

	keyspaceId := msgFlushDone.GetKeyspaceId()
	tsVbuuid := msgFlushDone.GetTS()
	streamId := msgFlushDone.GetStreamId()
	flushWasAborted := msgFlushDone.GetAborted()
	hasAllSB := msgFlushDone.HasAllSB()

	numVbuckets := s.config["numVbuckets"].Int()
	snapType := tsVbuuid.GetSnapType()
	tsVbuuid.Crc64 = common.HashVbuuid(tsVbuuid.Vbuuids)

	streamKeyspaceIdInstList := s.streamKeyspaceIdInstList.Get()
	instIdList := streamKeyspaceIdInstList[streamId][keyspaceId]

	streamKeyspaceIdInstsPerWorker := s.streamKeyspaceIdInstsPerWorker.Get()
	instsPerWorker := streamKeyspaceIdInstsPerWorker[streamId][keyspaceId]
	// The num_snapshot_workers config has changed. Re-adjust the
	// streamKeyspaceIdInstsPerWorker map according to new snapshot workers
	numSnapshotWorkers := s.getNumSnapshotWorkers()
	if len(instsPerWorker) != numSnapshotWorkers {
		func() {
			s.muSnap.Lock()
			defer s.muSnap.Unlock()

			newStreamKeyspaceIdInstsPerWorker := getStreamKeyspaceIdInstsPerWorker(streamKeyspaceIdInstList, numSnapshotWorkers)
			s.streamKeyspaceIdInstsPerWorker.Set(newStreamKeyspaceIdInstsPerWorker)
			instsPerWorker = newStreamKeyspaceIdInstsPerWorker[streamId][keyspaceId]
			logging.Infof("StorageMgr::handleCreateSnapshot Re-adjusting the streamKeyspaceIdInstsPerWorker map to %v workers. "+
				"StreamId: %v, keyspaceId: %v", numSnapshotWorkers, streamId, keyspaceId)
		}()
	}

	if snapType == common.NO_SNAP || snapType == common.NO_SNAP_OSO {
		logging.Debugf("StorageMgr::handleCreateSnapshot Skip Snapshot For %v "+
			"%v SnapType %v", streamId, keyspaceId, snapType)

		indexInstMap := s.indexInstMap.Get()
		indexPartnMap := s.indexPartnMap.Get()

		go s.flushDone(streamId, keyspaceId, indexInstMap, indexPartnMap,
			instIdList, tsVbuuid, flushWasAborted, hasAllSB)

		return
	}

	s.muSnap.Lock()
	defer s.muSnap.Unlock()

	//pass copy of maps to worker
	indexInstMap := s.indexInstMap.Get()
	indexPartnMap := s.indexPartnMap.Get()
	indexSnapMap := s.indexSnapMap.Get()
	tsVbuuid_copy := tsVbuuid.Copy()
	stats := s.stats.Get()

	go s.createSnapshotWorker(streamId, keyspaceId, tsVbuuid_copy, indexSnapMap,
		numVbuckets, indexInstMap, indexPartnMap, instIdList, instsPerWorker, stats, flushWasAborted, hasAllSB)

}

func (s *storageMgr) createSnapshotWorker(streamId common.StreamId, keyspaceId string,
	tsVbuuid *common.TsVbuuid, indexSnapMap IndexSnapMap, numVbuckets int,
	indexInstMap common.IndexInstMap, indexPartnMap IndexPartnMap,
	instIdList []common.IndexInstId, instsPerWorker [][]common.IndexInstId,
	stats *IndexerStats, flushWasAborted bool, hasAllSB bool) {

	startTime := time.Now().UnixNano()
	var needsCommit bool
	var forceCommit bool
	snapType := tsVbuuid.GetSnapType()
	if snapType == common.DISK_SNAP ||
		snapType == common.DISK_SNAP_OSO {
		needsCommit = true
	} else if snapType == common.FORCE_COMMIT || snapType == common.FORCE_COMMIT_MERGE {
		forceCommit = true
	}

	var wg sync.WaitGroup
	wg.Add(len(instIdList))
	for _, instListPerWorker := range instsPerWorker {
		go func(instList []common.IndexInstId) {
			for _, idxInstId := range instList {
				s.createSnapshotForIndex(streamId, keyspaceId, indexInstMap,
					indexPartnMap, indexSnapMap, numVbuckets, idxInstId, tsVbuuid,
					stats, hasAllSB, flushWasAborted, needsCommit, forceCommit,
					&wg, startTime)
			}
		}(instListPerWorker)
	}

	wg.Wait()

	keyspaceStats := s.stats.GetKeyspaceStats(streamId, keyspaceId)
	end := time.Now().UnixNano()
	if keyspaceStats != nil {
		if keyspaceStats.lastSnapDone.Value() == 0 {
			keyspaceStats.lastSnapDone.Set(end)
		}
		keyspaceStats.snapLatDist.Add(end - keyspaceStats.lastSnapDone.Value())
		keyspaceStats.lastSnapDone.Set(end)
	}

	s.lastFlushDone = end

	s.supvRespch <- &MsgMutMgrFlushDone{mType: STORAGE_SNAP_DONE,
		streamId:   streamId,
		keyspaceId: keyspaceId,
		ts:         tsVbuuid,
		aborted:    flushWasAborted}

}

func (s *storageMgr) createSnapshotForIndex(streamId common.StreamId,
	keyspaceId string, indexInstMap common.IndexInstMap,
	indexPartnMap IndexPartnMap, indexSnapMap IndexSnapMap, numVbuckets int,
	idxInstId common.IndexInstId, tsVbuuid *common.TsVbuuid, stats *IndexerStats,
	hasAllSB bool, flushWasAborted bool, needsCommit bool,
	forceCommit bool, wg *sync.WaitGroup, startTime int64) {

	idxInst := indexInstMap[idxInstId]
	//process only if index belongs to the flushed keyspaceId and stream
	if idxInst.Defn.KeyspaceId(idxInst.Stream) != keyspaceId ||
		idxInst.Stream != streamId ||
		idxInst.State == common.INDEX_STATE_DELETED {
		wg.Done()
		return
	}

	idxStats := stats.indexes[idxInst.InstId]
	snapC := indexSnapMap[idxInstId]
	snapC.Lock()
	lastIndexSnap := CloneIndexSnapshot(snapC.snap)
	defer DestroyIndexSnapshot(lastIndexSnap)
	snapC.Unlock()

	// Signal the wait group first before destroying the snapshot
	// inorder to avoid the cost of destroying the snapshot in the
	// snapshot generation code path
	defer wg.Done()

	// List of snapshots for reading current timestamp
	var isSnapCreated bool = true

	partnSnaps := make(map[common.PartitionId]PartitionSnapshot)
	hasNewSnapshot := false

	partnMap := indexPartnMap[idxInstId]
	//for all partitions managed by this indexer
	for _, partnInst := range partnMap {
		partnId := partnInst.Defn.GetPartitionId()

		var lastPartnSnap PartitionSnapshot

		if lastIndexSnap != nil && len(lastIndexSnap.Partitions()) != 0 {
			lastPartnSnap = lastIndexSnap.Partitions()[partnId]
		}
		sc := partnInst.Sc

		sliceSnaps := make(map[SliceId]SliceSnapshot)
		//create snapshot for all the slices
		for _, slice := range sc.GetAllSlices() {

			if flushWasAborted {
				slice.IsDirty()
				return
			}

			//if TK has seen all Stream Begins after stream restart,
			//the MTR after rollback can be considered successful.
			//All snapshots become eligible to retry for next rollback.
			if hasAllSB {
				slice.SetLastRollbackTs(nil)
			}

			var latestSnapshot Snapshot
			if lastPartnSnap != nil {
				lastSliceSnap := lastPartnSnap.Slices()[slice.Id()]
				latestSnapshot = lastSliceSnap.Snapshot()
			}

			//if flush timestamp is greater than last
			//snapshot timestamp, create a new snapshot
			var snapTs Timestamp
			if latestSnapshot != nil {
				snapTsVbuuid := latestSnapshot.Timestamp()
				snapTs = Timestamp(snapTsVbuuid.Seqnos)
			} else {
				snapTs = NewTimestamp(numVbuckets)
			}

			// Get Seqnos from TsVbuuid
			ts := Timestamp(tsVbuuid.Seqnos)

			//if flush is active for an instance and the flush TS is
			// greater than the last snapshot TS and slice has some changes.
			// Skip only in-memory snapshot in case of unchanged data.
			if latestSnapshot == nil ||
				((slice.IsDirty() || needsCommit) && ts.GreaterThan(snapTs)) ||
				forceCommit {

				newTsVbuuid := tsVbuuid
				var err error
				var info SnapshotInfo
				var newSnapshot Snapshot

				logging.Tracef("StorageMgr::handleCreateSnapshot Creating New Snapshot "+
					"Index: %v PartitionId: %v SliceId: %v Commit: %v Force: %v", idxInstId,
					partnId, slice.Id(), needsCommit, forceCommit)

				if forceCommit {
					needsCommit = forceCommit
				}

				slice.FlushDone()

				snapCreateStart := time.Now()
				if info, err = slice.NewSnapshot(newTsVbuuid, needsCommit); err != nil {
					logging.Errorf("handleCreateSnapshot::handleCreateSnapshot Error "+
						"Creating new snapshot Slice Index: %v Slice: %v. Skipped. Error %v", idxInstId,
						slice.Id(), err)
					isSnapCreated = false
					common.CrashOnError(err)
					continue
				}
				snapCreateDur := time.Since(snapCreateStart)

				hasNewSnapshot = true

				snapOpenStart := time.Now()
				if newSnapshot, err = slice.OpenSnapshot(info); err != nil {
					logging.Errorf("StorageMgr::handleCreateSnapshot Error Creating Snapshot "+
						"for Index: %v Slice: %v. Skipped. Error %v", idxInstId,
						slice.Id(), err)
					isSnapCreated = false
					common.CrashOnError(err)
					continue
				}
				snapOpenDur := time.Since(snapOpenStart)

				if needsCommit {
					logging.Infof("StorageMgr::handleCreateSnapshot Added New Snapshot Index: %v "+
						"PartitionId: %v SliceId: %v Crc64: %v (%v) SnapType %v SnapAligned %v "+
						"SnapCreateDur %v SnapOpenDur %v", idxInstId, partnId, slice.Id(),
						tsVbuuid.Crc64, info, tsVbuuid.GetSnapType(), tsVbuuid.IsSnapAligned(),
						snapCreateDur, snapOpenDur)
				}
				ss := &sliceSnapshot{
					id:   slice.Id(),
					snap: newSnapshot,
				}
				sliceSnaps[slice.Id()] = ss
			} else {
				// Increment reference
				latestSnapshot.Open()
				ss := &sliceSnapshot{
					id:   slice.Id(),
					snap: latestSnapshot,
				}
				sliceSnaps[slice.Id()] = ss

				if logging.IsEnabled(logging.Debug) {
					logging.Debugf("StorageMgr::handleCreateSnapshot Skipped Creating New Snapshot for Index %v "+
						"PartitionId %v SliceId %v. No New Mutations. IsDirty %v", idxInstId, partnId, slice.Id(), slice.IsDirty())
					logging.Debugf("StorageMgr::handleCreateSnapshot SnapTs %v FlushTs %v", snapTs, ts)
				}
				continue
			}
		}

		ps := &partitionSnapshot{
			id:     partnId,
			slices: sliceSnaps,
		}
		partnSnaps[partnId] = ps
	}

	if hasNewSnapshot {
		idxStats.numSnapshots.Add(1)
		if needsCommit {
			idxStats.numCommits.Add(1)
		}
	}

	is := &indexSnapshot{
		instId: idxInstId,
		ts:     tsVbuuid,
		partns: partnSnaps,

		// For debugging
		snapId:       idxStats.numSnapshots.Value(),
		creationTime: uint64(time.Now().UnixNano()),
	}

	if isSnapCreated {
		s.updateSnapMapAndNotify(is, idxStats)
	} else {
		DestroyIndexSnapshot(is)
	}
	s.updateSnapIntervalStat(idxStats, startTime)
}

func (s *storageMgr) flushDone(streamId common.StreamId, keyspaceId string,
	indexInstMap common.IndexInstMap, indexPartnMap IndexPartnMap,
	instIdList []common.IndexInstId, tsVbuuid *common.TsVbuuid,
	flushWasAborted bool, hasAllSB bool) {

	isInitial := func() bool {
		if streamId == common.INIT_STREAM {
			return true
		}

		// TODO (Collections): It is not optimal to iterate over
		// entire instIdList when there are large number of indexes on
		// a node in MAINT_STREAM. This logic needs to be optimised further
		// when writer tuning is enabled
		for _, instId := range instIdList {
			inst, ok := indexInstMap[instId]
			if ok && inst.Stream == streamId &&
				inst.Defn.KeyspaceId(inst.Stream) == keyspaceId &&
				(inst.State == common.INDEX_STATE_INITIAL || inst.State == common.INDEX_STATE_CATCHUP) {
				return true
			}
		}

		return false
	}

	checkInterval := func() int64 {

		if isInitial() {
			return int64(time.Minute)
		}

		return math.MaxInt64
	}

	if common.GetStorageMode() == common.PLASMA {

		if s.config["plasma.writer.tuning.enable"].Bool() &&
			time.Now().UnixNano()-s.lastFlushDone > checkInterval() {

			var wg sync.WaitGroup

			for _, idxInstId := range instIdList {
				wg.Add(1)
				go func(idxInstId common.IndexInstId) {
					defer wg.Done()

					idxInst := indexInstMap[idxInstId]
					if idxInst.Defn.KeyspaceId(idxInst.Stream) == keyspaceId &&
						idxInst.Stream == streamId &&
						idxInst.State != common.INDEX_STATE_DELETED {

						partnMap := indexPartnMap[idxInstId]
						for _, partnInst := range partnMap {
							for _, slice := range partnInst.Sc.GetAllSlices() {
								slice.FlushDone()
							}
						}
					}
				}(idxInstId)
			}

			wg.Wait()
			s.lastFlushDone = time.Now().UnixNano()
		}
	}

	//if TK has seen all Stream Begins after stream restart,
	//the MTR after rollback can be considered successful.
	//All snapshots become eligible to retry for next rollback.
	if hasAllSB {
		for idxInstId, partnMap := range indexPartnMap {
			idxInst := indexInstMap[idxInstId]
			if idxInst.Defn.KeyspaceId(idxInst.Stream) == keyspaceId &&
				idxInst.Stream == streamId &&
				idxInst.State != common.INDEX_STATE_DELETED {
				for _, partnInst := range partnMap {
					for _, slice := range partnInst.Sc.GetAllSlices() {
						slice.SetLastRollbackTs(nil)
					}
				}
			}
		}
	}

	s.supvRespch <- &MsgMutMgrFlushDone{
		mType:      STORAGE_SNAP_DONE,
		streamId:   streamId,
		keyspaceId: keyspaceId,
		ts:         tsVbuuid,
		aborted:    flushWasAborted}
}

func (s *storageMgr) updateSnapIntervalStat(idxStats *IndexStats, startTime int64) {

	// Compute avgTsInterval
	last := idxStats.lastTsTime.Value()
	curr := int64(time.Now().UnixNano())
	avg := idxStats.avgTsInterval.Value()

	avg = common.ComputeAvg(avg, last, curr)
	if avg != 0 {
		idxStats.avgTsInterval.Set(avg)
		idxStats.sinceLastSnapshot.Set(curr - last)
	}
	idxStats.snapGenLatDist.Add(curr - startTime)
	idxStats.lastTsTime.Set(curr)

	idxStats.updateAllPartitionStats(
		func(idxStats *IndexStats) {

			// Compute avgTsItemsCount
			last = idxStats.lastNumFlushQueued.Value()
			curr = idxStats.numDocsFlushQueued.Value()
			avg = idxStats.avgTsItemsCount.Value()

			avg = common.ComputeAvg(avg, last, curr)
			idxStats.avgTsItemsCount.Set(avg)
			idxStats.lastNumFlushQueued.Set(curr)
		})
}

// Update index-snapshot map whenever a snapshot is created for an index
func (s *storageMgr) updateSnapMapAndNotify(is IndexSnapshot, idxStats *IndexStats) {

	var snapC *IndexSnapshotContainer
	var ok, updated bool
	indexSnapMap := s.indexSnapMap.Get()
	if snapC, ok = indexSnapMap[is.IndexInstId()]; !ok {
		func() {
			s.muSnap.Lock()
			defer s.muSnap.Unlock()
			snapC, updated = s.initSnapshotContainerForInst(is.IndexInstId(), is, "updateSnapMapAndNotify")
		}()
	}
	if snapC == nil {
		return
	}

	if updated == false {
		snapC.Lock()
		DestroyIndexSnapshot(snapC.snap)
		snapC.snap = is
		snapC.Unlock()
	}

	// notify a new snapshot through channel
	// the channel receiver needs to destroy snapshot when done
	s.notifySnapshotCreation(is)

	var waitersContainer *SnapshotWaitersContainer
	waiterMap := s.waitersMap.Get()
	if waitersContainer, ok = waiterMap[is.IndexInstId()]; !ok {
		waitersContainer = s.initSnapshotWaitersForInst(is.IndexInstId())
	}

	if waitersContainer == nil {
		return
	}

	waitersContainer.Lock()
	defer waitersContainer.Unlock()
	waiters := waitersContainer.waiters

	var numReplies int64
	t := time.Now()
	// Also notify any waiters for snapshots creation
	var newWaiters []*snapshotWaiter
	for _, w := range waiters {
		// Clean up expired requests from queue
		if !w.expired.IsZero() && t.After(w.expired) {
			snapTs := is.Timestamp()
			logSnapInfoAtTimeout(snapTs, w.ts, is.IndexInstId(), "updateSnapMapAndNotify", idxStats.lastTsTime.Value())
			w.Error(common.ErrScanTimedOut)
			idxStats.numSnapshotWaiters.Add(-1)
			continue
		}

		if isSnapshotConsistent(is, w.cons, w.ts) {
			w.Notify(CloneIndexSnapshot(is))
			numReplies++
			idxStats.numSnapshotWaiters.Add(-1)
			continue
		}
		newWaiters = append(newWaiters, w)
	}
	waitersContainer.waiters = newWaiters
	idxStats.numLastSnapshotReply.Set(numReplies)
}

func (sm *storageMgr) getSortedPartnInst(partnMap PartitionInstMap) partitionInstList {

	if len(partnMap) == 0 {
		return partitionInstList(nil)
	}

	result := make(partitionInstList, 0, len(partnMap))
	for _, partnInst := range partnMap {
		result = append(result, partnInst)
	}

	sort.Sort(result)
	return result
}

//handleRollback will rollback to given timestamp
func (sm *storageMgr) handleRollback(cmd Message) {

	sm.supvCmdch <- &MsgSuccess{}

	// During rollback, some of the snapshot stats get reset
	// or updated by slice. Therefore, serialise rollback and
	// retrieving stats from slice to avoid any inconsistency
	// in stats
	sm.statsLock.Lock()
	defer sm.statsLock.Unlock()

	streamId := cmd.(*MsgRollback).GetStreamId()
	rollbackTs := cmd.(*MsgRollback).GetRollbackTs()
	keyspaceId := cmd.(*MsgRollback).GetKeyspaceId()
	sessionId := cmd.(*MsgRollback).GetSessionId()

	logging.Infof("StorageMgr::handleRollback %v %v rollbackTs %v", streamId, keyspaceId, rollbackTs)

	var err error
	var restartTs *common.TsVbuuid
	var rollbackToZero bool

	indexInstMap := sm.indexInstMap.Get()
	indexPartnMap := sm.indexPartnMap.Get()
	//for every index managed by this indexer
	for idxInstId, partnMap := range indexPartnMap {
		idxInst := indexInstMap[idxInstId]

		//if this keyspace in stream needs to be rolled back
		if idxInst.Defn.KeyspaceId(idxInst.Stream) == keyspaceId &&
			idxInst.Stream == streamId &&
			idxInst.State != common.INDEX_STATE_DELETED {

			restartTs, err = sm.rollbackIndex(streamId,
				keyspaceId, rollbackTs, idxInstId, partnMap, restartTs)

			if err != nil {
				sm.supvRespch <- &MsgRollbackDone{streamId: streamId,
					keyspaceId: keyspaceId,
					err:        err,
					sessionId:  sessionId}
				return
			}

			if restartTs == nil {
				err = sm.rollbackAllToZero(streamId, keyspaceId)
				if err != nil {
					sm.supvRespch <- &MsgRollbackDone{streamId: streamId,
						keyspaceId: keyspaceId,
						err:        err,
						sessionId:  sessionId}
					return
				}
				rollbackToZero = true
				break
			}
		}
	}

	go func() {
		// Notify all scan waiters for indexes in this keyspaceId
		// and stream with error
		stats := sm.stats.Get()
		waitersMap := sm.waitersMap.Get()
		for idxInstId, wc := range waitersMap {
			idxInst := sm.indexInstMap.Get()[idxInstId]
			idxStats := stats.indexes[idxInst.InstId]
			if idxInst.Defn.KeyspaceId(idxInst.Stream) == keyspaceId &&
				idxInst.Stream == streamId {
				wc.Lock()
				for _, w := range wc.waiters {
					w.Error(ErrIndexRollback)
					if idxStats != nil {
						idxStats.numSnapshotWaiters.Add(-1)
					}
				}
				wc.waiters = nil
				wc.Unlock()
			}
		}
	}()

	sm.updateIndexSnapMap(sm.indexPartnMap.Get(), streamId, keyspaceId)

	keyspaceStats := sm.stats.GetKeyspaceStats(streamId, keyspaceId)
	if keyspaceStats != nil {
		keyspaceStats.numRollbacks.Add(1)
		if rollbackToZero {
			keyspaceStats.numRollbacksToZero.Add(1)
		}
	}

	if restartTs != nil {
		//for pre 7.0 index snapshots, the manifestUID needs to be set to epoch
		restartTs.SetEpochManifestUIDIfEmpty()
		restartTs = sm.validateRestartTsVbuuid(keyspaceId, restartTs)
	}

	sm.supvRespch <- &MsgRollbackDone{streamId: streamId,
		keyspaceId: keyspaceId,
		restartTs:  restartTs,
		sessionId:  sessionId,
	}
}

func (sm *storageMgr) rollbackIndex(streamId common.StreamId, keyspaceId string,
	rollbackTs *common.TsVbuuid, idxInstId common.IndexInstId,
	partnMap PartitionInstMap, minRestartTs *common.TsVbuuid) (*common.TsVbuuid, error) {

	var restartTs *common.TsVbuuid
	var err error

	var markAsUsed bool
	if rollbackTs.HasZeroSeqNum() {
		markAsUsed = true
	}

	//for all partitions managed by this indexer
	partnInstList := sm.getSortedPartnInst(partnMap)
	for _, partnInst := range partnInstList {
		partnId := partnInst.Defn.GetPartitionId()
		sc := partnInst.Sc

		for _, slice := range sc.GetAllSlices() {
			snapInfo := sm.findRollbackSnapshot(slice, rollbackTs)

			restartTs, err = sm.rollbackToSnapshot(idxInstId, partnId,
				slice, snapInfo, markAsUsed)

			if err != nil {
				return nil, err
			}

			if restartTs == nil {
				return nil, nil
			}

			//if restartTs is lower than the minimum, use that
			if !restartTs.AsRecentTs(minRestartTs) {
				minRestartTs = restartTs
			}
		}
	}
	return minRestartTs, nil
}

func (sm *storageMgr) findRollbackSnapshot(slice Slice,
	rollbackTs *common.TsVbuuid) SnapshotInfo {

	infos, err := slice.GetSnapshots()
	if err != nil {
		panic("Unable read snapinfo -" + err.Error())
	}
	s := NewSnapshotInfoContainer(infos)

	//DCP doesn't allow using incomplete OSO snapshots
	//for stream restart
	for _, si := range s.List() {
		if si.IsOSOSnap() {
			return nil
		}
	}

	//if dcp has requested rollback to 0 for any vb, it is better to
	//try with all available disk snapshots. The rollback could be
	//due to vbuuid mismatch and using an older disk snapshot may work.
	var snapInfo SnapshotInfo
	if rollbackTs.HasZeroSeqNum() {
		lastRollbackTs := slice.LastRollbackTs()
		latestSnapInfo := s.GetLatest()

		if latestSnapInfo == nil || lastRollbackTs == nil {
			logging.Infof("StorageMgr::handleRollback %v latestSnapInfo %v "+
				"lastRollbackTs %v. Use latest snapshot.", slice.IndexInstId(), latestSnapInfo,
				lastRollbackTs)
			snapInfo = latestSnapInfo
		} else {
			slist := s.List()
			for i, si := range slist {
				if lastRollbackTs.Equal(si.Timestamp()) {
					//if there are more snapshots, use the next one
					if len(slist) >= i+2 {
						snapInfo = slist[i+1]
						logging.Infof("StorageMgr::handleRollback %v Discarding Already Used "+
							"Snapshot %v. Using Next snapshot %v", slice.IndexInstId(), si, snapInfo)
					} else {
						logging.Infof("StorageMgr::handleRollback %v Unable to find a snapshot "+
							"older than last used Snapshot %v. Use nil snapshot.", slice.IndexInstId(),
							latestSnapInfo)
						snapInfo = nil
					}
					break
				} else {
					//if lastRollbackTs is set(i.e. MTR after rollback wasn't completely successful)
					//use only snapshots lower than lastRollbackTs
					logging.Infof("StorageMgr::handleRollback %v Discarding Snapshot %v. Need older "+
						"than last used snapshot %v.", slice.IndexInstId(), si, lastRollbackTs)
				}
			}
		}
	} else {
		snapInfo = s.GetOlderThanTS(rollbackTs)
	}

	return snapInfo

}

func (sm *storageMgr) rollbackToSnapshot(idxInstId common.IndexInstId,
	partnId common.PartitionId, slice Slice, snapInfo SnapshotInfo,
	markAsUsed bool) (*common.TsVbuuid, error) {

	var restartTs *common.TsVbuuid
	if snapInfo != nil {
		err := slice.Rollback(snapInfo)
		if err == nil {
			logging.Infof("StorageMgr::handleRollback Rollback Index: %v "+
				"PartitionId: %v SliceId: %v To Snapshot %v ", idxInstId, partnId,
				slice.Id(), snapInfo)
			restartTs = snapInfo.Timestamp()
			if markAsUsed {
				slice.SetLastRollbackTs(restartTs)
			}
		} else {
			//send error response back
			return nil, err
		}

	} else {
		//if there is no snapshot available, rollback to zero
		err := slice.RollbackToZero()
		if err == nil {
			logging.Infof("StorageMgr::handleRollback Rollback Index: %v "+
				"PartitionId: %v SliceId: %v To Zero ", idxInstId, partnId,
				slice.Id())
			//once rollback to zero has happened, set response ts to nil
			//to represent the initial state of storage
			restartTs = nil
			slice.SetLastRollbackTs(nil)
		} else {
			//send error response back
			return nil, err
		}
	}
	return restartTs, nil
}

func (sm *storageMgr) rollbackAllToZero(streamId common.StreamId,
	keyspaceId string) error {

	logging.Infof("StorageMgr::rollbackAllToZero %v %v", streamId, keyspaceId)

	indexPartnMap := sm.indexPartnMap.Get()
	indexInstMap := sm.indexInstMap.Get()
	for idxInstId, partnMap := range indexPartnMap {
		idxInst := indexInstMap[idxInstId]

		//if this keyspace in stream needs to be rolled back
		if idxInst.Defn.KeyspaceId(idxInst.Stream) == keyspaceId &&
			idxInst.Stream == streamId &&
			idxInst.State != common.INDEX_STATE_DELETED {

			partnInstList := sm.getSortedPartnInst(partnMap)
			for _, partnInst := range partnInstList {
				partnId := partnInst.Defn.GetPartitionId()
				sc := partnInst.Sc

				for _, slice := range sc.GetAllSlices() {
					_, err := sm.rollbackToSnapshot(idxInstId, partnId,
						slice, nil, false)
					if err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (sm *storageMgr) validateRestartTsVbuuid(keyspaceId string,
	restartTs *common.TsVbuuid) *common.TsVbuuid {

	clusterAddr := sm.config["clusterAddr"].String()
	numVbuckets := sm.config["numVbuckets"].Int()

	bucket, _, _ := SplitKeyspaceId(keyspaceId)

	for i := 0; i < MAX_GETSEQS_RETRIES; i++ {

		flog, err := common.BucketFailoverLog(clusterAddr, DEFAULT_POOL,
			bucket, numVbuckets)

		if err != nil {
			logging.Warnf("StorageMgr::validateRestartTsVbuuid Bucket %v. "+
				"Error fetching failover log %v. Retrying(%v).", bucket, err, i+1)
			time.Sleep(time.Second)
			continue
		} else {
			//for each seqnum find the lowest recorded vbuuid in failover log
			//this safeguards in cases memcached loses a vbuuid that was sent
			//to indexer. Note that this cannot help in case memcached loses
			//both mutation and vbuuid.

			for i, seq := range restartTs.Seqnos {
				lowest, err := flog.LowestVbuuid(i, seq)
				if err == nil && lowest != 0 &&
					lowest != restartTs.Vbuuids[i] {
					logging.Infof("StorageMgr::validateRestartTsVbuuid Updating Bucket %v "+
						"Vb %v Seqno %v Vbuuid From %v To %v. Flog %v", bucket, i, seq,
						restartTs.Vbuuids[i], lowest, flog[i])
					restartTs.Vbuuids[i] = lowest
				}
			}
			break
		}
	}
	return restartTs
}

// The caller of this method should acquire muSnap Lock
func (s *storageMgr) initSnapshotContainerForInst(instId common.IndexInstId, is IndexSnapshot,
	caller string) (*IndexSnapshotContainer, bool) {
	indexInstMap := s.indexInstMap.Get()
	if inst, ok := indexInstMap[instId]; !ok || inst.State == common.INDEX_STATE_DELETED {
		return nil, false
	} else {
		indexSnapMap := s.indexSnapMap.Get()
		if sc, ok := indexSnapMap[instId]; ok {
			return sc, false
		}
		var snap IndexSnapshot
		bucket := inst.Defn.Bucket
		creationTime := uint64(time.Now().UnixNano())
		stats := s.stats.Get()
		idxStats := stats.indexes[instId]
		if is == nil {
			ts := common.NewTsVbuuid(bucket, s.config["numVbuckets"].Int())
			snap = &indexSnapshot{
				instId: instId,
				ts:     ts, // nil snapshot should have ZERO Crc64 :)
				epoch:  true,

				// For debugging MB-50006
				snapId:       idxStats.numSnapshots.Value(),
				creationTime: creationTime,
			}
		} else {
			snap = is
		}
		indexSnapMap = s.indexSnapMap.Clone()
		logging.Infof("StorageMgr::updateIndexSnapMapForIndex, New IndexSnapshotContainer is being created "+
			"for indexInst: %v, creation time: %v, caller: %v", instId, creationTime, caller)
		sc := &IndexSnapshotContainer{snap: snap, creationTime: creationTime}
		indexSnapMap[instId] = sc
		s.indexSnapMap.Set(indexSnapMap)
		return sc, true
	}
}

func (s *storageMgr) initSnapshotWaitersForInst(instId common.IndexInstId) *SnapshotWaitersContainer {
	s.muSnap.Lock()
	defer s.muSnap.Unlock()

	indexInstMap := s.indexInstMap.Get()
	if inst, ok := indexInstMap[instId]; !ok || inst.State == common.INDEX_STATE_DELETED {
		return nil
	}
	waitersMap := s.waitersMap.Get()
	var waiterContainer *SnapshotWaitersContainer
	var ok bool

	if waiterContainer, ok = waitersMap[instId]; !ok {
		waitersMap = s.waitersMap.Clone()
		waiterContainer = &SnapshotWaitersContainer{}
		waitersMap[instId] = waiterContainer
		s.waitersMap.Set(waitersMap)
	}
	return waiterContainer
}

func (s *storageMgr) addNilSnapshot(idxInstId common.IndexInstId, bucket string, caller string) {
	indexSnapMap := s.indexSnapMap.Get()
	if _, ok := indexSnapMap[idxInstId]; !ok {
		indexSnapMap := s.indexSnapMap.Clone()
		ts := common.NewTsVbuuid(bucket, s.config["numVbuckets"].Int())
		stats := s.stats.Get()
		idxStats := stats.indexes[idxInstId]
		creationTime := uint64(time.Now().UnixNano())
		snap := &indexSnapshot{
			instId: idxInstId,
			ts:     ts, // nil snapshot should have ZERO Crc64 :)
			epoch:  true,

			// For debugging MB-50006
			snapId:       idxStats.numSnapshots.Value(),
			creationTime: creationTime,
		}

		logging.Infof("StorageMgr::updateIndexSnapMapForIndex, New IndexSnapshotContainer is being created "+
			"for indexInst: %v, creation time: %v, caller: %v", idxInstId, creationTime, caller)
		indexSnapMap[idxInstId] = &IndexSnapshotContainer{snap: snap, creationTime: creationTime}
		s.indexSnapMap.Set(indexSnapMap)
		s.notifySnapshotCreation(snap)
	}
}

func (s *storageMgr) notifySnapshotDeletion(instId common.IndexInstId) {
	defer func() {
		if r := recover(); r != nil {
			logging.Errorf("storageMgr::notifySnapshot %v", r)
		}
	}()

	snap := &indexSnapshot{
		instId: instId,
		ts:     nil, // signal deletion with nil timestamp
	}
	index := uint64(instId) % uint64(len(s.snapshotNotifych))
	s.snapshotNotifych[int(index)] <- snap
}

func (s *storageMgr) notifySnapshotCreation(is IndexSnapshot) {
	defer func() {
		if r := recover(); r != nil {
			logging.Errorf("storageMgr::notifySnapshot %v", r)
		}
	}()

	index := uint64(is.IndexInstId()) % uint64(len(s.snapshotNotifych))
	s.snapshotNotifych[index] <- CloneIndexSnapshot(is)
}

func (s *storageMgr) handleUpdateIndexInstMap(cmd Message) {

	logging.Tracef("StorageMgr::handleUpdateIndexInstMap %v", cmd)
	req := cmd.(*MsgUpdateInstMap)
	indexInstMap := req.GetIndexInstMap()
	copyIndexInstMap := common.CopyIndexInstMap(indexInstMap)
	s.stats.Set(req.GetStatsObject())
	s.indexInstMap.Set(copyIndexInstMap)

	s.muSnap.Lock()
	defer s.muSnap.Unlock()

	indexInstMap = s.indexInstMap.Get()
	waitersMap := s.waitersMap.Clone()
	indexSnapMap := s.indexSnapMap.Clone()

	streamKeyspaceIdInstList := getStreamKeyspaceIdInstListFromInstMap(indexInstMap)
	s.streamKeyspaceIdInstList.Set(streamKeyspaceIdInstList)

	streamKeyspaceIdInstsPerWorker := getStreamKeyspaceIdInstsPerWorker(streamKeyspaceIdInstList, s.getNumSnapshotWorkers())
	s.streamKeyspaceIdInstsPerWorker.Set(streamKeyspaceIdInstsPerWorker)

	// Initialize waitersContainer for newly created instances
	for instId, inst := range indexInstMap {
		if _, ok := waitersMap[instId]; !ok && inst.State != common.INDEX_STATE_DELETED {
			waitersMap[instId] = &SnapshotWaitersContainer{}
		}
	}

	// Remove all snapshot waiters for indexes that do not exist anymore
	for id, wc := range waitersMap {
		if inst, ok := indexInstMap[id]; !ok || inst.State == common.INDEX_STATE_DELETED {
			wc.Lock()
			for _, w := range wc.waiters {
				w.Error(common.ErrIndexNotFound)
			}
			wc.waiters = nil
			delete(waitersMap, id)
			wc.Unlock()
		}
	}

	// Cleanup all invalid index's snapshots
	for idxInstId, snapC := range indexSnapMap {
		if inst, ok := indexInstMap[idxInstId]; !ok || inst.State == common.INDEX_STATE_DELETED {
			snapC.Lock()
			is := snapC.snap
			DestroyIndexSnapshot(is)
			delete(indexSnapMap, idxInstId)
			//set sc.deleted to true to indicate to concurrent readers
			//that this snap container should no longer be used
			snapC.deleted = true

			s.notifySnapshotDeletion(idxInstId)
			snapC.Unlock()
		}
	}

	s.indexSnapMap.Set(indexSnapMap)
	// Add 0 items index snapshots for newly added indexes
	for idxInstId, inst := range indexInstMap {
		if inst.State != common.INDEX_STATE_DELETED {
			s.addNilSnapshot(idxInstId, inst.Defn.Bucket, "handleUpdateIndexInstMap")
		}
	}

	//if manager is not enable, store the updated InstMap in
	//meta file
	if s.config["enableManager"].Bool() == false {

		instMap := indexInstMap

		for id, inst := range instMap {
			inst.Pc = nil
			instMap[id] = inst
		}

		//store indexInstMap in metadata store
		var instBytes bytes.Buffer
		var err error

		enc := gob.NewEncoder(&instBytes)
		err = enc.Encode(instMap)
		if err != nil {
			logging.Errorf("StorageMgr::handleUpdateIndexInstMap \n\t Error Marshalling "+
				"IndexInstMap %v. Err %v", instMap, err)
		}

		if err = s.meta.SetKV([]byte(INST_MAP_KEY_NAME), instBytes.Bytes()); err != nil {
			logging.Errorf("StorageMgr::handleUpdateIndexInstMap \n\tError "+
				"Storing IndexInstMap %v", err)
		}

		s.dbfile.Commit(forestdb.COMMIT_MANUAL_WAL_FLUSH)
	}

	s.supvCmdch <- &MsgSuccess{}
}

func (s *storageMgr) handleUpdateIndexPartnMap(cmd Message) {

	logging.Tracef("StorageMgr::handleUpdateIndexPartnMap %v", cmd)
	indexPartnMap := cmd.(*MsgUpdatePartnMap).GetIndexPartnMap()
	copyIndexPartnMap := CopyIndexPartnMap(indexPartnMap)
	s.indexPartnMap.Set(copyIndexPartnMap)

	s.supvCmdch <- &MsgSuccess{}
}

// handleUpdateKeyspaceStatsMap atomically swaps in the pointer to a new KeyspaceStatsMap.
func (s *storageMgr) handleUpdateKeyspaceStatsMap(cmd Message) {
	logging.Tracef("StorageMgr::handleUpdateKeyspaceStatsMap %v", cmd)
	req := cmd.(*MsgUpdateKeyspaceStatsMap)
	stats := s.stats.Get()
	if stats != nil {
		stats.keyspaceStatsMap.Set(req.GetStatsObject())
	}

	s.supvCmdch <- &MsgSuccess{}
}

// Process req for providing an index snapshot for index scan.
// The request contains atleast-timestamp and the storage
// manager will reply with a index snapshot soon after a
// snapshot meeting requested criteria is available.
// The requester will block wait until the response is
// available.
func (s *storageMgr) handleGetIndexSnapshot(cmd Message) {
	s.supvCmdch <- &MsgSuccess{}
	instId := cmd.(*MsgIndexSnapRequest).GetIndexId()
	index := uint64(instId) % uint64(len(s.snapshotReqCh))
	s.snapshotReqCh[int(index)] <- cmd
}

func (s *storageMgr) listenSnapshotReqs(index int) {
	for cmd := range s.snapshotReqCh[index] {
		func() {
			req := cmd.(*MsgIndexSnapRequest)
			inst, found := s.indexInstMap.Get()[req.GetIndexId()]
			if !found || inst.State == common.INDEX_STATE_DELETED {
				req.respch <- common.ErrIndexNotFound
				return
			}

			stats := s.stats.Get()
			idxStats := stats.indexes[req.GetIndexId()]

			// Return snapshot immediately if a matching snapshot exists already
			// Else add into waiters list so that next snapshot creation event
			// can notify the requester when a snapshot with matching timestamp
			// is available.
			snapC := s.indexSnapMap.Get()[req.GetIndexId()]
			if snapC == nil {
				func() {
					s.muSnap.Lock()
					defer s.muSnap.Unlock()
					snapC, _ = s.initSnapshotContainerForInst(req.GetIndexId(), nil, "listenSnapshotReqs")
				}()
				if snapC == nil {
					req.respch <- common.ErrIndexNotFound
					return
				}
			}

			snapC.Lock()
			//snapC.deleted indicates that the snapshot container belongs to a deleted
			//index and it should no longer be used.
			if snapC.deleted {
				req.respch <- common.ErrIndexNotFound
				snapC.Unlock()
				return
			}
			if isSnapshotConsistent(snapC.snap, req.GetConsistency(), req.GetTS()) {
				req.respch <- CloneIndexSnapshot(snapC.snap)
				snapC.Unlock()
				return
			}
			snapC.Unlock()

			waitersMap := s.waitersMap.Get()

			var waitersContainer *SnapshotWaitersContainer
			var ok bool
			if waitersContainer, ok = waitersMap[req.GetIndexId()]; !ok {
				waitersContainer = s.initSnapshotWaitersForInst(req.GetIndexId())
			}

			if waitersContainer == nil {
				req.respch <- common.ErrIndexNotFound
				return
			}

			w := newSnapshotWaiter(
				req.GetIndexId(), req.GetTS(), req.GetConsistency(),
				req.GetReplyChannel(), req.GetExpiredTime())

			if idxStats != nil {
				idxStats.numSnapshotWaiters.Add(1)
			}

			waitersContainer.Lock()
			defer waitersContainer.Unlock()
			waitersContainer.waiters = append(waitersContainer.waiters, w)
		}()
	}
}

func (s *storageMgr) handleGetIndexStorageStats(cmd Message) {
	s.supvCmdch <- &MsgSuccess{}
	go func() { // Process storage stats asyncronously
		s.statsLock.Lock()
		defer s.statsLock.Unlock()

		req := cmd.(*MsgIndexStorageStats)
		replych := req.GetReplyChannel()
		spec := req.GetStatsSpec()
		stats := s.getIndexStorageStats(spec)
		replych <- stats
	}()
}

func (s *storageMgr) handleStats(cmd Message) {
	s.supvCmdch <- &MsgSuccess{}

	go func() {
		s.statsLock.Lock()
		defer s.statsLock.Unlock()

		req := cmd.(*MsgStatsRequest)
		replych := req.GetReplyChannel()
		storageStats := s.getIndexStorageStats(nil)

		//node level stats
		var numStorageInstances int64
		var totalDataSize, totalDiskSize, totalRecsInMem, totalRecsOnDisk int64
		var avgMutationRate, avgDrainRate, avgDiskBps int64

		stats := s.stats.Get()
		indexInstMap := s.indexInstMap.Get()
		for _, st := range storageStats {
			inst := indexInstMap[st.InstId]
			if inst.State == common.INDEX_STATE_DELETED {
				continue
			}

			numStorageInstances++

			idxStats := stats.GetPartitionStats(st.InstId, st.PartnId)
			// TODO(sarath): Investigate the reason for inconsistent stats map
			// This nil check is a workaround to avoid indexer crashes for now.
			if idxStats != nil {
				idxStats.dataSize.Set(st.Stats.DataSize)
				idxStats.dataSizeOnDisk.Set(st.Stats.DataSizeOnDisk)
				idxStats.logSpaceOnDisk.Set(st.Stats.LogSpace)
				idxStats.diskSize.Set(st.Stats.DiskSize)
				idxStats.memUsed.Set(st.Stats.MemUsed)
				if common.GetStorageMode() != common.MOI {
					if common.GetStorageMode() == common.PLASMA {
						idxStats.fragPercent.Set(int64(st.getPlasmaFragmentation()))
					} else {
						idxStats.fragPercent.Set(int64(st.GetFragmentation()))
					}
				}

				idxStats.getBytes.Set(st.Stats.GetBytes)
				idxStats.insertBytes.Set(st.Stats.InsertBytes)
				idxStats.deleteBytes.Set(st.Stats.DeleteBytes)

				// compute mutation rate
				now := time.Now().UnixNano()
				elapsed := float64(now-idxStats.lastMutateGatherTime.Value()) / float64(time.Second)
				if elapsed > 60 {
					numDocsIndexed := idxStats.numDocsIndexed.Value()
					mutationRate := float64(numDocsIndexed-idxStats.lastNumDocsIndexed.Value()) / elapsed
					idxStats.avgMutationRate.Set(int64((mutationRate + float64(idxStats.avgMutationRate.Value())) / 2))
					idxStats.lastNumDocsIndexed.Set(numDocsIndexed)

					numItemsFlushed := idxStats.numItemsFlushed.Value()
					drainRate := float64(numItemsFlushed-idxStats.lastNumItemsFlushed.Value()) / elapsed
					idxStats.avgDrainRate.Set(int64((drainRate + float64(idxStats.avgDrainRate.Value())) / 2))
					idxStats.lastNumItemsFlushed.Set(numItemsFlushed)

					diskBytes := idxStats.getBytes.Value() + idxStats.insertBytes.Value() + idxStats.deleteBytes.Value()
					diskBps := float64(diskBytes-idxStats.lastDiskBytes.Value()) / elapsed
					idxStats.avgDiskBps.Set(int64((diskBps + float64(idxStats.avgDiskBps.Value())) / 2))
					idxStats.lastDiskBytes.Set(diskBytes)

					logging.Debugf("StorageManager.handleStats: partition %v DiskBps %v avgDiskBps %v drain rate %v",
						st.PartnId, diskBps, idxStats.avgDiskBps.Value(), idxStats.avgDrainRate.Value())

					idxStats.lastMutateGatherTime.Set(now)
				}

				//compute node level stats
				totalDataSize += st.Stats.DataSize
				totalDiskSize += st.Stats.DiskSize
				totalRecsInMem += idxStats.numRecsInMem.Value()
				totalRecsOnDisk += idxStats.numRecsOnDisk.Value()
				avgMutationRate += idxStats.avgMutationRate.Value()
				avgDrainRate += idxStats.avgDrainRate.Value()
				avgDiskBps += idxStats.avgDiskBps.Value()
			}
		}

		stats.totalDataSize.Set(totalDataSize)
		stats.totalDiskSize.Set(totalDiskSize)
		stats.numStorageInstances.Set(numStorageInstances)
		stats.avgMutationRate.Set(avgMutationRate)
		stats.avgDrainRate.Set(avgDrainRate)
		stats.avgDiskBps.Set(avgDiskBps)
		if numStorageInstances > 0 {
			stats.avgResidentPercent.Set(common.ComputePercent(totalRecsInMem, totalRecsOnDisk))
		} else {
			stats.avgResidentPercent.Set(0)
		}

		replych <- true
	}()
}

func (s *storageMgr) getIndexStorageStats(spec *statsSpec) []IndexStorageStats {
	var stats []IndexStorageStats
	var err error
	var sts StorageStatistics

	doPrepare := true

	instIDMap := make(map[common.IndexInstId]bool)
	if spec != nil && spec.indexSpec != nil && len(spec.indexSpec.GetInstances()) > 0 {
		insts := spec.indexSpec.GetInstances()
		for _, instId := range insts {
			instIDMap[instId] = true
		}
	}

	var consumerFilter uint64
	if spec != nil {
		consumerFilter = spec.consumerFilter
	}

	var numIndexes int64
	gStats := s.stats.Get()

	indexInstMap := s.indexInstMap.Get()
	indexPartnMap := s.indexPartnMap.Get()
	for idxInstId, partnMap := range indexPartnMap {

		// If list of instances are specified in the request and the current
		// instance does not match the instance specified in request, do not
		// process storage statistics for that instance
		if len(instIDMap) > 0 {
			if _, ok := instIDMap[idxInstId]; !ok {
				continue
			}
		}

		inst, ok := indexInstMap[idxInstId]
		//skip deleted indexes
		if !ok || inst.State == common.INDEX_STATE_DELETED {
			continue
		}

		numIndexes++

		for _, partnInst := range partnMap {
			var internalData []string
			internalDataMap := make(map[string]interface{})
			var dataSz, dataSzOnDisk, logSpace, diskSz, memUsed, extraSnapDataSize int64
			var getBytes, insertBytes, deleteBytes int64
			var nslices int64
			var needUpgrade = false
			var hasStats = false

			slices := partnInst.Sc.GetAllSlices()
			nslices += int64(len(slices))
			for i, slice := range slices {

				// Increment the ref count before gathering stats. This is to ensure that
				// the instance is not deleted in the middle of gathering stats.
				if !slice.CheckAndIncrRef() {
					continue
				}

				// Prepare stats once
				if doPrepare {
					slice.PrepareStats()
					doPrepare = false
				}

				sts, err = slice.Statistics(consumerFilter)
				slice.DecrRef()

				if err != nil {
					break
				}

				dataSz += sts.DataSize
				dataSzOnDisk += sts.DataSizeOnDisk
				memUsed += sts.MemUsed
				logSpace += sts.LogSpace
				diskSz += sts.DiskSize
				getBytes += sts.GetBytes
				insertBytes += sts.InsertBytes
				deleteBytes += sts.DeleteBytes
				extraSnapDataSize += sts.ExtraSnapDataSize
				internalData = append(internalData, sts.InternalData...)
				if sts.InternalDataMap != nil && len(sts.InternalDataMap) != 0 {
					internalDataMap[fmt.Sprintf("slice_%d", i)] = sts.InternalDataMap
				}
				needUpgrade = needUpgrade || sts.NeedUpgrade

				hasStats = true
			}

			if hasStats && err == nil {
				stat := IndexStorageStats{
					InstId:     idxInstId,
					PartnId:    partnInst.Defn.GetPartitionId(),
					Name:       inst.Defn.Name,
					Bucket:     inst.Defn.Bucket,
					Scope:      inst.Defn.Scope,
					Collection: inst.Defn.Collection,
					Stats: StorageStatistics{
						DataSize:          dataSz,
						DataSizeOnDisk:    dataSzOnDisk,
						LogSpace:          logSpace,
						DiskSize:          diskSz,
						MemUsed:           memUsed,
						GetBytes:          getBytes,
						InsertBytes:       insertBytes,
						DeleteBytes:       deleteBytes,
						ExtraSnapDataSize: extraSnapDataSize,
						NeedUpgrade:       needUpgrade,
						InternalData:      internalData,
						InternalDataMap:   internalDataMap,
					},
				}

				stats = append(stats, stat)
			}
		}
	}
	gStats.numIndexes.Set(numIndexes)

	return stats
}

func (s *storageMgr) handleRecoveryDone() {
	s.supvCmdch <- &MsgSuccess{}

	if common.GetStorageMode() == common.PLASMA {
		RecoveryDone()
	}
}

func (s *storageMgr) handleConfigUpdate(cmd Message) {
	cfgUpdate := cmd.(*MsgConfigUpdate)
	s.config = cfgUpdate.GetConfig()

	s.supvCmdch <- &MsgSuccess{}
}

func (s *storageMgr) handleIndexMergeSnapshot(cmd Message) {
	req := cmd.(*MsgIndexMergeSnapshot)
	srcInstId := req.GetSourceInstId()
	tgtInstId := req.GetTargetInstId()
	partitions := req.GetPartitions()

	var source, target IndexSnapshot
	indexSnapMap := s.indexSnapMap.Get()

	validateSnapshots := func() bool {
		sourceC, ok := indexSnapMap[srcInstId]
		if !ok {
			s.supvCmdch <- &MsgSuccess{}
			return false
		}
		sourceC.Lock()
		defer sourceC.Unlock()

		source = sourceC.snap

		targetC, ok := indexSnapMap[tgtInstId]
		if !ok {
			// increment source snapshot refcount
			target = s.deepCloneIndexSnapshot(source, false, nil)

		} else {
			targetC.Lock()
			defer targetC.Unlock()

			target = targetC.snap
			// Make sure that the source timestamp is greater than or equal to the target timestamp.
			// This comparison will only cover the seqno and vbuuids.
			//
			// Note that even if the index instance has 0 mutation or no new mutation, storage
			// manager will always create a new indexSnapshot with the current timestamp during
			// snapshot.
			//
			// But there is a chance that merge happens before snapshot.  In this case, source
			// could have a higher snapshot than target:
			// 1) source is merged to MAINT stream from INIT stream
			// 2) after (1), there is no flush/snapshot before merge partition happens
			//
			// Here, we just have to make sure that the source has a timestamp at least as high
			// as the target to detect potential data loss.   The merged snapshot will use the
			// target timestamp.    Since target timestamp cannot be higher than source snapshot,
			// there is no risk of data loss.
			//

			if !source.Timestamp().EqualOrGreater(target.Timestamp(), false) {
				logging.Fatalf("StorageMgr::handleIndexMergeSnapshot, Source InstId: %v, sourceC: %+v, Target InstId: %v, targetC: %+v", srcInstId, sourceC, tgtInstId, targetC)
				logging.Fatalf("StorageMgr::handleIndexMergeSnapshot Source InstId: %v, SnapId: %v, creationTime: %v, Target InstId: %v snapId: %v, creationTime: %v",
					source.IndexInstId(), source.SnapId(), source.CreationTime(), target.IndexInstId(), target.SnapId(), target.CreationTime())

				s.supvCmdch <- &MsgError{
					err: Error{code: ERROR_STORAGE_MGR_MERGE_SNAPSHOT_FAIL,
						severity: FATAL,
						category: STORAGE_MGR,
						cause: fmt.Errorf("Timestamp mismatch between snapshot\n target %v\n source %v\n",
							target.Timestamp(), source.Timestamp())}}
				return false
			}

			// source will not have partition snapshot if there is no mutation in bucket.  Skip validation check.
			// If bucket has at least 1 mutation, then source will have partition snapshot.
			if len(source.Partitions()) != 0 {
				// make sure that the source snapshot has all the required partitions
				count := 0
				for _, partnId := range partitions {
					for _, sp := range source.Partitions() {
						if partnId == sp.PartitionId() {
							count++
							break
						}
					}
				}
				if count != len(partitions) || count != len(source.Partitions()) {
					s.supvCmdch <- &MsgError{
						err: Error{code: ERROR_STORAGE_MGR_MERGE_SNAPSHOT_FAIL,
							severity: FATAL,
							category: STORAGE_MGR,
							cause: fmt.Errorf("Source snapshot %v does not have all the required partitions %v",
								srcInstId, partitions)}}
					return false
				}

				// make sure there is no overlapping partition between source and target snapshot
				for _, sp := range source.Partitions() {

					found := false
					for _, tp := range target.Partitions() {
						if tp.PartitionId() == sp.PartitionId() {
							found = true
							break
						}
					}

					if found {
						s.supvCmdch <- &MsgError{
							err: Error{code: ERROR_STORAGE_MGR_MERGE_SNAPSHOT_FAIL,
								severity: FATAL,
								category: STORAGE_MGR,
								cause: fmt.Errorf("Duplicate partition %v found between source %v and target %v",
									sp.PartitionId(), srcInstId, tgtInstId)}}
						return false
					}
				}
			} else {
				logging.Infof("skip validation in merge partitions %v between inst %v and %v", partitions, srcInstId, tgtInstId)
			}

			// Deep clone a new snapshot by copying internal maps + increment target snapshot refcount.
			// The target snapshot could be being used (e.g. under scan).  Increment the snapshot refcount
			// ensure that the snapshot will not get reclaimed.
			target = s.deepCloneIndexSnapshot(target, false, nil)
			if len(partitions) != 0 {
				// Increment source snaphsot refcount (only for copied partitions).  Those snapshots will
				// be copied over to the target snapshot.  Note that the source snapshot can have different
				// refcount than the target snapshot, since the source snapshot may not be used for scanning.
				// But it should be safe to copy from source to target, even if ref count is different.
				source = s.deepCloneIndexSnapshot(source, true, partitions)

				// move the partition in source snapshot to target snapshot
				for _, snap := range source.Partitions() {
					target.Partitions()[snap.PartitionId()] = snap
				}
			}
		}
		return true
	}()

	if !validateSnapshots {
		return
	}

	// decrement source snapshot refcount
	// Do not decrement source snapshot refcount.   When the proxy instance is deleted, storage manager will be notified
	// of the new instance state.   Storage manager will then decrement the ref count at that time.
	//DestroyIndexSnapshot(s.indexSnapMap[srcInstId])

	stats := s.stats.Get()
	idxStats := stats.indexes[tgtInstId]

	// update the target with new snapshot.  This will also decrement target old snapshot refcount.
	s.updateSnapMapAndNotify(target, idxStats)

	s.supvCmdch <- &MsgSuccess{}
}

func (s *storageMgr) handleIndexPruneSnapshot(cmd Message) {
	req := cmd.(*MsgIndexPruneSnapshot)
	instId := req.GetInstId()
	partitions := req.GetPartitions()

	snapC, ok := s.indexSnapMap.Get()[instId]
	if !ok {
		s.supvCmdch <- &MsgSuccess{}
		return
	}
	snapC.Lock()
	snapshot := snapC.snap

	// find the partitions that we want to keep
	kept := make([]common.PartitionId, 0, len(snapshot.Partitions()))
	for _, sp := range snapshot.Partitions() {

		found := false
		for _, partnId := range partitions {
			if partnId == sp.PartitionId() {
				found = true
				break
			}
		}

		if !found {
			kept = append(kept, sp.PartitionId())
		}
	}

	// Increment the snapshot refcount for the partition/slice that we want to keep.
	newSnapshot := s.deepCloneIndexSnapshot(snapshot, true, kept)

	stats := s.stats.Get()
	idxStats := stats.indexes[instId]
	snapC.Unlock()

	s.updateSnapMapAndNotify(newSnapshot, idxStats)

	s.supvCmdch <- &MsgSuccess{}
}

// deepCloneIndexSnapshot makes a clone of a partitioned-index snapshot, but optionally clones only
// a subset of the partition snapshots. It also increments the reference count (i.e. opens) all the
// slices of all the snapshot partitions that do get cloned.
//
// is -- the index shapshot to clone
// doPrune -- false clones ALL partitions and IGNORES the keepPartnIds[] arg. true clones only the
//   subset of partitions listed in the keepPartnIds[] arg.
// keepPartnIds[] -- used ONLY if doPrune == true, this gives the set of partitions whose snapshots
//   are to be cloned, which MAY BE EMPTY OR NIL to indicate pruning away of ALL partitions is
//   desired, in which case none of the partition snapshots are cloned. (This case can occur when a
//   prune is done of all partitions currently in the real instance while there is also an
//   outstanding proxy to be merged into the real instance. Even though all existing partns are
//   moving out, other partns are moving in, so we do a prune of all partitions in the real instance
//   instead of a drop of the index.)
func (s *storageMgr) deepCloneIndexSnapshot(is IndexSnapshot, doPrune bool, keepPartnIds []common.PartitionId) IndexSnapshot {

	snap := is.(*indexSnapshot)
	stats := s.stats.Get()
	idxStats := stats.indexes[snap.instId]

	clone := &indexSnapshot{
		instId: snap.instId,
		ts:     snap.ts.Copy(),
		partns: make(map[common.PartitionId]PartitionSnapshot),

		// For debugging MB-50006
		snapId:       idxStats.numSnapshots.Value(),
		creationTime: uint64(time.Now().UnixNano()),
	}

	// For each partition snapshot...
	for partnId, partnSnap := range snap.Partitions() {

		// Determine if we need to clone this partition snapshot
		doClone := false
		if !doPrune {
			doClone = true
		} else {
			for _, keepPartnId := range keepPartnIds {
				if partnId == keepPartnId {
					doClone = true
					break
				}
			}
		}

		if doClone {
			ps := &partitionSnapshot{
				id:     partnId,
				slices: make(map[SliceId]SliceSnapshot),
			}

			for sliceId, sliceSnap := range partnSnap.Slices() {

				// increment ref count of each slice snapshot
				sliceSnap.Snapshot().Open()
				ps.slices[sliceId] = &sliceSnapshot{
					id:   sliceSnap.SliceId(),
					snap: sliceSnap.Snapshot(),
				}
			}

			clone.partns[partnId] = ps
		}
	}

	return clone
}

func (s *storageMgr) handleIndexCompaction(cmd Message) {
	s.supvCmdch <- &MsgSuccess{}
	req := cmd.(*MsgIndexCompact)
	errch := req.GetErrorChannel()
	abortTime := req.GetAbortTime()
	minFrag := req.GetMinFrag()
	var slices []Slice

	inst, ok := s.indexInstMap.Get()[req.GetInstId()]
	stats := s.stats.Get()
	if !ok || inst.State == common.INDEX_STATE_DELETED {
		errch <- common.ErrIndexNotFound
		return
	}

	partnMap, _ := s.indexPartnMap.Get()[req.GetInstId()]
	idxStats := stats.indexes[req.GetInstId()]
	idxStats.numCompactions.Add(1)

	// Increment rc for slices
	for _, partnInst := range partnMap {
		// non-partitioned index has partitionId 0
		if partnInst.Defn.GetPartitionId() == req.GetPartitionId() {
			for _, slice := range partnInst.Sc.GetAllSlices() {
				slice.IncrRef()
				slices = append(slices, slice)
			}
		}
	}

	// Perform file compaction without blocking storage manager main loop
	go func() {
		for _, slice := range slices {
			err := slice.Compact(abortTime, minFrag)
			slice.DecrRef()
			if err != nil {
				errch <- err
				return
			}
		}

		errch <- nil
	}()
}

// Used for forestdb and memdb slices.
func (s *storageMgr) openSnapshot(idxInstId common.IndexInstId, partnInst PartitionInst,
	partnSnapMap PartnSnapMap) (PartnSnapMap, *common.TsVbuuid, error) {

	pid := partnInst.Defn.GetPartitionId()
	sc := partnInst.Sc

	//there is only one slice for now
	slice := sc.GetSliceById(0)
	infos, err := slice.GetSnapshots()
	// TODO: Proper error handling if possible
	if err != nil {
		panic("Unable to read snapinfo -" + err.Error())
	}

	snapInfoContainer := NewSnapshotInfoContainer(infos)
	allSnapShots := snapInfoContainer.List()

	snapFound := false
	usableSnapFound := false
	var tsVbuuid *common.TsVbuuid
	for _, snapInfo := range allSnapShots {
		snapFound = true
		logging.Infof("StorageMgr::openSnapshot IndexInst:%v Partition:%v Attempting to open snapshot (%v)",
			idxInstId, pid, snapInfo)
		usableSnapshot, err := slice.OpenSnapshot(snapInfo)
		if err != nil {
			if err == errStorageCorrupted {
				// Slice has already cleaned up the snapshot files. Try with older snapshot.
				// Note: plasma and forestdb never return errStorageCorrupted for OpenSnapshot.
				// So, we continue only in case of MOI.
				continue
			} else {
				panic("Unable to open snapshot -" + err.Error())
			}
		}
		ss := &sliceSnapshot{
			id:   SliceId(0),
			snap: usableSnapshot,
		}

		tsVbuuid = snapInfo.Timestamp()

		sid := SliceId(0)

		ps := &partitionSnapshot{
			id:     pid,
			slices: map[SliceId]SliceSnapshot{sid: ss},
		}

		partnSnapMap[pid] = ps
		usableSnapFound = true
		break
	}

	if !snapFound {
		logging.Infof("StorageMgr::openSnapshot IndexInst:%v Partition:%v No Snapshot Found.",
			idxInstId, pid)
		partnSnapMap = nil
		return partnSnapMap, tsVbuuid, nil
	}

	if !usableSnapFound {
		logging.Infof("StorageMgr::openSnapshot IndexInst:%v Partition:%v No Usable Snapshot Found.",
			idxInstId, pid)
		return partnSnapMap, nil, errStorageCorrupted
	}

	return partnSnapMap, tsVbuuid, nil
}

// Update index-snapshot map using index partition map
// This function should be called only during initialization
// of storage manager and during rollback.
// FIXME: Current implementation makes major assumption that
// single slice is supported.
func (s *storageMgr) updateIndexSnapMap(indexPartnMap IndexPartnMap,
	streamId common.StreamId, keyspaceId string) {

	s.muSnap.Lock()
	defer s.muSnap.Unlock()

	for idxInstId, partnMap := range indexPartnMap {
		idxInst := s.indexInstMap.Get()[idxInstId]
		s.updateIndexSnapMapForIndex(idxInstId, idxInst, partnMap, streamId, keyspaceId)
	}
}

// Caller of updateIndexSnapMapForIndex should ensure
// locking and subsequent unlocking of muSnap
func (s *storageMgr) updateIndexSnapMapForIndex(idxInstId common.IndexInstId, idxInst common.IndexInst,
	partnMap PartitionInstMap, streamId common.StreamId, keyspaceId string) {

	needRestart := false
	//if keyspace and stream have been provided
	if keyspaceId != "" && streamId != common.ALL_STREAMS {
		//skip the index if either keyspaceId or stream don't match
		if idxInst.Defn.KeyspaceId(idxInst.Stream) != keyspaceId || idxInst.Stream != streamId {
			return
		}
		//skip deleted indexes
		if idxInst.State == common.INDEX_STATE_DELETED {
			return
		}
	}

	partitionIDs, _ := idxInst.Pc.GetAllPartitionIds()
	logging.Infof("StorageMgr::updateIndexSnapMapForIndex IndexInst %v Partitions %v",
		idxInstId, partitionIDs)

	indexSnapMap := s.indexSnapMap.Clone()
	snapC := indexSnapMap[idxInstId]
	if snapC != nil {
		snapC.Lock()
		DestroyIndexSnapshot(snapC.snap)
		delete(indexSnapMap, idxInstId)
		s.indexSnapMap.Set(indexSnapMap)
		snapC.Unlock()
		s.notifySnapshotDeletion(idxInstId)
	}

	var tsVbuuid *common.TsVbuuid
	var err error
	partnSnapMap := make(PartnSnapMap)

	for _, partnInst := range partnMap {
		partnSnapMap, tsVbuuid, err = s.openSnapshot(idxInstId, partnInst, partnSnapMap)
		if err != nil {
			if err == errStorageCorrupted {
				needRestart = true
			} else {
				panic("Unable to open snapshot -" + err.Error())
			}
		}

		if partnSnapMap == nil {
			break
		}

		//if OSO snapshot, rollback all partitions to 0
		if tsVbuuid != nil && tsVbuuid.GetSnapType() == common.DISK_SNAP_OSO {
			for _, partnInst := range partnMap {
				partnId := partnInst.Defn.GetPartitionId()
				sc := partnInst.Sc

				for _, slice := range sc.GetAllSlices() {
					_, err := s.rollbackToSnapshot(idxInstId, partnId,
						slice, nil, false)
					if err != nil {
						panic("Unable to rollback to 0 - " + err.Error())
					}
				}
			}
			partnSnapMap = nil
			break
		}
	}
	creationTime := uint64(time.Now().UnixNano())
	stats := s.stats.Get()
	idxStats := stats.indexes[idxInstId]
	bucket, _, _ := SplitKeyspaceId(keyspaceId)
	if len(partnSnapMap) != 0 {
		is := &indexSnapshot{
			instId: idxInstId,
			ts:     tsVbuuid,
			partns: partnSnapMap,

			// For debugging MB-50006
			snapId:       idxStats.numSnapshots.Value(),
			creationTime: creationTime,
		}
		indexSnapMap = s.indexSnapMap.Clone()
		if snapC == nil {
			logging.Infof("StorageMgr::updateIndexSnapMapForIndex, New IndexSnapshotContainer is being created "+
				"for indexInst: %v, creation time: %v, caller: %v", idxInstId, creationTime, "updateIndexSnapMapForIndex")
			snapC = &IndexSnapshotContainer{snap: is, creationTime: creationTime}
		} else {
			snapC.Lock()
			snapC.snap = is
			snapC.Unlock()
		}

		indexSnapMap[idxInstId] = snapC
		s.indexSnapMap.Set(indexSnapMap)
		s.notifySnapshotCreation(is)
	} else {
		logging.Infof("StorageMgr::updateIndexSnapMapForIndex IndexInst %v Adding Nil Snapshot.",
			idxInstId)
		s.addNilSnapshot(idxInstId, bucket, "updateIndexSnapMapForIndex")
	}

	if needRestart {
		os.Exit(1)
	}
}

func (s *storageMgr) handleUpdateIndexSnapMapForIndex(cmd Message) {

	req := cmd.(*MsgUpdateSnapMap)
	idxInstId := req.GetInstId()
	idxInst := req.GetInst()
	partnMap := req.GetPartnMap()
	streamId := req.GetStreamId()
	keyspaceId := req.GetKeyspaceId()

	s.muSnap.Lock()
	s.updateIndexSnapMapForIndex(idxInstId, idxInst, partnMap, streamId, keyspaceId)
	s.muSnap.Unlock()

	s.supvCmdch <- &MsgSuccess{}
}

func getStreamKeyspaceIdInstListFromInstMap(indexInstMap common.IndexInstMap) StreamKeyspaceIdInstList {
	out := make(StreamKeyspaceIdInstList)
	for instId, inst := range indexInstMap {
		stream := inst.Stream
		keyspaceId := inst.Defn.KeyspaceId(inst.Stream)
		if _, ok := out[stream]; !ok {
			out[stream] = make(KeyspaceIdInstList)
		}
		out[stream][keyspaceId] = append(out[stream][keyspaceId], instId)
	}
	return out
}

func getStreamKeyspaceIdInstsPerWorker(streamKeyspaceIdInstList StreamKeyspaceIdInstList, numSnapshotWorkers int) StreamKeyspaceIdInstsPerWorker {
	out := make(StreamKeyspaceIdInstsPerWorker)
	for streamId, keyspaceIdInstList := range streamKeyspaceIdInstList {
		out[streamId] = make(KeyspaceIdInstsPerWorker)
		for keyspaceId, instList := range keyspaceIdInstList {
			out[streamId][keyspaceId] = make([][]common.IndexInstId, numSnapshotWorkers)
			//for every index managed by this indexer
			for i, idxInstId := range instList {
				index := i % numSnapshotWorkers
				out[streamId][keyspaceId][index] = append(out[streamId][keyspaceId][index], idxInstId)
			}
		}
	}
	return out
}

func copyIndexSnapMap(inMap IndexSnapMap) IndexSnapMap {

	outMap := make(IndexSnapMap)
	for k, v := range inMap {
		outMap[k] = v
	}
	return outMap

}

func destroyIndexSnapMap(ism IndexSnapMap) {

	for _, v := range ism {
		v.Lock()
		DestroyIndexSnapshot(v.snap)
		v.Unlock()
	}

}

func (s *IndexStorageStats) getPlasmaFragmentation() float64 {
	var fragPercent float64

	var wastedSpace int64
	if s.Stats.DataSizeOnDisk != 0 && s.Stats.LogSpace > s.Stats.DataSizeOnDisk {
		wastedSpace = s.Stats.LogSpace - s.Stats.DataSizeOnDisk
	}

	if s.Stats.LogSpace > 0 {
		fragPercent = float64(wastedSpace) * 100 / float64(s.Stats.LogSpace)
	}

	return fragPercent
}

func (s *storageMgr) getNumSnapshotWorkers() int {
	numSnapshotWorkers := s.config["numSnapshotWorkers"].Int()
	if numSnapshotWorkers < 1 {
		//Since indexer supports upto 10000 indexes in a cluster as of 7.0
		numSnapshotWorkers = 10000
	}
	return numSnapshotWorkers
}
