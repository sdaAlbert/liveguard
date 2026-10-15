package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

const runtimeLogMaxBytes = 10 << 20

type runtimeLogWriter struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
	max  int64
}

func openRuntimeLog(path string, maxBytes int64) (*runtimeLogWriter, error) {
	if path == "" {
		return nil, fmt.Errorf("runtime log path is required")
	}
	if maxBytes <= 0 {
		maxBytes = runtimeLogMaxBytes
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	writer := &runtimeLogWriter{path: path, file: file, size: info.Size(), max: maxBytes}
	if writer.size >= writer.max {
		if err := writer.rotateLocked(); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return writer, nil
}

func (w *runtimeLogWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(data)) > w.max {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(data)
	w.size += int64(n)
	return n, err
}

func (w *runtimeLogWriter) rotateLocked() error {
	if err := w.file.Sync(); err != nil {
		return err
	}
	if err := w.file.Close(); err != nil {
		return err
	}
	backup := w.path + ".1"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	if w.size > 0 {
		if err := os.Rename(w.path, backup); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.file, w.size = file, 0
	return nil
}

func (w *runtimeLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Sync()
	closeErr := w.file.Close()
	w.file = nil
	if err != nil {
		return err
	}
	return closeErr
}

func configureRuntimeLogging() func() {
	path := envOr("LIVEGUARD_LOG_PATH", filepath.Join("runtime", "liveguard.log"))
	writer, err := openRuntimeLog(path, runtimeLogMaxBytes)
	if err != nil {
		log.Printf("runtime log unavailable at %s: %v", path, err)
		return func() {}
	}
	log.SetOutput(io.MultiWriter(os.Stderr, writer))
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("runtime log persisted to %s", path)
	return func() {
		log.Print("LiveGuard shutting down")
		if err := writer.Close(); err != nil {
			log.Printf("close runtime log: %v", err)
		}
	}
}
