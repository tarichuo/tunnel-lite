// Package logging provides a small bounded file writer for structured server logs.
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Writer struct {
	mu      sync.Mutex
	path    string
	max     int64
	backups int
	file    *os.File
	size    int64
}

func Open(path string, max int64, backups int) (*Writer, error) {
	if max < 1 || backups < 1 {
		return nil, fmt.Errorf("invalid log limits")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	w := &Writer{path: path, max: max, backups: backups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}
func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.file = f
	w.size = st.Size()
	return nil
}
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if int64(len(p)) > w.max {
		return 0, fmt.Errorf("log record exceeds file size limit")
	}
	if w.size+int64(len(p)) > w.max {
		if err := w.file.Close(); err != nil {
			return 0, err
		}
		w.file = nil
		for i := w.backups; i >= 1; i-- {
			src := w.path
			if i > 1 {
				src = fmt.Sprintf("%s.%d", w.path, i-1)
			}
			dest := fmt.Sprintf("%s.%d", w.path, i)
			if err := os.Rename(src, dest); err != nil && !os.IsNotExist(err) {
				w.open()
				return 0, err
			}
		}
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
