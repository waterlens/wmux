package transcript

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStoreSegmentsCapsAndReopens(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(Config{Dir: dir, Limits: Limits{SegmentBytes: 150, MaxBytes: 450, SyncWrites: true}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		sequence, err := store.Append([]byte{byte(i), 0, 0xff})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if sequence != uint64(i) {
			t.Fatalf("sequence = %d, want %d", sequence, i)
		}
	}
	oldest, newest := store.Bounds()
	if oldest <= 1 {
		t.Fatalf("oldest = %d; size cap did not evict old segments", oldest)
	}
	if newest != 10 {
		t.Fatalf("newest = %d, want 10", newest)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(Config{Dir: dir, Limits: Limits{SegmentBytes: 150, MaxBytes: 450, SyncWrites: true}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var gotSequences []uint64
	var gotData [][]byte
	if err := store.Replay(0, 0, func(sequence uint64, _ time.Time, data []byte) error {
		gotSequences = append(gotSequences, sequence)
		gotData = append(gotData, append([]byte(nil), data...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(gotSequences) == 0 || gotSequences[0] != oldest || gotSequences[len(gotSequences)-1] != 10 {
		t.Fatalf("replayed sequences = %v, bounds = %d..10", gotSequences, oldest)
	}
	for i, sequence := range gotSequences {
		want := []byte{byte(sequence), 0, 0xff}
		if !reflect.DeepEqual(gotData[i], want) {
			t.Fatalf("data for sequence %d = %v, want %v", sequence, gotData[i], want)
		}
	}
	sequence, err := store.Append([]byte("after-restart"))
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 11 {
		t.Fatalf("sequence after restart = %d, want 11", sequence)
	}
}

func TestAppendFailureRollsBackAndReusesSequence(t *testing.T) {
	tests := []struct {
		name  string
		fault func(*faultAppendFile)
	}{
		{name: "short write", fault: func(file *faultAppendFile) { file.shortWrite = true }},
		{name: "write error", fault: func(file *faultAppendFile) { file.writeError = true }},
		{name: "sync error", fault: func(file *faultAppendFile) { file.syncError = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(Config{Dir: dir, Limits: Limits{SyncWrites: true}})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if sequence, err := store.Append([]byte("first")); err != nil || sequence != 1 {
				t.Fatalf("first append = (%d, %v)", sequence, err)
			}
			path := store.segments[len(store.segments)-1].path
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			fault := &faultAppendFile{appendFile: store.active}
			test.fault(fault)
			store.active = fault

			if sequence, err := store.Append([]byte("must roll back")); err == nil || sequence != 0 {
				t.Fatalf("failed append = (%d, %v), want (0, error)", sequence, err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if after.Size() != before.Size() {
				t.Fatalf("segment size after rollback = %d, want %d", after.Size(), before.Size())
			}
			if oldest, newest := store.Bounds(); oldest != 1 || newest != 1 {
				t.Fatalf("bounds after rollback = %d..%d", oldest, newest)
			}
			if sequence, err := store.Append([]byte("second")); err != nil || sequence != 2 {
				t.Fatalf("retry append = (%d, %v), want (2, nil)", sequence, err)
			}

			var sequences []uint64
			var payloads []string
			if err := store.Replay(0, 0, func(sequence uint64, _ time.Time, data []byte) error {
				sequences = append(sequences, sequence)
				payloads = append(payloads, string(data))
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sequences, []uint64{1, 2}) || !reflect.DeepEqual(payloads, []string{"first", "second"}) {
				t.Fatalf("replay after recovery = %v %v", sequences, payloads)
			}
		})
	}
}

type faultAppendFile struct {
	appendFile
	shortWrite bool
	writeError bool
	syncError  bool
}

func (f *faultAppendFile) Write(data []byte) (int, error) {
	if f.shortWrite {
		f.shortWrite = false
		return f.appendFile.Write(data[:len(data)/2])
	}
	if f.writeError {
		f.writeError = false
		n, _ := f.appendFile.Write(data[:len(data)/2])
		return n, errors.New("injected write failure")
	}
	return f.appendFile.Write(data)
}

func (f *faultAppendFile) Sync() error {
	if f.syncError {
		f.syncError = false
		return errors.New("injected sync failure")
	}
	return f.appendFile.Sync()
}

func TestStoreUsesJSONLBase64(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{0, '\n', 0xff, 'x'}
	if _, err := store.Append(payload); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "segment-*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("segments = %v, err = %v", paths, err)
	}
	line, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record diskRecord
	if err := json.Unmarshal(line, &record); err != nil {
		t.Fatalf("JSONL record: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(record.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, payload) {
		t.Fatalf("decoded data = %v, want %v", decoded, payload)
	}
}

func TestStoreRecoversTruncatedLastRecord(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(Config{Dir: dir, Limits: Limits{SyncWrites: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append([]byte("complete")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "segment-*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("segments = %v, err = %v", paths, err)
	}
	f, err := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"sequence":2,"time":"`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("recover open: %v", err)
	}
	defer store.Close()
	if _, newest := store.Bounds(); newest != 1 {
		t.Fatalf("newest after recovery = %d, want 1", newest)
	}
	if sequence, err := store.Append([]byte("after recovery")); err != nil || sequence != 2 {
		t.Fatalf("append after recovery = (%d, %v), want (2, nil)", sequence, err)
	}
}

func TestStoreRecoversMalformedCompleteTail(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(Config{Dir: dir, Limits: Limits{SyncWrites: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append([]byte("complete")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "segment-*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("segments = %v, err = %v", paths, err)
	}
	f, err := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	malformed, err := json.Marshal(diskRecord{Sequence: 2, Time: time.Now().UTC(), Data: "%%%"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(malformed, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(Config{Dir: dir, Limits: Limits{SyncWrites: true}})
	if err != nil {
		t.Fatalf("recover malformed tail: %v", err)
	}
	defer store.Close()
	if sequence, err := store.Append([]byte("second")); err != nil || sequence != 2 {
		t.Fatalf("append after malformed tail = (%d, %v)", sequence, err)
	}
}

func TestStoreRejectsCorruptionBeforeTail(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(Config{Dir: dir, Limits: Limits{SyncWrites: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "segment-*.jsonl"))
	contents, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	newline := bytes.IndexByte(contents, '\n')
	if newline < 0 {
		t.Fatal("record did not contain newline")
	}
	corrupt := append([]byte("not-json\n"), contents[newline+1:]...)
	if err := os.WriteFile(paths[0], corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(Config{Dir: dir}); err == nil {
		_ = reopened.Close()
		t.Fatal("expected non-tail corruption to be rejected")
	}
}

func TestAppendRollbackFailurePoisonsLiveStoreAndReopenRecovers(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append([]byte("first")); err != nil {
		t.Fatal(err)
	}
	store.active = &truncateFaultFile{appendFile: store.active}
	if _, err := store.Append([]byte("partial")); err == nil {
		t.Fatal("expected append rollback failure")
	}
	if _, err := store.Append([]byte("blocked")); err == nil {
		t.Fatal("expected poisoned store to reject later append")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("reopen poisoned store: %v", err)
	}
	defer store.Close()
	if sequence, err := store.Append([]byte("second")); err != nil || sequence != 2 {
		t.Fatalf("append after reopen = (%d, %v)", sequence, err)
	}
}

type truncateFaultFile struct{ appendFile }

func (f *truncateFaultFile) Write(data []byte) (int, error) {
	n, _ := f.appendFile.Write(data[:len(data)/2])
	return n, io.ErrUnexpectedEOF
}

func (f *truncateFaultFile) Truncate(int64) error {
	return errors.New("injected truncate failure")
}

func TestDirectorySeparatesUnsafeSessionIDsAndRemovesOne(t *testing.T) {
	root := t.TempDir()
	directory, err := NewDirectory(DirectoryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	first, err := directory.Open("a/b")
	if err != nil {
		t.Fatal(err)
	}
	second, err := directory.Open("a_b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Append([]byte("second")); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	_ = second.Close()
	if err := directory.Remove("a/b"); err != nil {
		t.Fatal(err)
	}
	second, err = directory.Open("a_b")
	if err != nil {
		t.Fatalf("reopen unaffected transcript: %v", err)
	}
	defer second.Close()
	_, newest := second.Bounds()
	if newest != 1 {
		t.Fatalf("unrelated transcript newest = %d, want 1", newest)
	}
}
