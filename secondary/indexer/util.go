//  Copyright (c) 2014 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.

package indexer

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/couchbase/indexing/secondary/common"
	"github.com/couchbase/indexing/secondary/logging"
)

const (
	MAX_GETSEQS_RETRIES = 10
)

func IsIPLocal(ip string) bool {

	netIP := net.ParseIP(ip)

	//if loopback address, return true
	if netIP.IsLoopback() {
		return true
	}

	//compare with the local ip
	if localIP, err := GetLocalIP(); err == nil {
		if localIP.Equal(netIP) {
			return true
		}
	}

	return false

}

func GetLocalIP() (net.IP, error) {

	tt, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, t := range tt {
		aa, err := t.Addrs()
		if err != nil {
			return nil, err
		}
		for _, a := range aa {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			v4 := ipnet.IP.To4()
			if v4 == nil || v4[0] == 127 { // loopback address
				continue
			}
			return v4, nil
		}
	}
	return nil, errors.New("cannot find local IP address")
}

func IndexPath(inst *common.IndexInst, partnId common.PartitionId, sliceId SliceId) string {
	instId := inst.InstId
	if inst.IsProxy() {
		instId = inst.RealInstId
	}
	return fmt.Sprintf("%s_%s_%d_%d.index", inst.Defn.Bucket, inst.Defn.Name, instId, partnId)
}

// This has to follow the pattern in IndexPath function defined above.
func GetIndexPathPattern() string {
	return "*_*_*_*.index"
}

// This has to follow the pattern in IndexPath function defined above.
func GetInstIdPartnIdFromPath(idxPath string) (common.IndexInstId,
	common.PartitionId, error) {

	pathComponents := strings.Split(idxPath, "_")
	if len(pathComponents) < 4 {
		err := errors.New(fmt.Sprintf("Malformed index path %v", idxPath))
		return common.IndexInstId(0), common.PartitionId(0), err
	}

	strInstId := pathComponents[len(pathComponents)-2]
	instId, err := strconv.ParseUint(strInstId, 10, 64)
	if err != nil {
		return common.IndexInstId(0), common.PartitionId(0), err
	}

	partnComponents := strings.Split(pathComponents[len(pathComponents)-1], ".")
	if len(partnComponents) != 2 {
		err := errors.New(fmt.Sprintf("Malformed index path %v", idxPath))
		return common.IndexInstId(0), common.PartitionId(0), err
	}

	strPartnId := partnComponents[0]
	partnId, err := strconv.ParseUint(strPartnId, 10, 64)
	if err != nil {
		return common.IndexInstId(0), common.PartitionId(0), err
	}

	return common.IndexInstId(instId), common.PartitionId(partnId), nil
}

func GetRealIndexInstId(inst *common.IndexInst) common.IndexInstId {
	instId := inst.InstId
	if inst.IsProxy() {
		instId = inst.RealInstId
	}
	return instId
}

func GetCurrentKVTs(cluster, pooln, bucketn, collId string, numVbs int) (Timestamp, error) {

	var seqnos []uint64

	fn := func(r int, err error) error {
		if r > 0 {
			logging.Warnf("Indexer::getCurrentKVTs error=%v Retrying (%d)", err, r)
		}

		//if collection id has not been specified, use bucket level
		if collId == "" {
			seqnos, err = common.BucketSeqnos(cluster, pooln, bucketn)
		} else {
			seqnos, err = common.CollectionSeqnos(cluster, pooln, bucketn, collId)
		}

		return err
	}

	start := time.Now()
	rh := common.NewRetryHelper(MAX_GETSEQS_RETRIES, time.Millisecond, 1, fn)
	err := rh.Run()

	if err != nil {
		// then log an error and give-up
		fmsg := "Indexer::getCurrentKVTs Error Connecting to KV Cluster %v"
		logging.Errorf(fmsg, err)
		return nil, err
	}
	if len(seqnos) < numVbs {
		fmsg := "BucketSeqnos(): got ts only for %v vbs"
		return nil, fmt.Errorf(fmsg, len(seqnos))
	}

	ts := NewTimestamp(numVbs)
	for i := 0; i < numVbs; i++ {
		ts[i] = seqnos[i]
	}

	elapsed := time.Since(start)
	logging.Verbosef("Indexer::getCurrentKVTs Time Taken %v", elapsed)
	return ts, err
}

func ValidateBucket(cluster, bucket string, uuids []string) bool {

	var cinfo *common.ClusterInfoCache
	url, err := common.ClusterAuthUrl(cluster)
	if err == nil {
		cinfo, err = common.NewClusterInfoCache(url, DEFAULT_POOL)
	}
	if err != nil {
		logging.Fatalf("Indexer::Fail to init ClusterInfoCache : %v", err)
		common.CrashOnError(err)
	}

	cinfo.Lock()
	defer cinfo.Unlock()

	if err := cinfo.Fetch(); err != nil {
		logging.Errorf("Indexer::Fail to init ClusterInfoCache : %v", err)
		common.CrashOnError(err)
	}

	if nids, err := cinfo.GetNodesByBucket(bucket); err == nil && len(nids) != 0 {
		// verify UUID
		currentUUID := cinfo.GetBucketUUID(bucket)
		for _, uuid := range uuids {
			if uuid != currentUUID {
				return false
			}
		}
		return true
	} else {
		logging.Fatalf("Indexer::Error Fetching Bucket Info: %v Nids: %v", err, nids)
		return false
	}

}

func IsEphemeral(cluster, bucket string) (bool, error) {
	var cinfo *common.ClusterInfoCache
	url, err := common.ClusterAuthUrl(cluster)
	if err == nil {
		cinfo, err = common.NewClusterInfoCache(url, DEFAULT_POOL)
	}
	if err != nil {
		logging.Fatalf("Indexer::Fail to init ClusterInfoCache : %v", err)
		common.CrashOnError(err)
	}

	cinfo.Lock()
	defer cinfo.Unlock()

	if err := cinfo.Fetch(); err != nil {
		logging.Errorf("Indexer::Fail to init ClusterInfoCache : %v", err)
		common.CrashOnError(err)
	}

	return cinfo.IsEphemeral(bucket)
}

//flip bits in-place for a given byte slice
func FlipBits(code []byte) {

	for i, b := range code {
		code[i] = ^b
	}
	return
}

func clusterVersion(clusterAddr string) uint64 {

	var cinfo *common.ClusterInfoCache
	url, err := common.ClusterAuthUrl(clusterAddr)
	if err != nil {
		return common.INDEXER_45_VERSION
	}

	cinfo, err = common.NewClusterInfoCache(url, DEFAULT_POOL)
	if err != nil {
		return common.INDEXER_45_VERSION
	}

	cinfo.Lock()
	defer cinfo.Unlock()

	if err := cinfo.Fetch(); err != nil {
		return common.INDEXER_45_VERSION
	}

	return cinfo.GetClusterVersion()
}
