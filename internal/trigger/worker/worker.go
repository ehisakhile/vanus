// Copyright 2022 Linkall Inc.
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

package worker

import (
	"context"
	"fmt"
	iInfo "github.com/linkall-labs/vanus/internal/primitive/info"
	"github.com/linkall-labs/vanus/internal/trigger/errors"
	"github.com/linkall-labs/vanus/internal/trigger/info"
	"github.com/linkall-labs/vanus/internal/trigger/offset"
	"os"
	"sync"
	"time"

	ce "github.com/cloudevents/sdk-go/v2"
	eb "github.com/linkall-labs/eventbus-go"
	"github.com/linkall-labs/eventbus-go/pkg/discovery"
	"github.com/linkall-labs/eventbus-go/pkg/discovery/record"
	"github.com/linkall-labs/eventbus-go/pkg/inmemory"
	"github.com/linkall-labs/vanus/internal/primitive"
	"github.com/linkall-labs/vanus/internal/trigger/reader"
	"github.com/linkall-labs/vanus/observability/log"
)

var (
	defaultEbVRN = "vanus+local:eventbus:example"
)

type Worker struct {
	subscriptions map[string]*subscriptionWorker
	offsetManager *offset.Manager
	lock          sync.RWMutex
	started       bool
	wg            sync.WaitGroup
	ctx           context.Context
	stop          context.CancelFunc
	config        Config
}

type subscriptionWorker struct {
	sub     *primitive.SubscriptionInfo
	trigger *Trigger
	events  chan info.EventLogOffset
	reader  *reader.Reader
}

func NewSubWorker(sub *primitive.SubscriptionInfo, subOffset *offset.SubscriptionOffset) *subscriptionWorker {
	w := &subscriptionWorker{
		events: make(chan info.EventLogOffset, 2048),
		sub:    sub,
	}
	w.trigger = NewTrigger(sub, subOffset)
	offset := make(map[string]int64)
	for _, o := range sub.Offsets {
		offset[o.EventLog] = o.Offset
	}
	w.reader = reader.NewReader(getReaderConfig(sub), offset, w.events)
	return w
}

func (w *subscriptionWorker) Run(ctx context.Context) error {
	err := w.reader.Start(ctx)
	if err != nil {
		return err
	}
	err = w.trigger.Start(ctx)
	if err != nil {
		return err
	}
	go func() {
		for event := range w.events {
			w.trigger.EventArrived(ctx, info.EventRecord{EventLogOffset: event})
		}
	}()
	return nil
}

func NewWorker(config Config) *Worker {
	w := &Worker{
		subscriptions: map[string]*subscriptionWorker{},
		offsetManager: offset.NewOffsetManager(),
		config:        config,
	}
	w.ctx, w.stop = context.WithCancel(context.Background())
	return w
}

func (w *Worker) Start() error {
	w.lock.Lock()
	defer w.lock.Unlock()
	if w.started {
		return nil
	}
	w.started = true
	testSend()
	return nil
}

func (w *Worker) Stop() error {
	w.lock.Lock()
	defer w.lock.Unlock()
	if !w.started {
		return nil
	}
	var wg sync.WaitGroup
	for id := range w.subscriptions {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			w.stopSub(id)
			w.cleanSub(id)
		}(id)
	}
	wg.Wait()
	w.stop()
	w.started = false
	return nil
}

func (w *Worker) stopSub(id string) {
	if info, exist := w.subscriptions[id]; exist {
		log.Info(w.ctx, "worker begin stop subscription", map[string]interface{}{
			"subId": id,
		})
		info.reader.Close()
		info.trigger.Stop()
		close(info.events)
		log.Info(w.ctx, "worker success stop subscription", map[string]interface{}{
			"subId": id,
		})
	}
}

func (w *Worker) cleanSub(id string) {
	w.offsetManager.RemoveSubscription(id)
	delete(w.subscriptions, id)
}

func (w *Worker) ListSubInfos() []iInfo.SubscriptionInfo {
	w.lock.RLock()
	defer w.lock.RUnlock()
	var list []iInfo.SubscriptionInfo
	for id := range w.subscriptions {
		list = append(list, iInfo.SubscriptionInfo{
			SubId:   id,
			Offsets: w.offsetManager.GetSubscription(id).GetCommit(),
		})
	}
	return list
}

func (w *Worker) AddSubscription(sub *primitive.SubscriptionInfo) error {
	w.lock.Lock()
	defer w.lock.Unlock()
	if !w.started {
		return errors.ErrWorkerNotStart
	}
	if _, exist := w.subscriptions[sub.ID]; exist {
		return errors.ErrResourceAlreadyExist
	}
	subOffset := w.offsetManager.RegisterSubscription(sub.ID)
	subWorker := NewSubWorker(sub, subOffset)
	err := subWorker.Run(w.ctx)
	if err != nil {
		w.offsetManager.RemoveSubscription(sub.ID)
		return err
	}
	w.subscriptions[sub.ID] = subWorker
	return nil
}

func (w *Worker) PauseSubscription(id string) error {
	w.lock.Lock()
	defer w.lock.Unlock()
	if !w.started {
		return errors.ErrWorkerNotStart
	}
	w.stopSub(id)
	return nil
}

func (w *Worker) RemoveSubscription(id string) error {
	w.lock.Lock()
	defer w.lock.Unlock()
	if !w.started {
		return errors.ErrWorkerNotStart
	}
	if _, exist := w.subscriptions[id]; !exist {
		return nil
	}
	w.stopSub(id)
	w.cleanSub(id)
	return nil
}

func getReaderConfig(sub *primitive.SubscriptionInfo) reader.Config {
	ebVrn := sub.EventBus
	if ebVrn == "" {
		ebVrn = defaultEbVRN
	}
	return reader.Config{
		EventBus: ebVrn,
	}
}

func testSend() {
	ebVRN := "vanus+local:eventbus:example"
	elVRN := "vanus+inmemory:eventlog:1"
	br := &record.EventBus{
		VRN: ebVRN,
		Logs: []*record.EventLog{
			{
				VRN:  elVRN,
				Mode: record.PremWrite | record.PremRead,
			},
		},
	}

	inmemory.UseInMemoryLog("vanus+inmemory")
	ns := inmemory.UseNameService("vanus+local")
	// register metadata of eventbus
	vrn, err := discovery.ParseVRN(ebVRN)
	if err != nil {
		panic(err.Error())
	}
	ns.Register(vrn, br)
	bw, err := eb.OpenBusWriter(ebVRN)
	if err != nil {
		log.Error(context.Background(), "open bus writer error", map[string]interface{}{"error": err})
		os.Exit(1)
	}

	go func() {
		i := 1
		tp := "none"
		for ; i < 10000; i++ {
			if i%3 == 0 {
				tp = "test"
			}
			if i%2 == 0 {
				time.Sleep(10 * time.Second)
				tp = "none"
			}
			// Create an Event.
			event := ce.NewEvent()
			event.SetID(fmt.Sprintf("%d", i))
			event.SetSource("example/uri")
			event.SetType(tp)
			event.SetData(ce.ApplicationJSON, map[string]string{"hello": "world", "type": tp})

			_, err = bw.Append(context.Background(), &event)
			if err != nil {
				log.Error(context.Background(), "append event error", map[string]interface{}{"error": err})
			}
		}
	}()
}
