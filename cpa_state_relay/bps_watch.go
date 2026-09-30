package main

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// A quiet connection and a heartbeat-only response need separate deadlines.
const bpsIdleTimeout = 120 * time.Second
const bpsProgressTimeout = 300 * time.Second

type bpsStreamState struct {
	LastEvent    string
	LastEventAt  time.Time
	PendingTools int
}

type bpsWatch struct {
	source                 io.ReadCloser
	mu                     sync.Mutex
	timer                  *time.Timer
	once                   sync.Once
	idle, progress         time.Duration
	lastByte, lastProgress time.Time
	stopped                bool
	failure                error
}

func newBPSWatch(source io.ReadCloser, idle, progress time.Duration) *bpsWatch {
	now := time.Now()
	w := &bpsWatch{source: source, idle: idle, progress: progress, lastByte: now, lastProgress: now}
	w.timer = time.AfterFunc(min(idle, progress), w.check)
	return w
}
func (w *bpsWatch) check() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	now := time.Now()
	remaining := min(w.idle-now.Sub(w.lastByte), w.progress-now.Sub(w.lastProgress))
	if remaining > 0 {
		w.timer.Reset(remaining)
		w.mu.Unlock()
		return
	}
	if now.Sub(w.lastByte) >= w.idle {
		w.failure = fmt.Errorf("BPS stream idle timeout: no data for %s", w.idle)
	} else {
		w.failure = fmt.Errorf("BPS stream progress timeout: no content progress for %s", w.progress)
	}
	w.stopped = true
	w.mu.Unlock()
	w.once.Do(func() { _ = w.source.Close() })
}
func (w *bpsWatch) Read(p []byte) (int, error) {
	n, err := w.source.Read(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failure != nil {
		return 0, w.failure
	}
	if n > 0 {
		w.lastByte = time.Now()
	}
	return n, err
}
func (w *bpsWatch) Progress() {
	w.mu.Lock()
	if !w.stopped {
		w.lastProgress = time.Now()
	}
	w.mu.Unlock()
}
func (w *bpsWatch) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failure
}
func (w *bpsWatch) Canceled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopped && w.failure == nil
}
func (w *bpsWatch) Close() error {
	w.mu.Lock()
	w.stopped = true
	w.timer.Stop()
	w.mu.Unlock()
	var err error
	w.once.Do(func() { err = w.source.Close() })
	return err
}
