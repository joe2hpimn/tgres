//
// Copyright 2016 Gregory Trubetskoy. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package receiver

import (
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/tgres/tgres/cluster"
	"github.com/tgres/tgres/rrd"
	"github.com/tgres/tgres/serde"
)

type fakeLogger struct {
	last []byte
}

func (f *fakeLogger) Write(p []byte) (n int, err error) {
	f.last = p
	return len(p), nil
}

func Test_directorIncomingDPMessages(t *testing.T) {
	defer func() {
		// restore default output
		log.SetOutput(os.Stderr)
	}()

	fl := &fakeLogger{}
	log.SetOutput(fl)

	rcv := make(chan *cluster.Msg)
	dpCh := make(chan *IncomingDP)

	count := 0
	go func() {
		for {
			if _, ok := <-dpCh; !ok {
				break
			}
			count++
		}
	}()

	go directorIncomingDPMessages(rcv, dpCh)

	// Sending a bogus message should not cause anything be written to dpCh
	rcv <- &cluster.Msg{}
	rcv <- &cluster.Msg{} // second send ensures the loop has gone full circle
	if count > 0 {
		t.Errorf("Malformed messages should not cause data points, count: %d", count)
	}
	if !strings.Contains(string(fl.last), "decoding FAILED") {
		t.Errorf("Malformed messages should log 'decoding FAILED'")
	}

	// now we need a real message
	dp := &IncomingDP{Name: "foo", TimeStamp: time.Unix(1000, 0), Value: 123}
	m, _ := cluster.NewMsg(&cluster.Node{}, dp)
	rcv <- m
	rcv <- m

	if count < 1 {
		t.Errorf("At least 1 data point should have been sent to dpCh")
	}

	dp.Hops = 1000 // exceed maxhops (which in fakeCluster is 0?)
	m, _ = cluster.NewMsg(&cluster.Node{}, dp)
	rcv <- m // "clear" the loop
	count = 0
	rcv <- m
	rcv <- m
	if count > 0 {
		t.Errorf("Hops exceeded should not cause data points, count: %d", count)
	}
	if !strings.Contains(string(fl.last), "max hops") {
		t.Errorf("Hops exceeded messages should log 'max hops'")
	}

	// Closing the dpCh should cause the recover() to happen
	// The test here is that it doesn't panic
	close(dpCh)
	dp.Hops = 0
	m, _ = cluster.NewMsg(&cluster.Node{}, dp)
	rcv <- m

	// Closing the channel exists (not sure how to really test for that)
	go directorIncomingDPMessages(rcv, dpCh)
	close(rcv)
}

func Test_directorForwardDPToNode(t *testing.T) {

	dp := &IncomingDP{Name: "foo", TimeStamp: time.Unix(1000, 0), Value: 123}
	md := make([]byte, 20)
	md[0] = 1 // Ready
	node := &cluster.Node{Node: &memberlist.Node{Meta: md}}
	snd := make(chan *cluster.Msg)

	count := 0
	go func() {
		for {
			if _, ok := <-snd; !ok {
				break
			}
			count++
		}
	}()

	// if hops is > 0, nothing happens
	dp.Hops = 1
	directorForwardDPToNode(dp, node, snd)
	directorForwardDPToNode(dp, node, snd)

	if count > 0 {
		t.Errorf("directorForwardDPToNode: Data points with hops > 0 should not be forwarded")
	}

	// otherwise it should work
	dp.Hops = 0
	directorForwardDPToNode(dp, node, snd)
	dp.Hops = 0 // because it just got incremented
	directorForwardDPToNode(dp, node, snd)

	if count < 1 {
		t.Errorf("Data point not sent to channel?")
	}

	// mark node not Ready
	md[0] = 0
	dp.Hops = 0 // because it just got incremented
	if err := directorForwardDPToNode(dp, node, snd); err == nil {
		t.Errorf("not ready node should cause an error")
	}
}

func Test_directorProcessOrForward(t *testing.T) {

	saveFn := directorForwardDPToNode
	forward, fwErr := 0, error(nil)
	directorForwardDPToNode = func(dp *IncomingDP, node *cluster.Node, snd chan *cluster.Msg) error {
		forward++
		return fwErr
	}

	// dsc
	db := &fakeSerde{}
	df := &SimpleDSFinder{DftDSSPec}
	sr := &fakeSr{}
	dsf := &dsFlusher{db: db, sr: sr}
	dsc := newDsCache(db, df, dsf)

	// rds
	foo := serde.Ident{"name": "foo"}
	ds := serde.NewDbDataSource(0, foo, rrd.NewDataSource(*DftDSSPec))
	rds := &cachedDs{DbDataSourcer: ds}

	// cluster
	clstr := &fakeCluster{}
	md := make([]byte, 20)
	md[0] = 1 // Ready
	node := &cluster.Node{Node: &memberlist.Node{Meta: md, Name: "local"}}
	clstr.nodesForDd = []*cluster.Node{node}
	clstr.ln = node

	// workerChs
	workerChs := make([]chan *incomingDpWithDs, 1)
	workerChs[0] = make(chan *incomingDpWithDs)
	sent := 0
	go func() {
		for {
			<-workerChs[0]
			sent++
		}
	}()

	// Test if we are LocalNode
	directorProcessOrForward(dsc, rds, clstr, workerChs, nil, nil)
	directorProcessOrForward(dsc, rds, clstr, workerChs, nil, nil)
	if sent < 1 {
		t.Errorf("directorProcessOrForward: Nothing sent to workerChs")
	}

	// Now test we are NOT LN, forward
	remote := &cluster.Node{Node: &memberlist.Node{Meta: md, Name: "remote"}}
	clstr.nodesForDd = []*cluster.Node{remote}

	n := directorProcessOrForward(dsc, rds, clstr, workerChs, nil, nil)
	if forward != 1 {
		t.Errorf("directorProcessOrForward: directorForwardDPToNode not called")
	}
	if n != 1 {
		t.Errorf("directorProcessOrForward: return value != 1")
	}

	fl := &fakeLogger{}
	log.SetOutput(fl)
	defer func() {
		// restore default output
		log.SetOutput(os.Stderr)
	}()

	fwErr = fmt.Errorf("some error")
	n = directorProcessOrForward(dsc, rds, clstr, workerChs, nil, nil)
	if n != 0 {
		t.Errorf("directorProcessOrForward: return value != 0")
	}
	if !strings.Contains(string(fl.last), "some error") {
		t.Errorf("directorProcessOrForward: directorForwardDPToNode not logged")
	}
	fwErr = nil

	// make an rds with points
	foo = serde.Ident{"name": "foo"}
	ds = serde.NewDbDataSource(0, foo, rrd.NewDataSource(rrd.DSSpec{
		Step: 10 * time.Second,
		RRAs: []rrd.RRASpec{
			rrd.RRASpec{Function: rrd.WMEAN,
				Step:   10 * time.Second,
				Span:   30 * time.Second,
				Latest: time.Unix(1000, 0),
			},
		},
	}))
	ds.ProcessDataPoint(123, time.Unix(2000, 0))
	ds.ProcessDataPoint(123, time.Unix(3000, 0))
	rds = &cachedDs{DbDataSourcer: ds}

	directorProcessOrForward(dsc, rds, clstr, workerChs, nil, nil)
	if !strings.Contains(string(fl.last), "PointCount") {
		t.Errorf("directorProcessOrForward: Missing the PointCount warning log")
	}
	if rds.PointCount() != 0 {
		t.Errorf("directorProcessOrForward: ClearRRAs(true) not called")
	}

	// restore directorForwardDPToNode
	directorForwardDPToNode = saveFn
}

func Test_directorProcessIncomingDP(t *testing.T) {

	saveFn := directorProcessOrForward
	dpofCalled := 0
	directorProcessOrForward = func(dsc *dsCache, cds *cachedDs, clstr clusterer, workerChs workerChannels, dp *IncomingDP, snd chan *cluster.Msg) (forwarded int) {
		dpofCalled++
		return 0
	}

	fl := &fakeLogger{}
	log.SetOutput(fl)
	defer func() {
		// restore default output
		log.SetOutput(os.Stderr)
	}()

	// dp
	dp := &IncomingDP{Name: "foo", TimeStamp: time.Unix(1000, 0), Value: 123}

	// dsc
	db := &fakeSerde{}
	df := &SimpleDSFinder{DftDSSPec}
	scr := &fakeSr{}
	dsf := &dsFlusher{db: db, sr: scr}
	dsc := newDsCache(db, df, dsf)

	// cluster
	clstr := &fakeCluster{cChange: make(chan bool)}

	// workerChs
	workerChs := make([]chan *incomingDpWithDs, 1)
	workerChs[0] = make(chan *incomingDpWithDs)
	sent := 0
	go func() {
		for {
			<-workerChs[0]
			sent++
		}
	}()

	// NaN
	dp.Value = math.NaN()
	directorProcessIncomingDP(dp, scr, dsc, nil, nil, nil)
	if scr.called != 1 {
		t.Errorf("directorProcessIncomingDP: With a NaN, reportStatCount() should only be called once")
	}
	if dpofCalled > 0 {
		t.Errorf("directorProcessIncomingDP: With a NaN, directorProcessOrForward should not be called")
	}

	// A value
	dp.Value = 1234
	scr.called, dpofCalled = 0, 0
	directorProcessIncomingDP(dp, scr, dsc, workerChs, clstr, nil)
	if scr.called != 2 {
		t.Errorf("directorProcessIncomingDP: With a value, reportStatCount() should be called twice: %v", scr.called)
	}
	if dpofCalled != 1 {
		t.Errorf("directorProcessIncomingDP: With a value, directorProcessOrForward should be called once: %v", dpofCalled)
	}

	// A blank name should cause a nil rds
	dp.Name = ""
	scr.called, dpofCalled = 0, 0
	directorProcessIncomingDP(dp, scr, dsc, nil, nil, nil)
	if scr.called != 1 {
		t.Errorf("directorProcessIncomingDP: With a blank name, reportStatCount() should be called once")
	}
	if dpofCalled > 0 {
		t.Errorf("directorProcessIncomingDP: With a blank name, directorProcessOrForward should not be called")
	}
	if !strings.Contains(string(fl.last), "No spec matched") {
		t.Errorf("should log 'No spec matched'")
	}

	// fake a db error
	dp.Name = "blah"
	db.fakeErr = true
	scr.called, dpofCalled = 0, 0
	directorProcessIncomingDP(dp, scr, dsc, nil, nil, nil)
	if scr.called != 1 {
		t.Errorf("directorProcessIncomingDP: With a db error, reportStatCount() should be called once")
	}
	if dpofCalled > 0 {
		t.Errorf("directorProcessIncomingDP: With a db error, directorProcessOrForward should not be called")
	}
	if !strings.Contains(string(fl.last), "error") {
		t.Errorf("should log 'error'")
	}

	// nil cluster
	dp.Value = 1234
	db.fakeErr = false
	scr.called = 0
	directorProcessIncomingDP(dp, scr, dsc, workerChs, nil, nil)
	if scr.called != 1 {
		t.Errorf("directorProcessIncomingDP: With a value, reportStatCount() should be called once: %v", scr.called)
	}
	if dpofCalled != 0 {
		t.Errorf("directorProcessIncomingDP: With a value and no cluster, directorProcessOrForward should not be called: %v", dpofCalled)
	}

	directorProcessOrForward = saveFn
}

func Test_the_director(t *testing.T) {

	saveFn1 := directorIncomingDPMessages
	saveFn2 := directorProcessIncomingDP
	dimCalled := 0
	directorIncomingDPMessages = func(rcv chan *cluster.Msg, dpCh chan *IncomingDP) { dimCalled++ }
	dpidpCalled := 0
	directorProcessIncomingDP = func(dp *IncomingDP, scr statReporter, dsc *dsCache, workerChs workerChannels, clstr clusterer, snd chan *cluster.Msg) {
		dpidpCalled++
	}

	fl := &fakeLogger{}
	log.SetOutput(fl)
	defer func() {
		// restore default output
		log.SetOutput(os.Stderr)
	}()

	wc := &wrkCtl{wg: &sync.WaitGroup{}, startWg: &sync.WaitGroup{}, id: "FOO"}
	clstr := &fakeCluster{cChange: make(chan bool)}
	dpCh := make(chan *IncomingDP)

	// dsc
	db := &fakeSerde{}
	df := &SimpleDSFinder{DftDSSPec}
	sr := &fakeSr{}
	dsf := &dsFlusher{db: db, sr: sr}
	dsc := newDsCache(db, df, dsf)

	wc.startWg.Add(1)
	go director(wc, dpCh, clstr, sr, dsc, nil)
	wc.startWg.Wait()

	if clstr.nReady == 0 {
		t.Errorf("director: Ready(true) not called on cluster")
	}

	if clstr.nReg == 0 {
		t.Errorf("director: cluster.RegisterMsgType() not called")
	}

	// This sometimes can fail because we don't wait for that goroutine in this test...
	time.Sleep(5 * time.Millisecond)
	if dimCalled == 0 {
		t.Errorf("director: directorIncomingDPMessages not started")
	}

	dp := &IncomingDP{Name: "foo", TimeStamp: time.Unix(1000, 0), Value: 123}
	dpCh <- dp
	dpCh <- dp

	if dpidpCalled == 0 {
		t.Errorf("director: directorProcessIncomingDP not called")
	}

	// Trigger a transition
	clstr.cChange <- true
	dpCh <- dp

	if clstr.nTrans == 0 {
		t.Errorf("director: on cluster change, Transition() not called")
	}

	// Transition with error
	clstr.tErr = true
	clstr.cChange <- true
	dpCh <- dp

	if !strings.Contains(string(fl.last), "some error") {
		t.Errorf("director: on transition error, 'some error' missing from logs")
	}

	dpidpCalled = 0
	close(dpCh)
	time.Sleep(1 * time.Second) // so that nil dp goroutine panics/recovers

	if dpidpCalled > 0 {
		t.Errorf("director: directorProcessIncomingDP must not be called on channel close")
	}

	if !strings.Contains(string(fl.last), "shutting down") {
		t.Errorf("director: on channel close, missing 'shutting down' log entry")
	}

	// overrun
	dpCh = make(chan *IncomingDP, 5)
	dpCh <- dp
	dpCh <- dp
	dpCh <- dp
	dpCh <- dp

	wc.startWg.Add(1)
	go director(wc, dpCh, clstr, sr, dsc, nil)
	wc.startWg.Wait()

	time.Sleep(100 * time.Millisecond)

	close(dpCh)

	directorIncomingDPMessages = saveFn1
	directorProcessIncomingDP = saveFn2
}

func Test_director_reportDirectorChannelFillPercent(t *testing.T) {
	defer func() {
		// restore default output
		log.SetOutput(os.Stderr)
	}()

	fl := &fakeLogger{}
	log.SetOutput(fl)

	ch := make(chan *IncomingDP, 10)
	sr := &fakeSr{}
	for i := 0; i < 9; i++ {
		ch <- &IncomingDP{}
	}
	queue := &dpQueue{}
	queue.push(&IncomingDP{})
	go reportDirectorChannelFillPercent(ch, queue, sr, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if sr.called == 0 {
		t.Errorf("reportDirectorChannelFillPercent: statReporter should have been called a bunch of times")
	}
	if !strings.Contains(string(fl.last), "WARNING") {
		t.Errorf("reportDirectorChannelFillPercent: there should be a warning about director channel nearly full")
	}
}

func Test_director_queue(t *testing.T) {

	queue := &dpQueue{}
	dp := &IncomingDP{}
	queue.push(dp)
	if queue.pop() != dp {
		t.Errorf("queue: pop returned wrong dp")
	}
	if queue.size() != 0 {
		t.Errorf("queue: should be empty")
	}
	queue.push(&IncomingDP{})
	if queue.size() != 1 {
		t.Errorf("queue: size != 1")
	}
}

func Test_director_checkSetAside(t *testing.T) {
	queue := &dpQueue{}
	dp := &IncomingDP{}
	dp2 := &IncomingDP{}

	r := checkSetAside(dp, queue, true)
	if r != nil {
		t.Errorf("with skip, checkSetAside should return nil")
	}
	if queue.size() != 1 {
		t.Errorf("checkSetAside: queue size != 1")
	}
	r = checkSetAside(dp2, queue, false)
	if r != dp {
		t.Errorf("checkSetAside returned wrong point")
	}
	r = checkSetAside(nil, queue, false)
	if r != dp2 {
		t.Errorf("checkSetAside returned wrong point")
	}
	if queue.size() != 0 {
		t.Errorf("checkSetAside: queue size != 0")
	}
	r = checkSetAside(nil, queue, false)
	if r != nil {
		t.Errorf("with skip false and empty queue, checkSetAside should return our point: nil")
	}
}
