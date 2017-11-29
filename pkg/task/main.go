/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package task

import (
	"time"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
)

const maxRetries = 15

// Queue manages a work queue through an independent worker that
// invokes the given sync function for every work item inserted.
type Queue struct {
	// queue is the work queue the worker polls
	queue workqueue.RateLimitingInterface
	// sync is called for each item in the queue
	sync func(interface{}) error
	// workerDone is closed when the worker exits
	workerDone chan bool
}

func (r *Queue) handleErr(err error, obj interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		r.queue.Forget(obj)
		r.queue.Done(obj)
		return
	}

	// This controller retries maxRetries times if something goes wrong. After that, it stops trying.
	if r.queue.NumRequeues(obj) < maxRetries {
		glog.Errorf("error syncing restore request (%v): %v", obj, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		r.queue.AddRateLimited(obj)
		return
	}

	r.queue.Forget(obj)
	r.queue.Done(obj)
	// Report that, even after several retries, we could not successfully process this key
	glog.Infof("dropping restore request (%v) out of the queue: %v", obj, err)
}

// Run ...
func (t *Queue) Run(period time.Duration, stopCh <-chan struct{}) {
	wait.Until(t.worker, period, stopCh)
}

// Enqueue enqueues ns/name of the given api object in the task queue.
func (t *Queue) Enqueue(obj interface{}) {
	if t.IsShuttingDown() {
		glog.Errorf("queue has been shutdown, failed to enqueue: %v", obj)
		return
	}
	glog.V(3).Infof("queuing item %v", obj)
	t.queue.Add(obj)
}

// worker processes work in the queue through sync.
func (t *Queue) worker() {
	for {
		obj, quit := t.queue.Get()
		if quit {
			if !isClosed(t.workerDone) {
				close(t.workerDone)
			}
			return
		}

		glog.V(3).Infof("syncing %v", obj)

		err := t.sync(obj)
		t.handleErr(err, obj)
	}
}

func isClosed(ch <-chan bool) bool {
	select {
	case <-ch:
		return true
	default:
	}

	return false
}

// Shutdown shuts down the work queue and waits for the worker to ACK
func (t *Queue) Shutdown() {
	t.queue.ShutDown()
	<-t.workerDone
}

// IsShuttingDown returns if the method Shutdown was invoked
func (t *Queue) IsShuttingDown() bool {
	return t.queue.ShuttingDown()
}

// NewTaskQueue creates a new task queue with the given sync function.
// The sync function is called for every element inserted into the queue.
func NewTaskQueue(syncFn func(interface{}) error) *Queue {
	return NewCustomTaskQueue(syncFn, nil)
}

// NewCustomTaskQueue ...
func NewCustomTaskQueue(syncFn func(interface{}) error, fn func(interface{}) (interface{}, error)) *Queue {
	q := &Queue{
		queue:      workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		sync:       syncFn,
		workerDone: make(chan bool),
	}
	return q
}
