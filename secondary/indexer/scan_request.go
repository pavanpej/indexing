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
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/couchbase/indexing/secondary/collatejson"
	"github.com/couchbase/indexing/secondary/common"
	protobuf "github.com/couchbase/indexing/secondary/protobuf/query"

	"github.com/couchbase/indexing/secondary/logging"
	"github.com/couchbase/query/expression"
	"github.com/couchbase/query/expression/parser"
	"github.com/couchbase/query/value"
)

type ScanReqType string

const (
	StatsReq          ScanReqType = "stats"
	CountReq                      = "count"
	ScanReq                       = "scan"
	ScanAllReq                    = "scanAll"
	HeloReq                       = "helo"
	MultiScanCountReq             = "multiscancount"
	FastCountReq                  = "fastcountreq" //generated internally
)

type ScanRequest struct {
	ScanType     ScanReqType
	DefnID       uint64
	IndexInstId  common.IndexInstId
	IndexName    string
	Bucket       string
	CollectionId string
	PartitionIds []common.PartitionId
	Ts           *common.TsVbuuid
	Low          IndexKey
	High         IndexKey
	Keys         []IndexKey
	Consistency  *common.Consistency
	Stats        *IndexStats
	IndexInst    common.IndexInst

	Ctxs []IndexReaderContext

	// user supplied
	LowBytes, HighBytes []byte
	KeysBytes           [][]byte

	Incl      Inclusion
	Limit     int64
	isPrimary bool

	// New parameters for API2 pushdowns
	Scans             []Scan
	Indexprojection   *Projection
	Reverse           bool
	Distinct          bool
	Offset            int64
	projectPrimaryKey bool

	//groupby/aggregate

	GroupAggr *GroupAggr

	//below two arrays indicate what parts of composite keys
	//need to be exploded and decoded. explodeUpto indicates
	//maximum position of explode or decode
	explodePositions []bool
	decodePositions  []bool
	explodeUpto      int

	// New parameters for partitioned index
	Sorted bool

	// Rollback Time
	rollbackTime int64

	ScanId      uint64
	ExpiredTime time.Time
	Timeout     *time.Timer
	CancelCh    <-chan bool

	RequestId string
	LogPrefix string

	keyBufList      []*[]byte
	indexKeyBuffer  []byte
	sharedBuffer    *[]byte
	sharedBufferLen int

	hasRollback *atomic.Value

	sco *scanCoordinator

	connCtx *ConnectionContext

	dataEncFmt common.DataEncodingFormat
	keySzCfg   keySizeConfig
}

type Projection struct {
	projectSecKeys   bool
	projectionKeys   []bool
	entryKeysEmpty   bool
	projectGroupKeys []projGroup
}

type projGroup struct {
	pos    int
	grpKey bool
}

type Scan struct {
	Low      IndexKey  // Overall Low for a Span. Computed from composite filters (Ranges)
	High     IndexKey  // Overall High for a Span. Computed from composite filters (Ranges)
	Incl     Inclusion // Overall Inclusion for a Span
	ScanType ScanFilterType
	Filters  []Filter // A collection qualifying filters
	Equals   IndexKey // TODO: Remove Equals
}

type Filter struct {
	// If composite index has n keys,
	// it will have <= n CompositeElementFilters
	CompositeFilters []CompositeElementFilter
	Low              IndexKey
	High             IndexKey
	Inclusion        Inclusion
	ScanType         ScanFilterType
}

type ScanFilterType string

// RangeReq is a span which is Range on the entire index
// without composite index filtering
// FilterRangeReq is a span request which needs composite
// index filtering
const (
	AllReq         ScanFilterType = "scanAll"
	LookupReq                     = "lookup"
	RangeReq                      = "range"       // Range with no filtering
	FilterRangeReq                = "filterRange" // Range with filtering
)

// Range for a single field in composite index
type CompositeElementFilter struct {
	Low       IndexKey
	High      IndexKey
	Inclusion Inclusion
}

// A point in index and the corresponding filter
// the point belongs to either as high or low
type IndexPoint struct {
	Value    IndexKey
	FilterId int
	Type     string
}

// Implements sort Interface
type IndexPoints []IndexPoint

// Implements sort Interface
type Filters []Filter

//Groupby/Aggregate pushdown

type GroupKey struct {
	EntryKeyId int32                 // Id that can be used in IndexProjection
	KeyPos     int32                 // >=0 means use expr at index key position otherwise use Expr
	Expr       expression.Expression // group expression
	ExprValue  value.Value           // Is non-nil if expression is constant
}

type Aggregate struct {
	AggrFunc   common.AggrFuncType   // Aggregate operation
	EntryKeyId int32                 // Id that can be used in IndexProjection
	KeyPos     int32                 // >=0 means use expr at index key position otherwise use Expr
	Expr       expression.Expression // Aggregate expression
	ExprValue  value.Value           // Is non-nil if expression is constant
	Distinct   bool                  // Aggregate only on Distinct values with in the group
}

type GroupAggr struct {
	Name                string       // name of the index aggregate
	Group               []*GroupKey  // group keys, nil means no group by
	Aggrs               []*Aggregate // aggregates with in the group, nil means no aggregates
	DependsOnIndexKeys  []int32      // GROUP and Aggregates Depends on List of index keys positions
	IndexKeyNames       []string     // Index key names used in expressions
	DependsOnPrimaryKey bool
	AllowPartialAggr    bool // Partial aggregates are allowed
	OnePerPrimaryKey    bool // Leading Key is ALL & equality span consider one per docid

	IsLeadingGroup     bool // Group by key(s) are leading subset
	IsPrimary          bool
	NeedDecode         bool // Need decode values for SUM or N1QLExpr evaluation
	NeedExplode        bool // If only constant expression
	HasExpr            bool // Has a non constant expression
	FirstValidAggrOnly bool // Scan storage entries upto first valid value - MB-27861

	//For caching values
	cv          *value.ScopeValue
	av          value.AnnotatedValue
	exprContext expression.Context
	aggrs       []*aggrVal
	groups      []*groupKey
}

func (ga GroupAggr) String() string {
	str := "Groups: "
	for _, g := range ga.Group {
		str += fmt.Sprintf(" %+v ", g)
	}

	str += "Aggregates: "
	for _, a := range ga.Aggrs {
		str += fmt.Sprintf(" %+v ", a)
	}

	str += fmt.Sprintf(" DependsOnIndexKeys %v", ga.DependsOnIndexKeys)
	str += fmt.Sprintf(" IndexKeyNames %v", ga.IndexKeyNames)
	str += fmt.Sprintf(" NeedDecode %v", ga.NeedDecode)
	str += fmt.Sprintf(" NeedExplode %v", ga.NeedExplode)
	str += fmt.Sprintf(" IsLeadingGroup %v", ga.IsLeadingGroup)
	return str
}

func (g GroupKey) String() string {
	str := "Group: "
	str += fmt.Sprintf(" EntryKeyId %v", g.EntryKeyId)
	str += fmt.Sprintf(" KeyPos %v", g.KeyPos)
	str += fmt.Sprintf(" Expr %v", logging.TagUD(g.Expr))
	str += fmt.Sprintf(" ExprValue %v", logging.TagUD(g.ExprValue))
	return str
}

func (a Aggregate) String() string {
	str := "Aggregate: "
	str += fmt.Sprintf(" AggrFunc %v", a.AggrFunc)
	str += fmt.Sprintf(" EntryKeyId %v", a.EntryKeyId)
	str += fmt.Sprintf(" KeyPos %v", a.KeyPos)
	str += fmt.Sprintf(" Expr %v", logging.TagUD(a.Expr))
	str += fmt.Sprintf(" ExprValue %v", logging.TagUD(a.ExprValue))
	str += fmt.Sprintf(" Distinct %v", a.Distinct)
	return str
}

var (
	ErrInvalidAggrFunc = errors.New("Invalid Aggregate Function")
)

var inclusionMatrix = [][]Inclusion{
	{Neither, High},
	{Low, Both},
}

/////////////////////////////////////////////////////////////////////////
//
//  scan request implementation
//
/////////////////////////////////////////////////////////////////////////

func NewScanRequest(protoReq interface{}, ctx interface{},
	cancelCh <-chan bool, s *scanCoordinator) (r *ScanRequest, err error) {

	r = new(ScanRequest)
	r.ScanId = atomic.AddUint64(&s.reqCounter, 1)
	r.LogPrefix = fmt.Sprintf("SCAN##%d", r.ScanId)
	r.sco = s

	cfg := s.config.Load()
	timeout := time.Millisecond * time.Duration(cfg["settings.scan_timeout"].Int())

	if timeout != 0 {
		r.ExpiredTime = time.Now().Add(timeout)
		r.Timeout = time.NewTimer(timeout)
	}

	r.CancelCh = cancelCh

	r.projectPrimaryKey = true

	if ctx == nil {
		r.connCtx = createConnectionContext().(*ConnectionContext)
	} else {
		r.connCtx = ctx.(*ConnectionContext)
	}

	r.keySzCfg = getKeySizeConfig(cfg)

	switch req := protoReq.(type) {
	case *protobuf.HeloRequest:
		r.ScanType = HeloReq
	case *protobuf.StatisticsRequest:
		r.DefnID = req.GetDefnID()
		r.RequestId = req.GetRequestId()
		r.ScanType = StatsReq
		r.Incl = Inclusion(req.GetSpan().GetRange().GetInclusion())
		r.Sorted = true
		if err = r.setIndexParams(); err != nil {
			return
		}

		err = r.fillRanges(
			req.GetSpan().GetRange().GetLow(),
			req.GetSpan().GetRange().GetHigh(),
			req.GetSpan().GetEquals())
		if err != nil {
			return
		}

	case *protobuf.CountRequest:
		r.DefnID = req.GetDefnID()
		r.RequestId = req.GetRequestId()
		r.rollbackTime = req.GetRollbackTime()
		r.PartitionIds = makePartitionIds(req.GetPartitionIds())
		cons := common.Consistency(req.GetCons())
		vector := req.GetVector()
		r.ScanType = CountReq
		r.Incl = Inclusion(req.GetSpan().GetRange().GetInclusion())
		r.Sorted = true

		if err = r.setIndexParams(); err != nil {
			return
		}

		if err = r.setConsistency(cons, vector); err != nil {
			return
		}

		err = r.fillRanges(
			req.GetSpan().GetRange().GetLow(),
			req.GetSpan().GetRange().GetHigh(),
			req.GetSpan().GetEquals())
		if err != nil {
			return
		}

		sc := req.GetScans()
		if len(sc) != 0 {
			err = r.fillScans(sc)
			r.ScanType = MultiScanCountReq
			r.Distinct = req.GetDistinct()
		}
		if err != nil {
			return
		}

	case *protobuf.ScanRequest:
		r.DefnID = req.GetDefnID()
		r.RequestId = req.GetRequestId()
		r.rollbackTime = req.GetRollbackTime()
		r.PartitionIds = makePartitionIds(req.GetPartitionIds())
		cons := common.Consistency(req.GetCons())
		vector := req.GetVector()
		r.ScanType = ScanReq
		r.Incl = Inclusion(req.GetSpan().GetRange().GetInclusion())
		r.Limit = req.GetLimit()
		r.Sorted = req.GetSorted()
		r.Reverse = req.GetReverse()
		proj := req.GetIndexprojection()
		r.dataEncFmt = common.DataEncodingFormat(req.GetDataEncFmt())
		if proj == nil {
			r.Distinct = req.GetDistinct()
		}
		r.Offset = req.GetOffset()

		if err = r.setIndexParams(); err != nil {
			return
		}

		if err = r.setConsistency(cons, vector); err != nil {
			return
		}

		if proj != nil {
			var localerr error
			if req.GetGroupAggr() == nil {
				if r.Indexprojection, localerr = validateIndexProjection(proj, len(r.IndexInst.Defn.SecExprs)); localerr != nil {
					err = localerr
					return
				}
				r.projectPrimaryKey = *proj.PrimaryKey
			} else {
				if r.Indexprojection, localerr = validateIndexProjectionGroupAggr(proj, req.GetGroupAggr()); localerr != nil {
					err = localerr
					return
				}
				r.projectPrimaryKey = false
			}
		}
		err = r.fillRanges(
			req.GetSpan().GetRange().GetLow(),
			req.GetSpan().GetRange().GetHigh(),
			req.GetSpan().GetEquals())
		if err != nil {
			return
		}
		if err = r.fillScans(req.GetScans()); err != nil {
			return
		}

		if err = r.fillGroupAggr(req.GetGroupAggr(), req.GetScans()); err != nil {
			return
		}
		r.setExplodePositions()

	case *protobuf.ScanAllRequest:
		r.DefnID = req.GetDefnID()
		r.RequestId = req.GetRequestId()
		r.rollbackTime = req.GetRollbackTime()
		r.PartitionIds = makePartitionIds(req.GetPartitionIds())
		cons := common.Consistency(req.GetCons())
		vector := req.GetVector()
		r.ScanType = ScanAllReq
		r.Limit = req.GetLimit()
		r.Scans = make([]Scan, 1)
		r.Scans[0].ScanType = AllReq
		r.Sorted = true
		r.dataEncFmt = common.DataEncodingFormat(req.GetDataEncFmt())

		if err = r.setIndexParams(); err != nil {
			return
		}

		if err = r.setConsistency(cons, vector); err != nil {
			return
		}
	default:
		err = ErrUnsupportedRequest
	}

	return
}

func (r *ScanRequest) getTimeoutCh() <-chan time.Time {
	if r.Timeout != nil {
		return r.Timeout.C
	}

	return nil
}

func (r *ScanRequest) Done() {
	// If the requested DefnID in invalid, stats object will not be populated
	if r.Stats != nil {
		r.Stats.numCompletedRequests.Add(1)
		if r.GroupAggr != nil {
			r.Stats.numCompletedRequestsAggr.Add(1)
		} else {
			r.Stats.numCompletedRequestsRange.Add(1)
		}

		for _, partitionId := range r.PartitionIds {
			r.Stats.updatePartitionStats(partitionId,
				func(stats *IndexStats) {
					stats.numCompletedRequests.Add(1)
				})
		}
	}

	for _, buf := range r.keyBufList {
		secKeyBufPool.Put(buf)
	}

	r.keyBufList = nil

	if r.Timeout != nil {
		r.Timeout.Stop()
	}
}

func (r *ScanRequest) isNil(k []byte) bool {
	if k == nil || (!r.isPrimary && string(k) == "[]") {
		return true
	}
	return false
}

func (r *ScanRequest) newKey(k []byte) (IndexKey, error) {
	if k == nil {
		return nil, fmt.Errorf("Key is null")
	}

	if r.isPrimary {
		return NewPrimaryKey(k)
	} else {
		return NewSecondaryKey(k, r.getKeyBuffer(3*len(k)), r.keySzCfg.allowLargeKeys, r.keySzCfg.maxSecKeyLen)
	}
}

func (r *ScanRequest) newLowKey(k []byte) (IndexKey, error) {
	if r.isNil(k) {
		return MinIndexKey, nil
	}

	return r.newKey(k)
}

func (r *ScanRequest) newHighKey(k []byte) (IndexKey, error) {
	if r.isNil(k) {
		return MaxIndexKey, nil
	}

	return r.newKey(k)
}

func (r *ScanRequest) fillRanges(low, high []byte, keys [][]byte) (localErr error) {
	var key IndexKey

	// range
	r.LowBytes = low
	r.HighBytes = high

	if r.Low, localErr = r.newLowKey(low); localErr != nil {
		localErr = fmt.Errorf("Invalid low key %s (%s)", string(low), localErr)
		return
	}

	if r.High, localErr = r.newHighKey(high); localErr != nil {
		localErr = fmt.Errorf("Invalid high key %s (%s)", string(high), localErr)
		return
	}

	// point query for keys
	for _, k := range keys {
		r.KeysBytes = append(r.KeysBytes, k)
		if key, localErr = r.newKey(k); localErr != nil {
			localErr = fmt.Errorf("Invalid equal key %s (%s)", string(k), localErr)
			return
		}
		r.Keys = append(r.Keys, key)
	}
	return
}

func (r *ScanRequest) joinKeys(keys [][]byte) ([]byte, error) {
	buf1 := r.getSharedBuffer(len(keys) * 3)
	joined, e := jsonEncoder.JoinArray(keys, buf1)
	if e != nil {
		e = fmt.Errorf("Error in joining keys: %s", e)
		return nil, e
	}
	r.sharedBufferLen += len(joined)
	return joined, nil
}

func (r *ScanRequest) areFiltersNil(protoScan *protobuf.Scan) bool {
	areFiltersNil := true
	for _, filter := range protoScan.Filters {
		if !r.isNil(filter.Low) || !r.isNil(filter.High) {
			areFiltersNil = false
			break
		}
	}
	return areFiltersNil
}

func (r *ScanRequest) getEmptyScan() Scan {
	key, _ := r.newKey([]byte(""))
	return Scan{Low: key, High: key, Incl: Neither, ScanType: RangeReq}
}

// Compute the overall low, high for a Filter
// based on composite filter ranges
func (r *ScanRequest) fillFilterLowHigh(compFilters []CompositeElementFilter, filter *Filter) error {
	if !r.IndexInst.Defn.HasDescending() {
		var lows, highs [][]byte
		var e error
		joinLowKey, joinHighKey := true, true

		if compFilters[0].Low == MinIndexKey {
			filter.Low = MinIndexKey
			joinLowKey = false
		}
		if compFilters[0].High == MaxIndexKey {
			filter.High = MaxIndexKey
			joinHighKey = false
		}

		var l, h []byte
		codec := collatejson.NewCodec(16)
		if joinLowKey {
			for _, f := range compFilters {
				if f.Low == MinIndexKey {
					break
				}
				lows = append(lows, f.Low.Bytes())
			}

			buf1 := r.getSharedBuffer(len(lows) * 3)
			if l, e = codec.JoinArray(lows, buf1); e != nil {
				e = fmt.Errorf("Error in forming low key %s", e)
				return e
			}
			r.sharedBufferLen += len(l)
			lowKey := secondaryKey(l)
			filter.Low = &lowKey
		}
		if joinHighKey {
			for _, f := range compFilters {
				if f.High == MaxIndexKey {
					break
				}
				highs = append(highs, f.High.Bytes())
			}

			buf2 := r.getSharedBuffer(len(highs) * 3)
			if h, e = codec.JoinArray(highs, buf2); e != nil {
				e = fmt.Errorf("Error in forming high key %s", e)
				return e
			}
			r.sharedBufferLen += len(h)
			highKey := secondaryKey(h)
			filter.High = &highKey
		}
		return nil
	}

	//********** Reverse Collation fix **********//

	// Step 1: Form lows and highs keys
	var lows, highs []IndexKey
	for _, f := range compFilters {
		lows = append(lows, f.Low)
		highs = append(highs, f.High)
	}

	// Step 2: Exchange lows and highs depending on []desc
	var lows2, highs2 []IndexKey
	for i, _ := range compFilters {
		if r.IndexInst.Defn.Desc[i] {
			lows2 = append(lows2, highs[i])
			highs2 = append(highs2, lows[i])
		} else {
			lows2 = append(lows2, lows[i])
			highs2 = append(highs2, highs[i])
		}
	}

	// Step 3: Prune lows2 and highs2 if Min/Max present
	for i, l := range lows2 {
		if l == MinIndexKey || l == MaxIndexKey {
			lows2 = lows2[:i]
			break
		}
	}
	for i, h := range highs2 {
		if h == MinIndexKey || h == MaxIndexKey {
			highs2 = highs2[:i]
			break
		}
	}

	// Step 4: Join lows2 and highs2
	var joinedLow, joinedHigh []byte
	var e error
	var lowKey, highKey IndexKey
	if len(lows2) > 0 {
		var lows2bytes [][]byte
		for _, l := range lows2 {
			lows2bytes = append(lows2bytes, l.Bytes())
		}
		if joinedLow, e = r.joinKeys(lows2bytes); e != nil {
			return e
		}
		lowKey, e = getReverseCollatedIndexKey(joinedLow, r.IndexInst.Defn.Desc[:len(lows2)])
		if e != nil {
			return e
		}
	} else {
		lowKey = MinIndexKey
	}

	if len(highs2) > 0 {
		var highs2bytes [][]byte
		for _, l := range highs2 {
			highs2bytes = append(highs2bytes, l.Bytes())
		}
		if joinedHigh, e = r.joinKeys(highs2bytes); e != nil {
			return e
		}
		highKey, e = getReverseCollatedIndexKey(joinedHigh, r.IndexInst.Defn.Desc[:len(highs2)])
		if e != nil {
			return e
		}
	} else {
		highKey = MaxIndexKey
	}
	filter.Low = lowKey
	filter.High = highKey
	//********** End of Reverse Collation fix **********//

	// TODO: Calculate the right inclusion
	// Right now using Both inclusion
	return nil
}

func (r *ScanRequest) fillFilterEquals(protoScan *protobuf.Scan, filter *Filter) error {
	var e error
	var equals [][]byte
	for _, k := range protoScan.Equals {
		var key IndexKey
		if key, e = r.newKey(k); e != nil {
			e = fmt.Errorf("Invalid equal key %s (%s)", string(k), e)
			return e
		}
		equals = append(equals, key.Bytes())
	}

	codec := collatejson.NewCodec(16)
	buf1 := r.getSharedBuffer(len(equals) * 3)
	var equalsKey, eqReverse []byte
	if equalsKey, e = codec.JoinArray(equals, buf1); e != nil {
		e = fmt.Errorf("Error in forming equals key %s", e)
		return e
	}
	r.sharedBufferLen += len(equalsKey)
	if !r.IndexInst.Defn.HasDescending() {
		eqReverse = equalsKey
	} else {
		eqReverse, e = jsonEncoder.ReverseCollate(equalsKey, r.IndexInst.Defn.Desc[:len(equals)])
		if e != nil {
			return e
		}
	}
	eqKey := secondaryKey(eqReverse)

	var compFilters []CompositeElementFilter
	for _, k := range equals {
		ek := secondaryKey(k)
		fl := CompositeElementFilter{
			Low:       &ek,
			High:      &ek,
			Inclusion: Both,
		}
		compFilters = append(compFilters, fl)
	}

	filter.Low = &eqKey
	filter.High = &eqKey
	filter.Inclusion = Both
	filter.CompositeFilters = compFilters
	filter.ScanType = LookupReq
	return nil
}

///// Compose Scans for Secondary Index
// Create scans from sorted Index Points
// Iterate over sorted points and keep track of applicable filters
// between overlapped regions
func (r *ScanRequest) composeScans(points []IndexPoint, filters []Filter) []Scan {

	var scans []Scan
	filtersMap := make(map[int]bool)
	var filtersList []int
	var low IndexKey
	for _, p := range points {
		if len(filtersMap) == 0 {
			low = p.Value
		}
		filterid := p.FilterId
		if _, present := filtersMap[filterid]; present {
			delete(filtersMap, filterid)
			if len(filtersMap) == 0 { // Empty filtersMap indicates end of overlapping region
				if len(scans) > 0 &&
					scans[len(scans)-1].High.ComparePrefixIndexKey(low) == 0 {
					// If high of previous scan == low of next scan, then
					// merge the filters instead of creating a new scan
					for _, fl := range filtersList {
						scans[len(scans)-1].Filters = append(scans[len(scans)-1].Filters, filters[fl])
					}
					scans[len(scans)-1].High = p.Value
					filtersList = nil
				} else {
					scan := Scan{
						Low:      low,
						High:     p.Value,
						Incl:     Both,
						ScanType: FilterRangeReq,
					}
					for _, fl := range filtersList {
						scan.Filters = append(scan.Filters, filters[fl])
					}

					scans = append(scans, scan)
					filtersList = nil
				}
			}
		} else {
			filtersMap[filterid] = true
			filtersList = append(filtersList, filterid)
		}
	}
	for i, _ := range scans {
		if len(scans[i].Filters) == 1 && scans[i].Filters[0].ScanType == LookupReq {
			scans[i].Equals = scans[i].Low
			scans[i].ScanType = LookupReq
		}

		if scans[i].ScanType == FilterRangeReq && len(scans[i].Filters) == 1 &&
			len(scans[i].Filters[0].CompositeFilters) == 1 {
			// Flip inclusion if first element is descending
			scans[i].Incl = flipInclusion(scans[i].Filters[0].CompositeFilters[0].Inclusion, r.IndexInst.Defn.Desc)
			scans[i].ScanType = RangeReq
		}
		// TODO: Optimzation if single CEF in all filters (for both primary and secondary)
	}

	return scans
}

///// Compose Scans for Primary Index
func lowInclude(lowInclusions []Inclusion) int {
	for _, incl := range lowInclusions {
		if incl == Low || incl == Both {
			return 1
		}
	}
	return 0
}

func highInclude(highInclusions []Inclusion) int {
	for _, incl := range highInclusions {
		if incl == High || incl == Both {
			return 1
		}
	}
	return 0
}

func MergeFiltersForPrimary(scans []Scan, f2 Filter) []Scan {

	getNewScans := func(scans []Scan, f Filter) []Scan {
		sc := Scan{Low: f.Low, High: f.High, Incl: f.Inclusion, ScanType: RangeReq}
		scans = append(scans, sc)
		return scans
	}

	if len(scans) > 0 {
		f1 := scans[len(scans)-1]
		l1, h1, i1 := f1.Low, f1.High, f1.Incl
		l2, h2, i2 := f2.Low, f2.High, f2.Inclusion

		//No Merge casess
		if l2.ComparePrefixIndexKey(h1) > 0 {
			return getNewScans(scans, f2)
		}
		if (h1.ComparePrefixIndexKey(l2) == 0) &&
			!(i1 == High || i1 == Both || i2 == Low || i2 == Both) {
			return getNewScans(scans, f2)
		}

		// Merge cases
		var low, high IndexKey
		inclLow, inclHigh := 0, 0
		if l1.ComparePrefixIndexKey(l2) == 0 {
			low = l1
			inclLow = lowInclude([]Inclusion{i1, i2})
		}
		if h1.ComparePrefixIndexKey(h2) == 0 {
			high = h1
			inclHigh = highInclude([]Inclusion{i1, i2})
		}
		if low == nil {
			if l1.ComparePrefixIndexKey(l2) < 0 {
				low = l1
				inclLow = lowInclude([]Inclusion{i1})
			} else {
				low = l2
				inclLow = lowInclude([]Inclusion{i2})
			}
		}
		if high == nil {
			if h1.ComparePrefixIndexKey(h2) > 0 {
				high = h1
				inclHigh = highInclude([]Inclusion{i1})
			} else {
				high = h2
				inclHigh = highInclude([]Inclusion{i2})
			}
		}
		f1.Low, f1.High = low, high
		f1.Incl = inclusionMatrix[inclLow][inclHigh]
		scans[len(scans)-1] = f1
		return scans
	}
	return getNewScans(scans, f2)
}

///// END - Compose Scans for Primary Index

func (r *ScanRequest) fillScans(protoScans []*protobuf.Scan) (localErr error) {
	var l, h IndexKey

	// For Upgrade
	if len(protoScans) == 0 {
		r.Scans = make([]Scan, 1)
		if len(r.Keys) > 0 {
			r.Scans[0].Equals = r.Keys[0] //TODO fix for multiple Keys needed?
			r.Scans[0].ScanType = LookupReq
		} else {
			r.Scans[0].Low = r.Low
			r.Scans[0].High = r.High
			r.Scans[0].Incl = r.Incl
			r.Scans[0].ScanType = RangeReq
		}
		return
	}

	// Array of Filters
	var filters []Filter
	var points []IndexPoint

	if r.isPrimary {
		var scans []Scan
		for _, protoScan := range protoScans {
			if len(protoScan.Equals) != 0 {
				var filter Filter
				var key IndexKey
				if key, localErr = r.newKey(protoScan.Equals[0]); localErr != nil {
					localErr = fmt.Errorf("Invalid equal key %s (%s)", string(protoScan.Equals[0]), localErr)
					return
				}
				filter.Low = key
				filter.High = key
				filter.Inclusion = Both
				filters = append(filters, filter)

				p1 := IndexPoint{Value: filter.Low, FilterId: len(filters) - 1, Type: "low"}
				p2 := IndexPoint{Value: filter.High, FilterId: len(filters) - 1, Type: "high"}
				points = append(points, p1, p2)
				continue
			}

			// If there are no filters in scan, it is ScanAll
			if len(protoScan.Filters) == 0 {
				r.Scans = make([]Scan, 1)
				r.Scans[0] = getScanAll()
				return
			}

			// if all scan filters are (nil, nil), it is ScanAll
			if r.areFiltersNil(protoScan) {
				r.Scans = make([]Scan, 1)
				r.Scans[0] = getScanAll()
				return
			}

			fl := protoScan.Filters[0]
			if l, localErr = r.newLowKey(fl.Low); localErr != nil {
				localErr = fmt.Errorf("Invalid low key %s (%s)", logging.TagStrUD(fl.Low), localErr)
				return
			}

			if h, localErr = r.newHighKey(fl.High); localErr != nil {
				localErr = fmt.Errorf("Invalid high key %s (%s)", logging.TagStrUD(fl.High), localErr)
				return
			}

			if IndexKeyLessThan(h, l) {
				scans = append(scans, r.getEmptyScan())
				continue
			}

			// When l == h, only valid case is: meta().id >= l && meta().id <= h
			if l.CompareIndexKey(h) == 0 && Inclusion(fl.GetInclusion()) != Both {
				scans = append(scans, r.getEmptyScan())
				continue
			}

			compfil := CompositeElementFilter{
				Low:       l,
				High:      h,
				Inclusion: Inclusion(fl.GetInclusion()),
			}

			filter := Filter{
				CompositeFilters: []CompositeElementFilter{compfil},
				Inclusion:        compfil.Inclusion,
				Low:              l,
				High:             h,
			}
			filters = append(filters, filter)
		}
		// Sort Filters based only on low value
		sort.Sort(Filters(filters))
		for _, filter := range filters {
			scans = MergeFiltersForPrimary(scans, filter)
		}
		r.Scans = scans
		return
	} else {
		for _, protoScan := range protoScans {
			skipScan := false
			if len(protoScan.Equals) != 0 {
				//Encode the equals keys
				var filter Filter
				if localErr = r.fillFilterEquals(protoScan, &filter); localErr != nil {
					return
				}
				filters = append(filters, filter)

				p1 := IndexPoint{Value: filter.Low, FilterId: len(filters) - 1, Type: "low"}
				p2 := IndexPoint{Value: filter.High, FilterId: len(filters) - 1, Type: "high"}
				points = append(points, p1, p2)
				continue
			}

			// If there are no filters in scan, it is ScanAll
			if len(protoScan.Filters) == 0 {
				r.Scans = make([]Scan, 1)
				r.Scans[0] = getScanAll()
				return
			}

			// if all scan filters are (nil, nil), it is ScanAll
			if r.areFiltersNil(protoScan) {
				r.Scans = make([]Scan, 1)
				r.Scans[0] = getScanAll()
				return
			}

			var compFilters []CompositeElementFilter
			// Encode Filters
			for _, fl := range protoScan.Filters {
				if l, localErr = r.newLowKey(fl.Low); localErr != nil {
					localErr = fmt.Errorf("Invalid low key %s (%s)", logging.TagStrUD(fl.Low), localErr)
					return
				}

				if h, localErr = r.newHighKey(fl.High); localErr != nil {
					localErr = fmt.Errorf("Invalid high key %s (%s)", logging.TagStrUD(fl.High), localErr)
					return
				}

				if IndexKeyLessThan(h, l) {
					skipScan = true
					break
				}

				compfil := CompositeElementFilter{
					Low:       l,
					High:      h,
					Inclusion: Inclusion(fl.GetInclusion()),
				}
				compFilters = append(compFilters, compfil)
			}

			if skipScan {
				continue
			}

			filter := Filter{
				CompositeFilters: compFilters,
				Inclusion:        Both,
			}

			if localErr = r.fillFilterLowHigh(compFilters, &filter); localErr != nil {
				return
			}

			filters = append(filters, filter)

			p1 := IndexPoint{Value: filter.Low, FilterId: len(filters) - 1, Type: "low"}
			p2 := IndexPoint{Value: filter.High, FilterId: len(filters) - 1, Type: "high"}
			points = append(points, p1, p2)

			// TODO: Does single Composite Element Filter
			// mean no filtering? Revisit single CEF
		}
	}

	// Sort Index Points
	sort.Sort(IndexPoints(points))
	r.Scans = r.composeScans(points, filters)
	return
}

// Populate list of positions of keys which need to be
// exploded for composite filtering and index projection
func (r *ScanRequest) setExplodePositions() {

	if r.isPrimary {
		return
	}

	maxCompositeFilters := 0
	for _, sc := range r.Scans {
		if sc.ScanType != FilterRangeReq {
			continue
		}

		for _, fl := range sc.Filters {
			num := len(fl.CompositeFilters)
			if num > maxCompositeFilters {
				maxCompositeFilters = num
			}
		}
	}

	if r.explodePositions == nil {
		r.explodePositions = make([]bool, len(r.IndexInst.Defn.SecExprs))
		r.decodePositions = make([]bool, len(r.IndexInst.Defn.SecExprs))
	}

	for i := 0; i < maxCompositeFilters; i++ {
		r.explodePositions[i] = true
	}

	if r.Indexprojection != nil && r.Indexprojection.projectSecKeys {
		for i, project := range r.Indexprojection.projectionKeys {
			if project {
				r.explodePositions[i] = true
			}
		}
	}

	// Set max position until which we need explode or decode
	for i := 0; i < len(r.explodePositions); i++ {
		if r.explodePositions[i] || r.decodePositions[i] {
			r.explodeUpto = i
		}
	}
}

func (r *ScanRequest) setConsistency(cons common.Consistency, vector *protobuf.TsConsistency) (localErr error) {

	r.Consistency = &cons
	cfg := r.sco.config.Load()
	if cons == common.QueryConsistency && vector != nil {
		r.Ts = common.NewTsVbuuid(r.Bucket, cfg["numVbuckets"].Int())
		// if vector == nil, it is similar to AnyConsistency
		for i, vbno := range vector.Vbnos {
			r.Ts.Seqnos[vbno] = vector.Seqnos[i]
			r.Ts.Vbuuids[vbno] = vector.Vbuuids[i]
		}
	} else if cons == common.SessionConsistency {
		cluster := cfg["clusterAddr"].String()
		r.Ts = &common.TsVbuuid{}
		t0 := time.Now()
		r.Ts.Seqnos, localErr = bucketSeqsWithRetry(cfg["settings.scan_getseqnos_retries"].Int(),
			r.LogPrefix, cluster, r.Bucket, cfg["numVbuckets"].Int(), r.CollectionId,
			cfg["use_bucket_seqnos"].Bool())
		if localErr == nil && r.Stats != nil {
			r.Stats.Timings.dcpSeqs.Put(time.Since(t0))
		}
		r.Ts.Crc64 = 0
		r.Ts.Bucket = r.Bucket
	}
	return
}

func (r *ScanRequest) setIndexParams() (localErr error) {
	r.sco.mu.RLock()
	defer r.sco.mu.RUnlock()

	var indexInst *common.IndexInst

	stats := r.sco.stats.Get()
	indexInst, r.Ctxs, localErr = r.sco.findIndexInstance(r.DefnID, r.PartitionIds)
	if localErr == nil {
		r.isPrimary = indexInst.Defn.IsPrimary
		r.IndexName, r.Bucket = indexInst.Defn.Name, indexInst.Defn.Bucket
		r.CollectionId = indexInst.Defn.CollectionId
		r.IndexInstId = indexInst.InstId
		r.IndexInst = *indexInst

		if indexInst.State != common.INDEX_STATE_ACTIVE {
			localErr = common.ErrIndexNotReady
		}
		r.Stats = stats.indexes[r.IndexInstId]
		rbMap := *r.sco.getRollbackInProgress()
		r.hasRollback = rbMap[indexInst.Defn.Bucket]
	}
	return
}

func validateIndexProjection(projection *protobuf.IndexProjection, cklen int) (*Projection, error) {
	if len(projection.EntryKeys) > cklen {
		e := errors.New(fmt.Sprintf("Invalid number of Entry Keys %v in IndexProjection", len(projection.EntryKeys)))
		return nil, e
	}

	projectionKeys := make([]bool, cklen)
	for _, position := range projection.EntryKeys {
		if position >= int64(cklen) || position < 0 {
			e := errors.New(fmt.Sprintf("Invalid Entry Key %v in IndexProjection", position))
			return nil, e
		}
		projectionKeys[position] = true
	}

	projectAllSecKeys := true
	for _, sp := range projectionKeys {
		if sp == false {
			projectAllSecKeys = false
		}
	}

	indexProjection := &Projection{}
	indexProjection.projectSecKeys = !projectAllSecKeys
	indexProjection.projectionKeys = projectionKeys
	indexProjection.entryKeysEmpty = len(projection.EntryKeys) == 0

	return indexProjection, nil
}

func validateIndexProjectionGroupAggr(projection *protobuf.IndexProjection, protoGroupAggr *protobuf.GroupAggr) (*Projection, error) {

	nproj := len(projection.GetEntryKeys())

	if nproj <= 0 {
		return nil, errors.New("Grouping without projection is not supported")
	}

	projGrp := make([]projGroup, nproj)
	var found bool
	for i, entryId := range projection.GetEntryKeys() {

		found = false
		for j, g := range protoGroupAggr.GetGroupKeys() {
			if entryId == int64(g.GetEntryKeyId()) {
				projGrp[i] = projGroup{pos: j, grpKey: true}
				found = true
				break
			}
		}

		if found {
			continue
		}

		for j, a := range protoGroupAggr.GetAggrs() {
			if entryId == int64(a.GetEntryKeyId()) {
				projGrp[i] = projGroup{pos: j, grpKey: false}
				found = true
				break
			}
		}

		if !found {
			return nil, errors.New(fmt.Sprintf("Projection EntryId %v not found in any Group/Aggregate %v", entryId, protoGroupAggr))
		}

	}

	indexProjection := &Projection{}
	indexProjection.projectGroupKeys = projGrp
	indexProjection.projectSecKeys = true

	return indexProjection, nil
}

func (r *ScanRequest) fillGroupAggr(protoGroupAggr *protobuf.GroupAggr, protoScans []*protobuf.Scan) (err error) {

	if protoGroupAggr == nil {
		return nil
	}

	if r.explodePositions == nil {
		r.explodePositions = make([]bool, len(r.IndexInst.Defn.SecExprs))
		r.decodePositions = make([]bool, len(r.IndexInst.Defn.SecExprs))
	}

	r.GroupAggr = &GroupAggr{}

	if err = r.unmarshallGroupKeys(protoGroupAggr); err != nil {
		return
	}

	if err = r.unmarshallAggrs(protoGroupAggr); err != nil {
		return
	}

	if r.isPrimary {
		r.GroupAggr.IsPrimary = true
	}

	for _, d := range protoGroupAggr.GetDependsOnIndexKeys() {
		r.GroupAggr.DependsOnIndexKeys = append(r.GroupAggr.DependsOnIndexKeys, d)
		if !r.isPrimary && int(d) == len(r.IndexInst.Defn.SecExprs) {
			r.GroupAggr.DependsOnPrimaryKey = true
		}
	}

	for _, d := range protoGroupAggr.GetIndexKeyNames() {
		r.GroupAggr.IndexKeyNames = append(r.GroupAggr.IndexKeyNames, string(d))
	}

	r.GroupAggr.AllowPartialAggr = protoGroupAggr.GetAllowPartialAggr()
	r.GroupAggr.OnePerPrimaryKey = protoGroupAggr.GetOnePerPrimaryKey()

	if err = r.validateGroupAggr(); err != nil {
		return
	}

	// Look at groupAggr.DependsOnIndexKeys to figure out
	// explode and decode positions for N1QL expression dependencies
	if !r.isPrimary && r.GroupAggr.HasExpr {
		for _, depends := range r.GroupAggr.DependsOnIndexKeys {
			if int(depends) == len(r.IndexInst.Defn.SecExprs) {
				continue //Expr depends on meta().id, so ignore
			}
			r.explodePositions[depends] = true
			r.decodePositions[depends] = true
		}
	}

	cfg := r.sco.config.Load()
	if cfg["scan.enable_fast_count"].Bool() {
		if r.canUseFastCount(protoScans) {
			r.ScanType = FastCountReq
		}
	}

	return
}

func (r *ScanRequest) unmarshallGroupKeys(protoGroupAggr *protobuf.GroupAggr) error {

	for _, g := range protoGroupAggr.GetGroupKeys() {

		var groupKey GroupKey

		groupKey.EntryKeyId = g.GetEntryKeyId()
		groupKey.KeyPos = g.GetKeyPos()

		if groupKey.KeyPos < 0 {
			if string(g.GetExpr()) == "" {
				return errors.New("Group expression is empty")
			}
			expr, err := compileN1QLExpression(string(g.GetExpr()))
			if err != nil {
				return err
			}
			groupKey.Expr = expr
			groupKey.ExprValue = expr.Value() // value will be nil if it is not constant expr
			if groupKey.ExprValue == nil {
				r.GroupAggr.HasExpr = true
				r.GroupAggr.NeedDecode = true
				r.GroupAggr.NeedExplode = true
			}
			if r.GroupAggr.cv == nil {
				r.GroupAggr.cv = value.NewScopeValue(make(map[string]interface{}), nil)
				r.GroupAggr.av = value.NewAnnotatedValue(r.GroupAggr.cv)
				r.GroupAggr.exprContext = expression.NewIndexContext()
			}
		} else {
			r.GroupAggr.NeedExplode = true
			if !r.isPrimary {
				r.explodePositions[groupKey.KeyPos] = true
			}
		}

		r.GroupAggr.Group = append(r.GroupAggr.Group, &groupKey)
	}

	return nil

}

func (r *ScanRequest) unmarshallAggrs(protoGroupAggr *protobuf.GroupAggr) error {

	for _, a := range protoGroupAggr.GetAggrs() {

		var aggr Aggregate

		aggr.AggrFunc = common.AggrFuncType(a.GetAggrFunc())
		aggr.EntryKeyId = a.GetEntryKeyId()
		aggr.KeyPos = a.GetKeyPos()
		aggr.Distinct = a.GetDistinct()

		if aggr.KeyPos < 0 {
			if string(a.GetExpr()) == "" {
				return errors.New("Aggregate expression is empty")
			}
			expr, err := compileN1QLExpression(string(a.GetExpr()))
			if err != nil {
				return err
			}
			aggr.Expr = expr
			aggr.ExprValue = expr.Value() // value will be nil if it is not constant expr
			if aggr.ExprValue == nil {
				r.GroupAggr.HasExpr = true
				r.GroupAggr.NeedDecode = true
				r.GroupAggr.NeedExplode = true
			}
			if r.GroupAggr.cv == nil {
				r.GroupAggr.cv = value.NewScopeValue(make(map[string]interface{}), nil)
				r.GroupAggr.av = value.NewAnnotatedValue(r.GroupAggr.cv)
				r.GroupAggr.exprContext = expression.NewIndexContext()
			}
		} else {
			if aggr.AggrFunc == common.AGG_SUM {
				r.GroupAggr.NeedDecode = true
				if !r.isPrimary {
					r.decodePositions[aggr.KeyPos] = true
				}
			}
			r.GroupAggr.NeedExplode = true
			if !r.isPrimary {
				r.explodePositions[aggr.KeyPos] = true
			}
		}

		r.GroupAggr.Aggrs = append(r.GroupAggr.Aggrs, &aggr)
	}

	return nil

}

func (r *ScanRequest) validateGroupAggr() error {

	if r.isPrimary {
		return nil
	}

	//identify leading/non-leading
	var prevPos int32 = -1
	r.GroupAggr.IsLeadingGroup = true

outerloop:
	for _, g := range r.GroupAggr.Group {
		if g.KeyPos < 0 {
			r.GroupAggr.IsLeadingGroup = false
			break
		} else if g.KeyPos == 0 {
			prevPos = 0
		} else {
			if g.KeyPos != prevPos+1 {
				for prevPos < g.KeyPos-1 {
					prevPos++
					if !r.hasAllEqualFilters(int(prevPos)) {
						prevPos--
						break
					}
				}
				if g.KeyPos != prevPos+1 {
					r.GroupAggr.IsLeadingGroup = false
					break outerloop
				}
			}
		}
		prevPos = g.KeyPos
	}

	var err error

	if !r.GroupAggr.AllowPartialAggr && !r.GroupAggr.IsLeadingGroup {
		err = fmt.Errorf("Requested Partial Aggr %v Not Supported For Given Scan", r.GroupAggr.AllowPartialAggr)
		logging.Errorf("ScanRequest::validateGroupAggr %v ", err)
		return err
	}

	//validate aggregates
	for _, a := range r.GroupAggr.Aggrs {
		if a.AggrFunc >= common.AGG_INVALID {
			logging.Errorf("ScanRequest::validateGroupAggr %v %v", ErrInvalidAggrFunc, a.AggrFunc)
			return ErrInvalidAggrFunc
		}
		if int(a.KeyPos) >= len(r.IndexInst.Defn.SecExprs) {
			err = fmt.Errorf("Invalid KeyPos In Aggr %v", a)
			logging.Errorf("ScanRequest::validateGroupAggr %v", err)
			return err
		}
	}

	//validate group by
	for _, g := range r.GroupAggr.Group {
		if int(g.KeyPos) >= len(r.IndexInst.Defn.SecExprs) {
			err = fmt.Errorf("Invalid KeyPos In GroupKey %v", g)
			logging.Errorf("ScanRequest::validateGroupAggr %v", err)
			return err
		}
	}

	//validate DependsOnIndexKeys
	for _, k := range r.GroupAggr.DependsOnIndexKeys {
		if int(k) > len(r.IndexInst.Defn.SecExprs) {
			err = fmt.Errorf("Invalid KeyPos In DependsOnIndexKeys %v", k)
			logging.Errorf("ScanRequest::validateGroupAggr %v", err)
			return err
		}
	}

	r.GroupAggr.FirstValidAggrOnly = r.processFirstValidAggrOnly()
	return nil
}

// Scan needs to process only first valid aggregate value
// if below rules are satisfied. It is an optimization added for MB-27861
func (r *ScanRequest) processFirstValidAggrOnly() bool {

	if len(r.GroupAggr.Group) != 0 {
		return false
	}

	if len(r.GroupAggr.Aggrs) != 1 {
		return false
	}

	aggr := r.GroupAggr.Aggrs[0]

	if aggr.AggrFunc != common.AGG_MIN &&
		aggr.AggrFunc != common.AGG_MAX &&
		aggr.AggrFunc != common.AGG_COUNT {
		return false
	}

	checkEqualityFilters := func(keyPos int32) bool {
		if keyPos < 0 {
			return false
		}
		if keyPos == 0 {
			return true
		}

		// If keyPos > 0, check if there is more than 1 span
		// In case of multiple spans, do not apply the optimization
		if len(r.Scans) > 1 {
			return false
		}

		return r.hasAllEqualFiltersUpto(int(keyPos) - 1)
	}

	isAscKey := func(keyPos int32) bool {
		if !r.IndexInst.Defn.HasDescending() {
			return true
		}
		if r.IndexInst.Defn.Desc[keyPos] {
			return false
		}
		return true
	}

	if aggr.AggrFunc == common.AGG_MIN {
		if !checkEqualityFilters(aggr.KeyPos) {
			return false
		}

		return isAscKey(aggr.KeyPos)
	}

	if aggr.AggrFunc == common.AGG_MAX {
		if !checkEqualityFilters(aggr.KeyPos) {
			return false
		}

		return !isAscKey(aggr.KeyPos)
	}

	// Rule applies for COUNT(DISTINCT const_expr)
	if aggr.AggrFunc == common.AGG_COUNT {
		if aggr.ExprValue != nil && aggr.Distinct {
			return true
		}
		return false
	}

	return false
}

func (r *ScanRequest) canUseFastCount(protoScans []*protobuf.Scan) bool {

	//only one aggregate
	if len(r.GroupAggr.Aggrs) != 1 {
		return false
	}

	//no group by
	if len(r.GroupAggr.Group) != 0 {
		return false
	}

	//ignore array index
	if r.IndexInst.Defn.IsArrayIndex {
		return false
	}

	//ignore primary index
	if r.IndexInst.Defn.IsPrimary {
		return false
	}

	aggr := r.GroupAggr.Aggrs[0]

	//only non distinct count
	if aggr.AggrFunc != common.AGG_COUNT || aggr.Distinct {
		return false
	}

	if r.canUseFastCountNoWhere() {
		return true
	}
	if r.canUseFastCountWhere(protoScans) {
		return true
	}
	return false

}

func (r *ScanRequest) canUseFastCountWhere(protoScans []*protobuf.Scan) bool {

	aggr := r.GroupAggr.Aggrs[0]
	//only the first leading key or constant expression
	if aggr.KeyPos == 0 || aggr.ExprValue != nil {
		//if index has where clause
		if r.IndexInst.Defn.WhereExpr != "" {

			for _, scan := range protoScans {
				//compute filter covers
				wExpr, err := parser.Parse(r.IndexInst.Defn.WhereExpr)
				if err != nil {
					logging.Errorf("%v Error parsing where expr %v", r.LogPrefix, err)
				}

				fc := make(map[string]value.Value)
				fc = wExpr.FilterCovers(fc)

				for i, fl := range scan.Filters {

					//only equal filter is supported
					if !checkEqualFilter(fl) {
						return false
					}

					var cv *value.ScopeValue
					var av value.AnnotatedValue

					cv = value.NewScopeValue(make(map[string]interface{}), nil)
					av = value.NewAnnotatedValue(cv)

					av.SetCover(r.IndexInst.Defn.SecExprs[i], value.NewValue(fl.Low))

					cv1 := av.Covers()
					fields := cv1.Fields()

					for f, v := range fields {
						if v1, ok := fc[f]; ok {
							if v != v1 {
								return false
							}
						} else {
							return false
						}
					}
				}
				return true
			}
		}
	}
	return false
}

func (r *ScanRequest) canUseFastCountNoWhere() bool {

	aggr := r.GroupAggr.Aggrs[0]

	//only the first leading key or constant expression
	if aggr.KeyPos == 0 || aggr.ExprValue != nil {

		//full index scan
		if len(r.Scans) == 1 {
			scan := r.Scans[0]
			if len(scan.Filters) == 1 {
				filter := scan.Filters[0]
				if len(filter.CompositeFilters) == 1 {
					if isEncodedNull(filter.CompositeFilters[0].Low.Bytes()) &&
						filter.CompositeFilters[0].High.Bytes() == nil &&
						(filter.CompositeFilters[0].Inclusion == Low ||
							filter.CompositeFilters[0].Inclusion == Neither) {
						return true
					}
				}
			}
		}
	}

	return false
}

func checkEqualFilter(fl *protobuf.CompositeElementFilter) bool {

	if (fl.Low != nil && fl.High != nil && bytes.Equal(fl.Low, fl.High)) && Inclusion(fl.GetInclusion()) == Both {
		return true
	}
	return false

}

func (r *ScanRequest) hasAllEqualFiltersUpto(keyPos int) bool {
	for i := 0; i <= keyPos; i++ {
		if !r.hasAllEqualFilters(i) {
			return false
		}
	}
	return true
}

// Returns true if all filters for the given keyPos(index field) are equal
// and atleast one equal filter exists.
//
// (1) "nil" value for high or low means the filter is unbounded on one end
//     or the both ends. So, it cannot be an equality filter.
// (2) If Low == High AND
//     (2.1) If Inclusion is Low or High, then the filter is contradictory.
//     (2.2) If Inclusion is Neither, then everything will be filtered out,
//           which is an unexpected behavior.
// (3) If there are multiple filters, and at least one filter has less number
//     of composite filters as compared to the input keyPos, then for that
//     filter the equality is unknown and hence return false.
// So, for these cases, hasAllEqualFilters returns false.
func (r *ScanRequest) hasAllEqualFilters(keyPos int) bool {

	found := false
	for _, scan := range r.Scans {
		for _, filter := range scan.Filters {
			if len(filter.CompositeFilters) > keyPos {
				lowBytes := filter.CompositeFilters[keyPos].Low.Bytes()
				highBytes := filter.CompositeFilters[keyPos].High.Bytes()
				if lowBytes == nil || highBytes == nil {
					return false
				}

				if !bytes.Equal(lowBytes, highBytes) {
					return false
				} else {
					if filter.CompositeFilters[keyPos].Inclusion != Both {
						return false
					}

					found = true
				}
			} else {
				return false
			}
		}
	}
	return found
}

func compileN1QLExpression(expr string) (expression.Expression, error) {

	cExpr, err := parser.Parse(expr)
	if err != nil {
		logging.Errorf("ScanRequest::compileN1QLExpression() %v: %v\n", logging.TagUD(expr), err)
		return nil, err
	}
	return cExpr, nil

}

/////////////////////////////////////////////////////////////////////////
//
// Helpers
//
/////////////////////////////////////////////////////////////////////////

func getReverseCollatedIndexKey(input []byte, desc []bool) (IndexKey, error) {
	reversed, err := jsonEncoder.ReverseCollate(input, desc)
	if err != nil {
		return nil, err
	}
	key := secondaryKey(reversed)
	return &key, nil
}

func flipInclusion(incl Inclusion, desc []bool) Inclusion {
	if len(desc) != 0 && desc[0] {
		if incl == Low {
			return High
		} else if incl == High {
			return Low
		}
	}
	return incl
}

func getScanAll() Scan {
	s := Scan{
		ScanType: AllReq,
	}
	return s
}

// Return true if a < b
func IndexKeyLessThan(a, b IndexKey) bool {
	if a == MinIndexKey {
		return true
	} else if a == MaxIndexKey {
		return false
	} else if b == MinIndexKey {
		return false
	} else if b == MaxIndexKey {
		return true
	}
	return (bytes.Compare(a.Bytes(), b.Bytes()) < 0)
}

func (r ScanRequest) String() string {
	str := fmt.Sprintf("defnId:%v, instId:%v, index:%v/%v, type:%v, partitions:%v",
		r.DefnID, r.IndexInstId, r.Bucket, r.IndexName, r.ScanType, r.PartitionIds)

	if len(r.Scans) == 0 {
		var incl, span string

		switch r.Incl {
		case Low:
			incl = "incl:low"
		case High:
			incl = "incl:high"
		case Both:
			incl = "incl:both"
		default:
			incl = "incl:none"
		}

		if len(r.Keys) == 0 {
			if r.ScanType == StatsReq || r.ScanType == ScanReq || r.ScanType == CountReq {
				span = fmt.Sprintf("range (%s,%s %s)", r.Low, r.High, incl)
			} else {
				span = "all"
			}
		} else {
			span = "keys ( "
			for _, k := range r.Keys {
				span = span + k.String() + " "
			}
			span = span + ")"
		}

		str += fmt.Sprintf(", span:%s", logging.TagUD(span))
	} else {
		str += fmt.Sprintf(", scans: %+v", logging.TagUD(r.Scans))
	}

	if r.Limit > 0 {
		str += fmt.Sprintf(", limit:%d", r.Limit)
	}

	if r.Consistency != nil {
		str += fmt.Sprintf(", consistency:%s", strings.ToLower(r.Consistency.String()))
	}

	if r.RequestId != "" {
		str += fmt.Sprintf(", requestId:%v", r.RequestId)
	}

	if r.GroupAggr != nil {
		str += fmt.Sprintf(", groupaggr: %v", r.GroupAggr)
	}

	return str
}

func (r *ScanRequest) getKeyBuffer(minSize int) []byte {
	if r.indexKeyBuffer == nil {
		buf := secKeyBufPool.Get()
		if minSize != 0 {
			newBuf := resizeEncodeBuf(*buf, minSize, true)
			r.keyBufList = append(r.keyBufList, &newBuf)
			r.indexKeyBuffer = newBuf
		} else {
			r.keyBufList = append(r.keyBufList, buf)
			r.indexKeyBuffer = *buf
		}
	}
	return r.indexKeyBuffer
}

// Reuses buffer from buffer pool. When current buffer is insufficient
// get new buffer from the pool, reset sharedBuffer & sharedBufferLen
func (r *ScanRequest) getSharedBuffer(length int) []byte {
	if r.sharedBuffer == nil || (cap(*r.sharedBuffer)-r.sharedBufferLen) < length {
		buf := secKeyBufPool.Get()
		r.keyBufList = append(r.keyBufList, buf)
		r.sharedBuffer = buf
		r.sharedBufferLen = 0
		return (*r.sharedBuffer)[:0]
	}
	return (*r.sharedBuffer)[r.sharedBufferLen:r.sharedBufferLen]
}

/////////////////////////////////////////////////////////////////////////
//
// IndexPoints Implementation
//
/////////////////////////////////////////////////////////////////////////

func (ip IndexPoints) Len() int {
	return len(ip)
}

func (ip IndexPoints) Swap(i, j int) {
	ip[i], ip[j] = ip[j], ip[i]
}

func (ip IndexPoints) Less(i, j int) bool {
	return IndexPointLessThan(ip[i], ip[j])
}

// Return true if x < y
func IndexPointLessThan(x, y IndexPoint) bool {
	a := x.Value
	b := y.Value
	if a == MinIndexKey {
		return true
	} else if a == MaxIndexKey {
		return false
	} else if b == MinIndexKey {
		return false
	} else if b == MaxIndexKey {
		return true
	}

	if a.ComparePrefixIndexKey(b) < 0 {
		return true
	} else if a.ComparePrefixIndexKey(b) == 0 {
		if len(a.Bytes()) == len(b.Bytes()) {
			if x.Type == "low" && y.Type == "high" {
				return true
			}
			return false
		}
		inclusiveKey := minlen(x, y)
		switch inclusiveKey.Type {
		case "low":
			if inclusiveKey == x {
				return true
			}
		case "high":
			if inclusiveKey == y {
				return true
			}
		}
		return false
	}
	return false
}

func minlen(x, y IndexPoint) IndexPoint {
	if len(x.Value.Bytes()) < len(y.Value.Bytes()) {
		return x
	}
	return y
}

/////////////////////////////////////////////////////////////////////////
//
// Filters Implementation
//
/////////////////////////////////////////////////////////////////////////

func (fl Filters) Len() int {
	return len(fl)
}

func (fl Filters) Swap(i, j int) {
	fl[i], fl[j] = fl[j], fl[i]
}

func (fl Filters) Less(i, j int) bool {
	return FilterLessThan(fl[i], fl[j])
}

// Return true if x < y
func FilterLessThan(x, y Filter) bool {
	a := x.Low
	b := y.Low
	if a == MinIndexKey {
		return true
	} else if a == MaxIndexKey {
		return false
	} else if b == MinIndexKey {
		return false
	} else if b == MaxIndexKey {
		return true
	}

	if a.ComparePrefixIndexKey(b) < 0 {
		return true
	}
	return false
}

/////////////////////////////////////////////////////////////////////////
//
// Connection Handler
//
/////////////////////////////////////////////////////////////////////////

const (
	ScanBufPoolSize = DEFAULT_MAX_SEC_KEY_LEN_SCAN + MAX_DOCID_LEN + 2
)

const (
	ScanQueue = "ScanQueue"
)

type ConCacheObj interface {
	Free() bool
}

type ConnectionContext struct {
	bufPool map[common.PartitionId]*common.BytesBufPool
	cache   map[string]ConCacheObj
	mutex   sync.RWMutex
}

func createConnectionContext() interface{} {
	return &ConnectionContext{
		bufPool: make(map[common.PartitionId]*common.BytesBufPool),
		cache:   make(map[string]ConCacheObj),
	}
}

func (c *ConnectionContext) GetBufPool(partitionId common.PartitionId) *common.BytesBufPool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if _, ok := c.bufPool[partitionId]; !ok {
		c.bufPool[partitionId] = common.NewByteBufferPool(ScanBufPoolSize)
	}

	return c.bufPool[partitionId]
}

func (c *ConnectionContext) Get(id string) ConCacheObj {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.cache[id]
}

func (c *ConnectionContext) Put(id string, obj ConCacheObj) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.cache[id] = obj
}

func (c *ConnectionContext) ResetCache() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	for key, obj := range c.cache {
		if obj.Free() {
			delete(c.cache, key)
		}
	}
}
