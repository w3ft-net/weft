package weftd

import (
	"bufio"
	"io"
	"log"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Tailer watches a log file and emits new lines on a channel.
// Handles file truncation (logrotate copytruncate) and recreation.
type Tailer struct {
	path    string
	file    *os.File
	watcher *fsnotify.Watcher
	lines   chan string
	done    chan struct{}
}

// NewTailer starts tailing a file from the current end position.
func NewTailer(path string) (*Tailer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// Seek to end — only tail new content.
	f.Seek(0, io.SeekEnd)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := watcher.Add(path); err != nil {
		f.Close()
		watcher.Close()
		return nil, err
	}

	t := &Tailer{
		path:    path,
		file:    f,
		watcher: watcher,
		lines:   make(chan string, 1024),
		done:    make(chan struct{}),
	}
	go t.watch()
	return t, nil
}

// Lines returns the channel of new log lines.
func (t *Tailer) Lines() <-chan string {
	return t.lines
}

// Close stops the tailer.
func (t *Tailer) Close() {
	close(t.done)
	t.watcher.Close()
	t.file.Close()
}

func (t *Tailer) watch() {
	defer close(t.lines)

	scanner := bufio.NewScanner(t.file)

	for {
		select {
		case <-t.done:
			return

		case event, ok := <-t.watcher.Events:
			if !ok {
				return
			}

			if event.Op&fsnotify.Write == fsnotify.Write {
				// Read new lines.
				for scanner.Scan() {
					t.lines <- scanner.Text()
				}
				// Reset scanner error state for next read.
				scanner = bufio.NewScanner(t.file)
			}

			if event.Op&fsnotify.Remove == fsnotify.Remove || event.Op&fsnotify.Rename == fsnotify.Rename {
				// File rotated — wait briefly then reopen.
				t.file.Close()
				time.Sleep(100 * time.Millisecond)
				f, err := os.Open(t.path)
				if err != nil {
					log.Printf("tailer[%s]: reopen after rotation: %v", t.path, err)
					return
				}
				t.file = f
				scanner = bufio.NewScanner(t.file)
				// Re-watch the new file.
				t.watcher.Add(t.path)
			}

		case err, ok := <-t.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("tailer[%s]: watch error: %v", t.path, err)
		}
	}
}
