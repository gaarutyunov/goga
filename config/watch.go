package config

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchDebounce is how long the watcher waits for the file to stop changing
// before it reloads.
//
// One logical change is several inotify/kqueue events: an editor writes a
// temporary file and renames it over the target, and a Kubernetes ConfigMap
// update swaps a symlinked directory. Without a quiet period the callback runs
// two or three times per save, each rebuilding whatever the application rebuilds
// on a reload. Fifty milliseconds is far below anything a human notices and far
// above the gap between the events of one write.
const watchDebounce = 50 * time.Millisecond

// watcher reloads the configuration when a watched file changes.
//
// It watches the parent DIRECTORY of each file rather than the file itself, and
// that is the whole design. A watch on a file follows the inode, so the moment
// anything replaces the file — which is what every editor, every `mv` and every
// ConfigMap update does — the watch is attached to an unlinked inode that will
// never change again, and the reload silently stops happening. A directory
// watch sees the new name appear.
//
// The same reasoning rules out koanf's own file provider here, which was tried
// first: its watcher treats a Remove on the watched name as fatal and stops the
// goroutine. On macOS a rename over an existing path emits Remove before
// Create, so an atomically-replaced config file kills the watch on the first
// save — the exact case a config watcher exists for.
type watcher struct {
	fsw *fsnotify.Watcher

	// files is the set of cleaned paths this watcher cares about. Every other
	// event in the watched directories is ignored.
	files map[string]bool

	onChange func(path string)
	onError  func(err error)

	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// newWatcher starts watching the directories holding paths.
//
// onChange is called with the path of a file that changed, at most once per
// [watchDebounce] window per file; onError with anything the underlying watcher
// reports. Both run on the watcher's goroutine.
func newWatcher(paths []string, onChange func(string), onError func(error)) (*watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("goga/config: watch: %w", err)
	}

	w := &watcher{
		fsw:      fsw,
		files:    make(map[string]bool, len(paths)),
		onChange: onChange,
		onError:  onError,
		done:     make(chan struct{}),
	}

	dirs := make(map[string]bool, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		w.files[clean] = true
		dirs[filepath.Dir(clean)] = true
	}

	for dir := range dirs {
		if err := fsw.Add(dir); err != nil {
			// Nothing is watched yet that the caller could observe, so the
			// half-built watcher is torn down rather than returned.
			_ = fsw.Close()
			return nil, fmt.Errorf("goga/config: watch %s: %w", dir, err)
		}
	}

	w.wg.Add(1)
	go w.run()

	return w, nil
}

// run is the watcher's event loop.
func (w *watcher) run() {
	defer w.wg.Done()

	var (
		pending = map[string]bool{}
		timer   = time.NewTimer(watchDebounce)
	)
	// Start with the timer stopped: it is armed by the first event.
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		select {
		case <-w.done:
			return

		case event, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if !w.interesting(event) {
				continue
			}
			pending[filepath.Clean(event.Name)] = true
			// Go 1.23 and later discard a value the timer already sent, so
			// resetting an expired timer cannot leave a stale tick behind.
			timer.Reset(watchDebounce)

		case <-timer.C:
			for path := range pending {
				w.onChange(path)
			}
			pending = map[string]bool{}

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.onError(err)
		}
	}
}

// interesting reports whether event concerns one of the watched files.
//
// Remove and Rename count. A file that disappears has changed the
// configuration as surely as one that was edited — under [WithFile] its values
// are simply gone, and under [WithRequiredFile] the reload fails and says so —
// and treating either as the end of the watch is what breaks an atomic
// replacement. Chmod does not count: a permission or timestamp change alters no
// content.
func (w *watcher) interesting(event fsnotify.Event) bool {
	if !w.files[filepath.Clean(event.Name)] {
		return false
	}
	return event.Has(fsnotify.Create) ||
		event.Has(fsnotify.Write) ||
		event.Has(fsnotify.Remove) ||
		event.Has(fsnotify.Rename)
}

// Close stops the watcher and waits for its goroutine to finish, so that no
// callback can run after Close returns. Calling it twice is harmless.
func (w *watcher) Close() error {
	var err error
	w.closeOnce.Do(func() {
		close(w.done)
		err = w.fsw.Close()
		w.wg.Wait()
	})
	if err != nil {
		return fmt.Errorf("goga/config: watch close: %w", err)
	}
	return nil
}
