package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

var errDownloadIdleTimeout = errors.New("download body idle timeout")

// Canceling the HTTP request interrupts a blocked Read. Checking elapsed time
// only after Read returns would leave stalled responses blocked indefinitely.
type downloadProgressReader struct {
	reader       io.Reader
	timeout      time.Duration
	cancel       context.CancelCauseFunc
	mu           sync.Mutex
	lastProgress time.Time
	timer        *time.Timer
	stopped      bool
}

func newDownloadProgressReader(reader io.Reader, timeout time.Duration, cancel context.CancelCauseFunc) *downloadProgressReader {
	if timeout <= 0 {
		timeout = defaultDownloadIdleTimeout
	}
	progress := &downloadProgressReader{reader: reader, timeout: timeout, cancel: cancel, lastProgress: time.Now()}
	progress.mu.Lock()
	progress.timer = time.AfterFunc(timeout, progress.check)
	progress.mu.Unlock()
	return progress
}

func (reader *downloadProgressReader) check() {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.stopped {
		return
	}
	if remaining := reader.timeout - time.Since(reader.lastProgress); remaining > 0 {
		reader.timer.Reset(remaining)
		return
	}
	reader.stopped = true
	reader.cancel(fmt.Errorf("%w after %s without data", errDownloadIdleTimeout, reader.timeout))
}

func (reader *downloadProgressReader) Read(buffer []byte) (int, error) {
	n, err := reader.reader.Read(buffer)
	if n > 0 {
		reader.mu.Lock()
		reader.lastProgress = time.Now()
		reader.mu.Unlock()
	}
	if err != nil {
		reader.stop()
	}
	return n, err
}

func (reader *downloadProgressReader) stop() {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.stopped = true
	reader.timer.Stop()
}
