//  Copyright (c) 2016 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

package cbft

import (
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/blevesearch/bleve"
	bleveMoss "github.com/blevesearch/bleve/index/store/moss"
	"github.com/blevesearch/bleve/index/upsidedown"

	"encoding/json"

	"github.com/couchbase/cbgt"
	"github.com/couchbase/cbgt/rest"
	"github.com/couchbase/moss"
)

func TestPartialRollbackMossNoOp(t *testing.T) {
	testHandlersWithOnePartitionPrimaryFeedPartialRollbackMoss(t, 100,
		"Rollback to after all the snapshots, so it should be a no-op",
		map[string]bool{`{"status":"ok","count":4}`: true})
}

func TestPartialRollbackMossSeq11(t *testing.T) {
	testHandlersWithOnePartitionPrimaryFeedPartialRollbackMoss(t, 11,
		"Rollback to seq 11 in the middle of the stream of snapshots",
		map[string]bool{`{"status":"ok","count":3}`: true})
}

func TestPartialRollbackMossSeq10(t *testing.T) {
	testHandlersWithOnePartitionPrimaryFeedPartialRollbackMoss(t, 10,
		"Rollback to seq 10 in the middle of the stream of snapshots",
		map[string]bool{`{"status":"ok","count":2}`: true})
}

func TestPartialRollbackMossSeq0(t *testing.T) {
	testHandlersWithOnePartitionPrimaryFeedPartialRollbackMoss(t, 0,
		"Rollback to seq number 0",
		map[string]bool{`{"status":"ok","count":0}`: true})
}

func setVBucketFailoverLog(feed *cbgt.PrimaryFeed, partition string) {
	flog := make([][]uint64, 1)
	flog[0] = []uint64{0, 100}
	buf, _ := json.Marshal(flog)
	feed.OpaqueSet(partition, buf)
}

func testHandlersWithOnePartitionPrimaryFeedPartialRollbackMoss(t *testing.T,
	rollbackToSeq uint64,
	rollbackDesc string,
	afterRollbackCountResponseMatch map[string]bool) {
	rest.RequestProxyStubFunc = func() bool {
		return false
	}
	BlevePIndexAllowMossPrev := BlevePIndexAllowMoss
	BlevePIndexAllowMoss = true

	bleveConfigDefaultKVStorePrev := bleve.Config.DefaultKVStore
	bleve.Config.DefaultKVStore = "mossStore"
	bleveConfigDefaultIndexTypePrev := bleve.Config.DefaultIndexType
	bleve.Config.DefaultIndexType = upsidedown.Name

	bleveMossRegistryCollectionOptionsFTSPrev := bleveMoss.RegistryCollectionOptions["fts"]
	bleveMoss.RegistryCollectionOptions["fts"] = moss.CollectionOptions{}

	defer func() {
		BlevePIndexAllowMoss = BlevePIndexAllowMossPrev
		bleve.Config.DefaultKVStore = bleveConfigDefaultKVStorePrev
		bleve.Config.DefaultIndexType = bleveConfigDefaultIndexTypePrev
		bleveMoss.RegistryCollectionOptions["fts"] = bleveMossRegistryCollectionOptionsFTSPrev
		rest.RequestProxyStubFunc = nil
	}()

	emptyDir, _ := ioutil.TempDir("./tmp", "test")
	defer os.RemoveAll(emptyDir)

	cfg := cbgt.NewCfgMem()
	meh := &TestMEH{}
	mgr := cbgt.NewManager(cbgt.VERSION, cfg, cbgt.NewUUID(),
		nil, "", 1, "", ":1000", emptyDir, "some-datasource", meh)
	mgr.Start("wanted")
	mgr.Kick("test-start-kick")

	mr, _ := cbgt.NewMsgRing(os.Stderr, 1000)

	router, _, err := NewRESTRouter("v0", mgr, "static", "", mr, nil)
	if err != nil || router == nil {
		t.Errorf("no mux router")
	}

	var pindex *cbgt.PIndex
	var feed *cbgt.PrimaryFeed

	waitForPersistence := func(docCount float64) {
		for i := 0; i < 100; i++ {
			stats := map[string]interface{}{
				"doc_count":           float64(0),
				"num_recs_to_persist": float64(0),
			}
			err = addPIndexStats(pindex, stats)
			if err != nil {
				t.Errorf("expected nil addPIndexStats err, got: %v", err)
			}
			v, ok := stats["num_recs_to_persist"]
			if ok {
				nrtp, ok := v.(float64)
				if ok && nrtp <= 0.0 {
					dv, ok1 := stats["doc_count"]
					if ok1 {
						dc, ok := dv.(float64)
						if ok && dc == docCount {
							return
						}
					}
				}
			}

			time.Sleep(100 * time.Millisecond)
		}
		t.Errorf("persistence took too long!")
	}

	tests := []*RESTHandlerTest{
		{
			Desc:   "create dest feed with 1 partition for rollback",
			Path:   "/api/index/idx0",
			Method: "PUT",
			Params: url.Values{
				"indexType": []string{"fulltext-index"},
				"indexParams": []string{
					`{"store":{"mossStoreOptions":{"CompactionLevelMaxSegments":100000}}}`,
				}, // Never compact during this test.
				"sourceType":   []string{"primary"},
				"sourceParams": []string{`{"numPartitions":1}`},
			},
			Status: http.StatusOK,
			ResponseMatch: map[string]bool{
				`{"status":"ok"}`: true,
			},
			After: func() {
				time.Sleep(10 * time.Millisecond)
				mgr.Kick("after-rollback")
				feeds, pindexes := mgr.CurrentMaps()
				if len(feeds) != 1 {
					t.Errorf("expected to be 1 feed, got feeds: %+v", feeds)
				}
				if len(pindexes) != 1 {
					t.Errorf("expected to be 1 pindex,"+
						" got pindexes: %+v", pindexes)
				}
				for _, f := range feeds {
					var ok bool
					feed, ok = f.(*cbgt.PrimaryFeed)
					if !ok {
						t.Errorf("expected the 1 feed to be a PrimaryFeed")
					}
				}
				for _, p := range pindexes {
					pindex = p
				}
			},
		},
		{
			Before: func() {
				partition := "0"
				snapStart := uint64(1)
				snapEnd := uint64(10)
				err = feed.SnapshotStart(partition, snapStart, snapEnd)
				if err != nil {
					t.Errorf("expected no err on snapshot-start")
				}
				setVBucketFailoverLog(feed, partition)
			},
			Desc:   "count idx0 0 when snapshot just started, pre-rollback",
			Path:   "/api/index/idx0/count",
			Method: "GET",
			Status: http.StatusOK,
			ResponseMatch: map[string]bool{
				`{"status":"ok","count":0}`: true,
			},
		},
		{
			Before: func() {
				partition := "0"
				key := []byte("ss1-k1")
				seq := uint64(1)
				val := []byte(`{"foo":"bar","yow":"wow"}`)
				err = feed.DataUpdate(partition, key, seq, val,
					0, cbgt.DEST_EXTRAS_TYPE_NIL, nil)
				if err != nil {
					t.Errorf("expected no err on data-update")
				}
			},
			Desc: "count idx0 should be 0 when got an update (doc created)" +
				" but snapshot not ended, pre-rollback",
			Path:   "/api/index/idx0/count",
			Method: "GET",
			Status: http.StatusOK,
			ResponseMatch: map[string]bool{
				`{"status":"ok","count":0}`: true,
			},
		},
		{
			Before: func() {
				partition := "0"
				key := []byte("ss1-k2")
				seq := uint64(2)
				val := []byte(`{"foo":"bing","yow":"wow"}`)
				err = feed.DataUpdate(partition, key, seq, val,
					0, cbgt.DEST_EXTRAS_TYPE_NIL, nil)
				if err != nil {
					t.Errorf("expected no err on data-update")
				}
			},
			Desc: "count idx0 0 when got 2nd update (another doc created)" +
				" but snapshot not ended, pre-rollback",
			Path:   "/api/index/idx0/count",
			Method: "GET",
			Status: http.StatusOK,
			ResponseMatch: map[string]bool{
				`{"status":"ok","count":0}`: true,
			},
		},
		{
			Before: func() {
				partition := "0"
				snapStart := uint64(11)
				snapEnd := uint64(20)
				err = feed.SnapshotStart(partition, snapStart, snapEnd)
				if err != nil {
					t.Errorf("expected no err on snapshot-start")
				}
				waitForPersistence(float64(2))
			},
			Desc:   "count idx0 2 when 1st snapshot ended, pre-rollback",
			Path:   "/api/index/idx0/count",
			Method: "GET",
			Status: http.StatusOK,
			ResponseMatch: map[string]bool{
				`{"status":"ok","count":2}`: true,
			},
		},
		{
			Before: func() {
				partition := "0"
				key := []byte("ss11-k11")
				seq := uint64(11)
				val := []byte(`{"foo":"baz","yow":"NOPE"}`)
				err = feed.DataUpdate(partition, key, seq, val,
					0, cbgt.DEST_EXTRAS_TYPE_NIL, nil)
				if err != nil {
					t.Errorf("expected no err on data-update")
				}
			},
			Desc: "count idx0 should be 2 when got an update (doc created)" +
				" in 2nd snapshot, 2nd snapshot not ended, pre-rollback",
			Path:   "/api/index/idx0/count",
			Method: "GET",
			Status: http.StatusOK,
			ResponseMatch: map[string]bool{
				`{"status":"ok","count":2}`: true,
			},
		},
		{
			Before: func() {
				partition := "0"
				snapStart := uint64(21)
				snapEnd := uint64(30)
				err = feed.SnapshotStart(partition, snapStart, snapEnd)
				if err != nil {
					t.Errorf("expected no err on snapshot-start")
				}
				waitForPersistence(float64(3))
			},
			Desc:   "count idx0 3 when 2nd snapshot ended, pre-rollback",
			Path:   "/api/index/idx0/count",
			Method: "GET",
			Params: nil,
			Body:   nil,
			Status: http.StatusOK,
			ResponseMatch: map[string]bool{
				`{"status":"ok","count":3}`: true,
			},
		},
		{
			Before: func() {
				partition := "0"
				key := []byte("ss21-k21")
				seq := uint64(21)
				val := []byte(`{"foo":"baz","yow":"whoa"}`)
				err = feed.DataUpdate(partition, key, seq, val,
					0, cbgt.DEST_EXTRAS_TYPE_NIL, nil)
				if err != nil {
					t.Errorf("expected no err on data-update")
				}
			},
			Desc: "count idx0 should be 3 when got an update (doc created)" +
				" in 3rd snapshot, 3rd snapshot not ended, pre-rollback",
			Path:   "/api/index/idx0/count",
			Method: "GET",
			Status: http.StatusOK,
			ResponseMatch: map[string]bool{
				`{"status":"ok","count":3}`: true,
			},
		},
		{
			Before: func() {
				partition := "0"
				snapStart := uint64(31)
				snapEnd := uint64(40)
				err = feed.SnapshotStart(partition, snapStart, snapEnd)
				if err != nil {
					t.Errorf("expected no err on snapshot-start")
				}
				// need to revisit this sleep part, ideally
				// waitForPersistence should have sufficed
				time.Sleep(100 * time.Millisecond)
				waitForPersistence(float64(4))
			},
			Desc:   "count idx0 4 when 3rd snapshot ended, pre-rollback",
			Path:   "/api/index/idx0/count",
			Method: "GET",
			Params: nil,
			Body:   nil,
			Status: http.StatusOK,
			ResponseMatch: map[string]bool{
				`{"status":"ok","count":4}`: true,
			},
		},
		{
			// Rollback to a high seq number, so it's a no-op
			// w.r.t. doing an actual rollback.
			Before: func() {
				partition := "0"
				err = feed.RollbackEx(partition, 0, rollbackToSeq)
				if err != nil {
					t.Errorf("expected no err on rollback, got: %v", err)
				}
				// NOTE: We might check right after rollback but before
				// we get a kick, but unfortunately results will be race-y.
				time.Sleep(100 * time.Millisecond)
				mgr.Kick("after-rollback")

			},
			Desc:          rollbackDesc,
			Path:          "/api/index/idx0/count",
			Method:        "GET",
			Params:        nil,
			Body:          nil,
			Status:        http.StatusOK,
			ResponseMatch: afterRollbackCountResponseMatch,
		},
	}

	testRESTHandlers(t, tests, router)
}
