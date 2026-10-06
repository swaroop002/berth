package reconcile

import (
	"sync"
	"time"
)

// queue is a de-duplicating work queue with per-key exponential backoff: the
// same environment is never processed by two workers at once, and a failing
// one backs off instead of hammering the provider.
type queue struct {
	mu       sync.Mutex
	ready    chan string
	inReady  map[string]bool        // pushed to `ready`, not yet taken
	timers   map[string]*time.Timer // scheduled for later
	active   map[string]bool        // being processed right now
	again    map[string]bool        // requested while active: run once more after
	failures map[string]int
	min, max time.Duration
	closed   bool
}

func newQueue(min, max time.Duration) *queue {
	return &queue{ready: make(chan string, 4096), inReady: map[string]bool{}, timers: map[string]*time.Timer{},
		active: map[string]bool{}, again: map[string]bool{}, failures: map[string]int{}, min: min, max: max}
}

// Add schedules a key now (idempotent). It overrides a pending delayed add.
func (q *queue) Add(key string) { q.AddAfter(key, 0) }

// AddAfter schedules a key after a delay. An immediate add wins over a
// delayed one; a delayed add never postpones an immediate one.
func (q *queue) AddAfter(key string, d time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if q.active[key] {
		q.again[key] = true
		return
	}
	if q.inReady[key] {
		return
	}
	if d <= 0 {
		if t := q.timers[key]; t != nil {
			t.Stop()
			delete(q.timers, key)
		}
		q.push(key)
		return
	}
	if q.timers[key] != nil {
		return
	}
	q.timers[key] = time.AfterFunc(d, func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		delete(q.timers, key)
		if !q.closed && !q.inReady[key] && !q.active[key] {
			q.push(key)
		}
	})
}

// push requires q.mu held.
func (q *queue) push(key string) {
	select {
	case q.ready <- key:
		q.inReady[key] = true
	default:
		// Buffer full: try again shortly rather than block a producer.
		q.timers[key] = time.AfterFunc(10*time.Millisecond, func() {
			q.mu.Lock()
			defer q.mu.Unlock()
			delete(q.timers, key)
			if !q.closed && !q.inReady[key] {
				q.push(key)
			}
		})
	}
}

// Get blocks for the next key; ok is false once the queue is closed.
func (q *queue) Get() (key string, ok bool) {
	key, ok = <-q.ready
	if !ok {
		return "", false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return "", false
	}
	delete(q.inReady, key)
	q.active[key] = true
	return key, true
}

// Done marks processing finished. On failure the key is re-added with
// backoff; on success the failure count resets. A request that arrived while
// the key was active is honoured now.
func (q *queue) Done(key string, failed bool, requeueAfter time.Duration) {
	q.mu.Lock()
	delete(q.active, key)
	again := q.again[key]
	delete(q.again, key)
	var delay time.Duration
	if failed {
		n := q.failures[key]
		q.failures[key] = n + 1
		delay = q.min << uint(min(n, 20))
		if delay > q.max || delay <= 0 {
			delay = q.max
		}
	} else {
		delete(q.failures, key)
		delay = requeueAfter
	}
	q.mu.Unlock()
	switch {
	case again:
		q.Add(key)
	case failed || requeueAfter > 0:
		q.AddAfter(key, delay)
	}
}

// Len is the number of keys waiting or scheduled.
func (q *queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.inReady) + len(q.timers)
}

// Failures of a key so far (for status.attempts and tests).
func (q *queue) Failures(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.failures[key]
}

func (q *queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		for _, t := range q.timers {
			t.Stop()
		}
		close(q.ready)
	}
}
