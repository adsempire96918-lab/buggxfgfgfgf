// Package main — output files + resume (Task 4).
// Byte-compatible with the Python project: same 8 files, same line formats.
//
//	0to500.txt … 3000ormore.txt   one RAW input line per hit (e.g. "user:pass")
//	not_found.txt                 "user | NOT_FOUND"
//	banned.txt                    "user | SUSPENDED"
//	errors.txt                    "user | ERROR: <err>: <detail>"
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// KarmaBucket mirrors reddit_fetch.KARMA_BUCKETS; Upper<0 means infinity.
type KarmaBucket struct {
	Upper int64
	Name  string
}

// KarmaBuckets — same ranges and file names as the Python project.
var KarmaBuckets = []KarmaBucket{
	{500, "0to500.txt"},
	{1000, "500to1000.txt"},
	{2000, "1000to2000.txt"},
	{3000, "2000to3000.txt"},
	{-1, "3000ormore.txt"},
}

// KarmaBucketIndex — the bucket a karma value belongs to (same order as
// KarmaBuckets). Negative karma goes into the first bucket.
func KarmaBucketIndex(karma int64) int {
	for i, b := range KarmaBuckets {
		if b.Upper < 0 || karma <= b.Upper {
			return i
		}
	}
	return len(KarmaBuckets) - 1
}

// KarmaBucketName — parity with reddit_fetch.karma_bucket (negative -> first).
func KarmaBucketName(karma int64) string {
	return KarmaBuckets[KarmaBucketIndex(karma)].Name
}

// ResumeFiles are the files scanned for already-finished usernames.
var ResumeFiles = append(
	[]string{},
	append([]string{"not_found.txt", "banned.txt"},
		func() []string {
			names := make([]string, 0, len(KarmaBuckets))
			for _, b := range KarmaBuckets {
				names = append(names, b.Name)
			}
			return names
		}()...)...)

// OutputWriter appends to the 8 output files under one lock (parity with
// check_users.append_line + write_lock).
//
// PERFORMANCE: every Append used to do a write(2) syscall, which is the
// single biggest backend cost of a long run (one syscall per checked
// account, 500k+ of them). The writer now buffers per file and flushes in
// the background, so the hot path is a memcpy into a buffer.
//
// The buffers are flushed:
//   - every flushInterval (default 2s) by the background flusher,
//   - on Close (normal end of run),
//   - by Flush() which the caller can use before an intentional exit.
//
// A crash can therefore lose at most the last ~2 seconds of results; the
// resume scan then re-checks only those accounts.
type OutputWriter struct {
	Dir   string
	files map[string]*os.File
	bufs  map[string]*bufio.Writer
	mu    sync.Mutex
	dirty int
}

// flushInterval is how often the background flusher pushes the buffers to
// disk. 2s keeps the resume map fresh without a syscall per line.
const flushInterval = 2 * time.Second

// activeWriter is the writer of the running check. The signal handler uses
// it to flush the buffers when the user hits Ctrl+C, so a killed run still
// leaves a complete resume map on disk.
var activeWriter atomic.Pointer[OutputWriter]

// InstallSignalFlush makes Ctrl+C (and a polite SIGTERM) flush the output
// buffers before exiting. Without it the last ~2 seconds of results would be
// lost and the resume would re-check those accounts.
func InstallSignalFlush() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-ch
		fmt.Fprintf(os.Stderr, "\n%s received - flushing results and stopping (run again to resume)...\n", sig)
		if w := activeWriter.Load(); w != nil {
			w.Flush()
		}
		// Give the flush a moment, then leave.
		time.Sleep(150 * time.Millisecond)
		os.Exit(130)
	}()
}

// Open creates (if missing) and opens all 8 output files in append mode.
func (w *OutputWriter) Open() error {
	w.files = make(map[string]*os.File, len(KarmaBuckets)+3)
	w.bufs = make(map[string]*bufio.Writer, len(KarmaBuckets)+3)
	names := make([]string, 0, len(KarmaBuckets)+3)
	for _, b := range KarmaBuckets {
		names = append(names, b.Name)
	}
	names = append(names, "not_found.txt", "banned.txt", "errors.txt")
	for _, name := range names {
		f, err := os.OpenFile(filepath.Join(w.Dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			w.Close()
			return err
		}
		w.files[name] = f
		w.bufs[name] = bufio.NewWriterSize(f, 64*1024)
	}
	activeWriter.Store(w)
	// background flusher: keeps the resume map fresh during a long run
	go w.flushLoop()
	return nil
}

// flushLoop pushes the buffers to disk every flushInterval until Close.
func (w *OutputWriter) flushLoop() {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for range t.C {
		w.Flush()
	}
}

// Flush pushes every buffered file to disk.
func (w *OutputWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.bufs {
		if b != nil {
			b.Flush()
		}
	}
	w.dirty = 0
}

// Close flushes and closes all open files.
func (w *OutputWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for name, b := range w.bufs {
		if b != nil {
			b.Flush()
			delete(w.bufs, name)
		}
	}
	for name, f := range w.files {
		f.Close()
		delete(w.files, name)
	}
}

// Append writes one line + newline (parity with append_line). Python's text
// mode writes CRLF on Windows, so we do the same for byte-identical output.
//
// The write goes into a per-file buffer - no syscall in the hot path.
func (w *OutputWriter) Append(name, line string) {
	w.mu.Lock()
	if b, ok := w.bufs[name]; ok {
		b.WriteString(line)
		b.WriteString(lineEnding)
		w.dirty++
	}
	w.mu.Unlock()
}

// lineEnding matches Python's text-mode newline translation on this platform.
var lineEnding = "\n"

func init() {
	if runtime.GOOS == "windows" {
		lineEnding = "\r\n"
	}
}

// LoadLines reads the input list — parity with check_users.load_lines
// (UTF-8/UTF-16 BOM, blank lines and # comments skipped, lines stripped).
func LoadLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := readAll(f)
	if err != nil {
		return nil, err
	}
	var text string
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE {
		text = decodeUTF16LE(data[2:])
	} else {
		text = string(data)
	}
	text = strings.TrimPrefix(text, "\ufeff")
	raw := strings.Split(text, "\n")
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		l = strings.TrimRight(l, "\r")
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// DedupKey returns the identity of an input line for duplicate detection.
//
// The FULL "user:pass" is the identity, not just the user name: the same
// account with a different password is a DIFFERENT credential and must be
// checked separately. The key is lower-cased so "User:Pass" and "user:pass"
// are recognised as the same row.
func DedupKey(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// DedupLines removes exact duplicate "user:pass" rows while keeping the
// FIRST occurrence (and the original order). It reports how many rows were
// dropped so the run can tell the user.
func DedupLines(lines []string) ([]string, int) {
	seen := make(map[string]bool, len(lines))
	out := make([]string, 0, len(lines))
	dupes := 0
	for _, l := range lines {
		k := DedupKey(l)
		if k == "" {
			continue
		}
		if seen[k] {
			dupes++
			continue
		}
		seen[k] = true
		out = append(out, l)
	}
	return out, dupes
}

// ReadDoneUsers collects finished usernames from the resume files — parity
// with check_users.read_done_users (first token before '|', normalized, lower).
// Tolerates a UTF-8 BOM (external editors), like load_lines does for input.
func ReadDoneUsers(dir string) map[string]bool {
	done := make(map[string]bool)
	for _, name := range ResumeFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		text := strings.TrimPrefix(string(data), "\ufeff")
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			head := line
			if i := strings.Index(line, "|"); i >= 0 {
				head = line[:i]
			}
			if u := NormalizeUsername(head); u != "" {
				done[strings.ToLower(u)] = true
			}
		}
	}
	return done
}

// readAll + decodeUTF16LE are small stdlib-free helpers for input loading.
func readAll(f *os.File) ([]byte, error) {
	// Pre-size the buffer from the file size: the input list is ~13 MB and
	// the old append-per-chunk growth copied the data many times.
	st, err := f.Stat()
	if err == nil && st.Size() > 0 {
		buf := make([]byte, 0, st.Size()+4096)
		tmp := make([]byte, 256*1024)
		for {
			n, err := f.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				if err.Error() == "EOF" {
					return buf, nil
				}
				return buf, err
			}
		}
	}
	buf := make([]byte, 64*1024)
	out := []byte{}
	for {
		n, err := f.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
	}
}

func decodeUTF16LE(b []byte) string {
	var sb strings.Builder
	for i := 0; i+1 < len(b); i += 2 {
		sb.WriteRune(rune(b[i]) | rune(b[i+1])<<8)
	}
	return sb.String()
}
