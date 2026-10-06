package core

import (
	"sync"
	"time"
)

// Queue runs posted functions in order on one goroutine. All engine state
// lives on it, so the engine needs no locks.
type Queue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	items   []func()
	stopped bool
	onPanic func(any)
}

func NewQueue(onPanic func(any)) *Queue {
	q := &Queue{onPanic: onPanic}
	q.cond = sync.NewCond(&q.mu)
	go q.run()
	return q
}

func (q *Queue) Post(f func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return
	}
	q.items = append(q.items, f)
	q.cond.Signal()
}

func (q *Queue) PostAfter(d time.Duration, f func()) {
	time.AfterFunc(d, func() { q.Post(f) })
}

// Run posts f and waits for it, at most timeout. It reports whether f ran.
func (q *Queue) Run(f func(), timeout time.Duration) bool {
	done := make(chan struct{})
	q.Post(func() {
		defer close(done)
		f()
	})
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (q *Queue) Stop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stopped = true
	q.cond.Signal()
}

func (q *Queue) run() {
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.stopped {
			q.cond.Wait()
		}
		if q.stopped && len(q.items) == 0 {
			q.mu.Unlock()
			return
		}
		f := q.items[0]
		q.items[0] = nil
		q.items = q.items[1:]
		q.mu.Unlock()
		q.call(f)
	}
}

func (q *Queue) call(f func()) {
	defer func() {
		if r := recover(); r != nil && q.onPanic != nil {
			q.onPanic(r)
		}
	}()
	f()
}
