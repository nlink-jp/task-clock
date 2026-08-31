// Package logrotate is a minimal size-capped rotating file writer for the
// daemon's own operational log. Under launchd the daemon's stdout goes
// nowhere, so serve tees its log lines into this writer; rotation keeps
// the file from growing forever (KeepAlive daemons run for months).
package logrotate

import (
	"fmt"
	"os"
	"sync"
)

// Writer appends to path, rotating to path.1 … path.<keep> when a write
// would push the file past maxBytes. Writes are serialized; a rotation
// failure degrades to appending in place rather than losing lines.
type Writer struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	maxBytes int64
	keep     int
	size     int64
}

// New opens (creating if needed) the log at path.
func New(path string, maxBytes int64, keep int) (*Writer, error) {
	if maxBytes <= 0 || keep < 1 {
		return nil, fmt.Errorf("logrotate: maxBytes and keep must be positive")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{f: f, path: path, maxBytes: maxBytes, keep: keep, size: info.Size()}, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.maxBytes && w.size > 0 {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate shifts path.(keep-1)→path.keep … path→path.1 and reopens path.
// Called with the lock held.
func (w *Writer) rotate() {
	w.f.Close()
	for i := w.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	os.Rename(w.path, w.path+".1")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		// Degrade: reopen the (renamed or original) file so lines keep
		// landing somewhere instead of being dropped.
		f, err = os.OpenFile(w.path+".1", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		w.f = f
		return
	}
	w.f = f
	w.size = 0
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
