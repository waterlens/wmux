package transcript

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultSegmentBytes int64 = 1 << 20
	defaultMaxBytes     int64 = 8 << 20
	maxScanToken              = 16 << 20
)

var ErrClosed = errors.New("transcript: log is closed")

// Log is the append-only output history consumed by the terminal runtime.
// Replay calls yield while holding a consistent snapshot: an Append cannot
// interleave with a replay.
type Log interface {
	Append(data []byte) (uint64, error)
	Replay(after uint64, limit int, yield func(sequence uint64, at time.Time, data []byte) error) error
	Bounds() (oldest, newest uint64)
	Close() error
}

// Factory opens one durable log per terminal session.
type Factory interface {
	Open(sessionID string) (Log, error)
}

type Config struct {
	Dir          string
	SegmentBytes int64
	MaxBytes     int64
	SyncWrites   bool
}

type diskRecord struct {
	Sequence uint64    `json:"sequence"`
	Time     time.Time `json:"time"`
	Data     string    `json:"data"`
}

type segment struct {
	path  string
	first uint64
	last  uint64
	size  int64
}

type appendFile interface {
	Write([]byte) (int, error)
	Sync() error
	Truncate(int64) error
	Close() error
}

// Store is a segmented, size-bounded JSONL transcript. Data is represented as
// standard base64 so arbitrary terminal bytes survive a process restart.
type Store struct {
	mu sync.Mutex

	cfg      Config
	segments []segment
	active   appendFile
	closed   bool
	failed   error
	oldest   uint64
	newest   uint64
	total    int64
}

func Open(cfg Config) (*Store, error) {
	if cfg.Dir == "" {
		return nil, errors.New("transcript: directory is required")
	}
	if cfg.SegmentBytes <= 0 {
		cfg.SegmentBytes = defaultSegmentBytes
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.SegmentBytes > cfg.MaxBytes {
		cfg.SegmentBytes = cfg.MaxBytes
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("transcript: create directory: %w", err)
	}

	s := &Store{cfg: cfg}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	paths, err := filepath.Glob(filepath.Join(s.cfg.Dir, "segment-*.jsonl"))
	if err != nil {
		return fmt.Errorf("transcript: list segments: %w", err)
	}
	sort.Strings(paths)

	for index, path := range paths {
		seg, err := inspectSegment(path, index == len(paths)-1)
		if err != nil {
			return err
		}
		if seg.first == 0 {
			// An empty file can be left by a crash immediately after rotation.
			_ = os.Remove(path)
			continue
		}
		if s.newest != 0 && seg.first <= s.newest {
			return fmt.Errorf("transcript: overlapping sequence at %s", path)
		}
		s.segments = append(s.segments, seg)
		s.total += seg.size
		if s.oldest == 0 {
			s.oldest = seg.first
		}
		s.newest = seg.last
	}

	if err := s.trimLocked(); err != nil {
		return err
	}
	if len(s.segments) != 0 {
		last := &s.segments[len(s.segments)-1]
		if last.size < s.cfg.SegmentBytes {
			f, err := os.OpenFile(last.path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return fmt.Errorf("transcript: reopen segment: %w", err)
			}
			s.active = f
		}
	}
	return nil
}

func inspectSegment(path string, recoverTail bool) (segment, error) {
	flags := os.O_RDONLY
	if recoverTail {
		flags = os.O_RDWR
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return segment{}, fmt.Errorf("transcript: open %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return segment{}, fmt.Errorf("transcript: stat %s: %w", path, err)
	}
	seg := segment{path: path, size: info.Size()}
	reader := bufio.NewReaderSize(f, 64<<10)
	var previous uint64
	var validSize int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			if recoverTail && errors.Is(readErr, io.EOF) {
				if err := f.Truncate(validSize); err != nil {
					return segment{}, fmt.Errorf("transcript: recover %s: %w", path, err)
				}
				seg.size = validSize
				break
			}
			return segment{}, fmt.Errorf("transcript: read %s: %w", path, readErr)
		}
		var rec diskRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			if recoverTail && readerAtEOF(reader) {
				if truncateErr := f.Truncate(validSize); truncateErr != nil {
					return segment{}, fmt.Errorf("transcript: recover %s: %w", path, truncateErr)
				}
				seg.size = validSize
				break
			}
			return segment{}, fmt.Errorf("transcript: decode %s: %w", path, err)
		}
		if rec.Sequence == 0 || (previous != 0 && rec.Sequence <= previous) {
			if recoverTail && readerAtEOF(reader) {
				if truncateErr := f.Truncate(validSize); truncateErr != nil {
					return segment{}, fmt.Errorf("transcript: recover %s: %w", path, truncateErr)
				}
				seg.size = validSize
				break
			}
			return segment{}, fmt.Errorf("transcript: non-monotonic sequence in %s", path)
		}
		if _, err := base64.StdEncoding.DecodeString(rec.Data); err != nil {
			if recoverTail && readerAtEOF(reader) {
				if truncateErr := f.Truncate(validSize); truncateErr != nil {
					return segment{}, fmt.Errorf("transcript: recover %s: %w", path, truncateErr)
				}
				seg.size = validSize
				break
			}
			return segment{}, fmt.Errorf("transcript: invalid base64 in %s: %w", path, err)
		}
		if seg.first == 0 {
			seg.first = rec.Sequence
		}
		seg.last = rec.Sequence
		previous = rec.Sequence
		validSize += int64(len(line))
	}
	return seg, nil
}

func readerAtEOF(reader *bufio.Reader) bool {
	_, err := reader.Peek(1)
	return errors.Is(err, io.EOF)
}

func (s *Store) Append(data []byte) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	if s.failed != nil {
		return 0, s.failed
	}

	sequence := s.newest + 1
	rec := diskRecord{
		Sequence: sequence,
		Time:     time.Now().UTC(),
		Data:     base64.StdEncoding.EncodeToString(data),
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return 0, fmt.Errorf("transcript: encode record: %w", err)
	}
	line = append(line, '\n')

	if s.active == nil || (len(s.segments) != 0 && s.segments[len(s.segments)-1].size > 0 && s.segments[len(s.segments)-1].size+int64(len(line)) > s.cfg.SegmentBytes) {
		if err := s.rotateLocked(sequence); err != nil {
			return 0, err
		}
	}
	last := &s.segments[len(s.segments)-1]
	previousSize := last.size
	n, writeErr := s.active.Write(line)
	if writeErr != nil || n != len(line) {
		cause := writeErr
		if cause == nil {
			cause = io.ErrShortWrite
		}
		return 0, s.rollbackAppendLocked(previousSize, fmt.Errorf("transcript: append: %w", cause))
	}
	if s.cfg.SyncWrites {
		if err := s.active.Sync(); err != nil {
			return 0, s.rollbackAppendLocked(previousSize, fmt.Errorf("transcript: sync: %w", err))
		}
	}

	// Commit in-memory state only after the complete record (and requested
	// durability barrier) succeeds. A retry therefore reuses this sequence.
	last.last = sequence
	last.size += int64(len(line))
	s.total += int64(len(line))
	if s.oldest == 0 {
		s.oldest = sequence
	}
	s.newest = sequence
	// Retention is maintenance after commit. Failure to remove an old segment
	// must not report the successfully committed sequence as failed; a later
	// append/open retries trimming.
	_ = s.trimLocked()
	return sequence, nil
}

func (s *Store) rollbackAppendLocked(size int64, cause error) error {
	if err := s.active.Truncate(size); err != nil {
		s.failed = errors.Join(cause, fmt.Errorf("transcript: roll back append: %w", err))
		return s.failed
	}
	if s.cfg.SyncWrites {
		if err := s.active.Sync(); err != nil {
			s.failed = errors.Join(cause, fmt.Errorf("transcript: sync append rollback: %w", err))
			return s.failed
		}
	}
	return cause
}

func (s *Store) rotateLocked(first uint64) error {
	if s.active != nil {
		if err := s.active.Close(); err != nil {
			return fmt.Errorf("transcript: close segment: %w", err)
		}
		s.active = nil
	}
	path := filepath.Join(s.cfg.Dir, fmt.Sprintf("segment-%020d.jsonl", first))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("transcript: create segment: %w", err)
	}
	s.active = f
	s.segments = append(s.segments, segment{path: path, first: first})
	return nil
}

func (s *Store) trimLocked() error {
	defer s.refreshBoundsLocked()
	// Keep the active/newest segment even when one unusually large record is
	// larger than MaxBytes. The next rotation makes it eligible for eviction.
	for s.total > s.cfg.MaxBytes && len(s.segments) > 1 {
		old := s.segments[0]
		if err := os.Remove(old.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("transcript: remove old segment: %w", err)
		}
		s.total -= old.size
		s.segments = s.segments[1:]
	}
	return nil
}

func (s *Store) refreshBoundsLocked() {
	if len(s.segments) == 0 {
		s.oldest = 0
		s.newest = 0
	} else {
		s.oldest = s.segments[0].first
		s.newest = s.segments[len(s.segments)-1].last
	}
}

func (s *Store) Replay(after uint64, limit int, yield func(sequence uint64, at time.Time, data []byte) error) error {
	if yield == nil {
		return errors.New("transcript: replay callback is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}

	remaining := limit
	for _, seg := range s.segments {
		if seg.last <= after {
			continue
		}
		f, err := os.Open(seg.path)
		if err != nil {
			return fmt.Errorf("transcript: open replay segment: %w", err)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64<<10), maxScanToken)
		for scanner.Scan() {
			var rec diskRecord
			if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
				_ = f.Close()
				return fmt.Errorf("transcript: decode replay: %w", err)
			}
			if rec.Sequence <= after {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(rec.Data)
			if err != nil {
				_ = f.Close()
				return fmt.Errorf("transcript: decode replay data: %w", err)
			}
			if err := yield(rec.Sequence, rec.Time, data); err != nil {
				_ = f.Close()
				return err
			}
			if limit > 0 {
				remaining--
				if remaining == 0 {
					_ = f.Close()
					return nil
				}
			}
		}
		scanErr := scanner.Err()
		closeErr := f.Close()
		if scanErr != nil {
			return fmt.Errorf("transcript: scan replay: %w", scanErr)
		}
		if closeErr != nil {
			return fmt.Errorf("transcript: close replay segment: %w", closeErr)
		}
	}
	return nil
}

func (s *Store) Bounds() (oldest, newest uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.oldest, s.newest
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.active == nil {
		return nil
	}
	err := s.active.Close()
	s.active = nil
	return err
}

type DirectoryConfig struct {
	Root         string
	SegmentBytes int64
	MaxBytes     int64
	SyncWrites   bool
}

// Directory is a filesystem-backed Factory. Session IDs are escaped into a
// single safe path component rather than used as paths directly.
type Directory struct {
	cfg DirectoryConfig
}

func NewDirectory(cfg DirectoryConfig) (*Directory, error) {
	if cfg.Root == "" {
		return nil, errors.New("transcript: root directory is required")
	}
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, fmt.Errorf("transcript: create root: %w", err)
	}
	return &Directory{cfg: cfg}, nil
}

func (d *Directory) Open(sessionID string) (Log, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("transcript: session ID is required")
	}
	return Open(Config{
		Dir:          filepath.Join(d.cfg.Root, safeComponent(sessionID)),
		SegmentBytes: d.cfg.SegmentBytes,
		MaxBytes:     d.cfg.MaxBytes,
		SyncWrites:   d.cfg.SyncWrites,
	})
}

// Remove deletes exactly one session transcript. Callers must close its Log
// first. The hashed, sanitized component prevents traversal and collisions.
func (d *Directory) Remove(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("transcript: session ID is required")
	}
	path := filepath.Join(d.cfg.Root, safeComponent(sessionID))
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("transcript: remove session: %w", err)
	}
	return nil
}

func safeComponent(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		name = "session"
	}
	if len(name) > 48 {
		name = name[:48]
	}
	hash := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%s-%x", name, hash[:6])
}
