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
	"sync"
	"time"

	"github.com/tgres/tgres/aggregator"
)

type wrkCtl struct {
	wg, startWg *sync.WaitGroup
	id          string
}

func (w *wrkCtl) ident() string { return w.id }
func (w *wrkCtl) onEnter()      { w.wg.Add(1) }
func (w *wrkCtl) onExit()       { w.wg.Done() }
func (w *wrkCtl) onStarted()    { w.startWg.Done() }

type wController interface {
	ident() string
	onEnter()
	onExit()
	onStarted()
}

var startAllWorkers = func(r *Receiver, startWg *sync.WaitGroup) {
	startWorkers(r, startWg)
	startFlushers(r, startWg)
	startAggWorker(r, startWg)
	startPacedMetricWorker(r, startWg)
}

var doStart = func(r *Receiver) {
	log.Printf("Receiver: Caching data sources...")
	r.dsc.preLoad()
	log.Printf("Receiver: Cached %d data sources.", len(r.dsc.byIdent))

	log.Printf("Receiver: starting...")

	var startWg sync.WaitGroup
	startAllWorkers(r, &startWg)

	// Wait for workers/flushers to start correctly
	startWg.Wait()
	log.Printf("Receiver: All workers running, starting director.")

	startWg.Add(1)
	go director(&wrkCtl{wg: &r.directorWg, startWg: &startWg, id: "director"}, r.dpCh, r.cluster, r, r.dsc, r.workerChs)
	startWg.Wait()

	log.Printf("Receiver: Ready.")
}

var stopDirector = func(r *Receiver) {
	log.Printf("Closing director channel...")
	close(r.dpCh)
	r.directorWg.Wait()
	log.Printf("Director finished.")
}

var doStop = func(r *Receiver, clstr clusterer) {
	stopDirector(r)
	stopAllWorkers(r)
	log.Printf("Leaving cluster...")
	clstr.Leave(1 * time.Second)
	clstr.Shutdown()
	log.Printf("Left cluster.")
}

var stopWorkers = func(workerChs []chan *incomingDpWithDs, workerWg *sync.WaitGroup) {
	log.Printf("stopWorkers(): closing all worker channels...")
	for _, ch := range workerChs {
		close(ch)
	}
	log.Printf("stopWorkers(): waiting for workers to finish...")
	workerWg.Wait()
	log.Printf("stopWorkers(): all workers finished.")
}

var stopFlushers = func(flusherChs []chan *dsFlushRequest, flusherWg *sync.WaitGroup) {
	log.Printf("stopFlushers(): closing all flusher channels...")
	for _, ch := range flusherChs {
		close(ch)
	}
	log.Printf("stopFlushers(): waiting for flushers to finish...")
	flusherWg.Wait()
	log.Printf("stopFlushers(): all flushers finished.")
}

var stopAggWorker = func(aggCh chan *aggregator.Command, aggWg *sync.WaitGroup) {
	log.Printf("stopAggWorker(): closing agg channel...")
	close(aggCh)
	log.Printf("stopAggWorker(): waiting for agg worker to finish...")
	aggWg.Wait()
	log.Printf("stopAggWorker(): agg worker finished.")
}

var stopPacedMetricWorker = func(pacedMetricCh chan *pacedMetric, pacedMetricWg *sync.WaitGroup) {
	log.Printf("stopPacedMetricWorker(): closing paced metric channel...")
	close(pacedMetricCh)
	log.Printf("stopPacedMetricWorker(): waiting for paced metric worker to finish...")
	pacedMetricWg.Wait()
	log.Printf("stopPacedMetricWorker(): paced metric worker finished.")
}

var stopAllWorkers = func(r *Receiver) {
	// Order matters here
	stopPacedMetricWorker(r.pacedMetricCh, &r.pacedMetricWg)
	stopAggWorker(r.aggCh, &r.aggWg)
	stopWorkers(r.workerChs, &r.workerWg)
	stopFlushers(r.flusher.channels(), &r.flusherWg)
}

var startWorkers = func(r *Receiver, startWg *sync.WaitGroup) {

	r.workerChs = make([]chan *incomingDpWithDs, r.NWorkers)

	log.Printf("Starting %d workers...", r.NWorkers)
	startWg.Add(r.NWorkers)
	for i := 0; i < r.NWorkers; i++ {
		r.workerChs[i] = make(chan *incomingDpWithDs, 1024)
		go worker(&wrkCtl{wg: &r.flusherWg, startWg: startWg, id: fmt.Sprintf("worker_%d", i)}, r.flusher, r.workerChs[i], r.MinCacheDuration, r.MaxCacheDuration, r.MaxCachedPoints, time.Second, r)

	}
}

var startFlushers = func(r *Receiver, startWg *sync.WaitGroup) {
	if r.flusher.flusher() == nil { // This serde doesn't support flushing
		return
	}

	log.Printf("Starting %d flushers...", r.NWorkers)
	startWg.Add(r.NWorkers)
	r.flusher.start(r.NWorkers, &r.flusherWg, startWg, r.MaxFlushRatePerSecond)
}

var startAggWorker = func(r *Receiver, startWg *sync.WaitGroup) {
	log.Printf("Starting aggWorker...")
	startWg.Add(1)
	go aggWorker(&wrkCtl{wg: &r.aggWg, startWg: startWg, id: "aggWorker"}, r.aggCh, r.cluster, r.StatFlushDuration, r.StatsNamePrefix, r, r)
}

var startPacedMetricWorker = func(r *Receiver, startWg *sync.WaitGroup) {
	log.Printf("Starting pacedMetricWorker...")
	startWg.Add(1)
	go pacedMetricWorker(&wrkCtl{wg: &r.pacedMetricWg, startWg: startWg, id: "pacedMetricWorker"}, r.pacedMetricCh, r, r, time.Second, r)
}
