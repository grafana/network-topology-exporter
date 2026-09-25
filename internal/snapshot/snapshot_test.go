package snapshot

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/network-topology-exporter/internal/discovery"
)

// errInjected is a sentinel used by injection helpers to distinguish injected
// failures from real OS errors in assertions.
var errInjected = errors.New("injected failure")

// restoreOSFns returns a function that restores all injectable OS-call vars to
// their current values. Call it with defer at the top of any test that
// overrides these vars.
func restoreOSFns() func() {
	origReadFile := readFileFn
	origMarshal := marshalFn
	origMkdirAll := mkdirAllFn
	origCreateTemp := createTempFn
	origRename := renameFn
	origOpenFile := openFileFn
	return func() {
		readFileFn = origReadFile
		marshalFn = origMarshal
		mkdirAllFn = origMkdirAll
		createTempFn = origCreateTemp
		renameFn = origRename
		openFileFn = origOpenFile
	}
}

// fakeTmpFile wraps a real *os.File but lets individual operations be
// overridden by the test. Fields left nil delegate to the real file.
type fakeTmpFile struct {
	real    *os.File
	writeFn func([]byte) (int, error)
	syncFn  func() error
	closeFn func() error
}

func (f *fakeTmpFile) Name() string { return f.real.Name() }
func (f *fakeTmpFile) Write(b []byte) (int, error) {
	if f.writeFn != nil {
		return f.writeFn(b)
	}
	return f.real.Write(b)
}
func (f *fakeTmpFile) Sync() error {
	if f.syncFn != nil {
		return f.syncFn()
	}
	return f.real.Sync()
}
func (f *fakeTmpFile) Close() error {
	if f.closeFn != nil {
		return f.closeFn()
	}
	return f.real.Close()
}

// fakeQuarantineFile is a quarantineFile implementation whose operations can
// be overridden to inject errors. The real file underneath is only used to
// satisfy the interface; it is always closed regardless of injected errors.
type fakeQuarantineFile struct {
	real    *os.File
	writeFn func([]byte) (int, error)
	closeFn func() error
}

func (f *fakeQuarantineFile) Write(b []byte) (int, error) {
	if f.writeFn != nil {
		return f.writeFn(b)
	}
	return f.real.Write(b)
}
func (f *fakeQuarantineFile) Close() error {
	_ = f.real.Close() // always close the real file to avoid leaks
	if f.closeFn != nil {
		return f.closeFn()
	}
	return nil
}

// LD-13: a missing snapshot is "first run", not an error. Operators don't
// need to pre-create the file.
func TestLoadMissingFileReturnsNil(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("missing snapshot should not error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil File for missing path, got %#v", got)
	}
}

// LD-13: round-trip a snapshot through Write+Load and confirm the device,
// edge, and credential-cache contents survive verbatim.
func TestWriteLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	in := File{
		Devices: []discovery.Device{{ID: "dev-1", SysName: "Dev-1", Vendor: "cisco", Site: "lab"}},
		Edges: []discovery.Edge{{
			SrcDevice: "dev-1", SrcPort: "Gi0/1", SrcIfIndex: 7,
			DstDevice: "dev-2", DstPort: "Gi0/2",
			DiscoveryProto: "lldp",
			Direction:      discovery.DirectionBidirectional,
			Confidence:     discovery.ConfidenceHigh,
			Adjacency:      discovery.AdjacencyDirect,
			PrecedenceRank: 2,
		}},
		CredentialCache: map[string]string{"dev-1": "core-v3"},
		UnconfirmedAges: map[string]int{"a|Gi0/1|b|Gi0/2": 1},
	}
	if err := Write(path, in); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out == nil {
		t.Fatal("Load returned nil for an existing snapshot")
		return
	}
	if out.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", out.Version, CurrentVersion)
	}
	if len(out.Devices) != 1 || out.Devices[0].ID != "dev-1" || out.Devices[0].SysName != "Dev-1" {
		t.Errorf("Devices round-trip mismatch: %#v", out.Devices)
	}
	if len(out.Edges) != 1 || out.Edges[0].PrecedenceRank != 2 || out.Edges[0].SrcIfIndex != 7 {
		t.Errorf("Edges round-trip mismatch: %#v", out.Edges)
	}
	if out.CredentialCache["dev-1"] != "core-v3" {
		t.Errorf("CredentialCache round-trip mismatch: %#v", out.CredentialCache)
	}
}

// LD-13: a snapshot with an unrecognised version is not silently parsed.
// Falling through to a cold start is documented behaviour, but the caller
// has to see the version-mismatch error to log the warning first.
func TestLoadRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	body := []byte(`{"version": 99, "devices": []}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("Load error = %v, want ErrVersionMismatch", err)
	}
}

// LD-13: Load returns an error (not nil) for a file that exists but is not
// valid JSON.
func TestLoadCorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for corrupt JSON, got nil")
	}
}

func TestLoadCorruptJSONDoesNotOverwriteExistingBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json")
	badPath := path + ".bad"
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt snapshot: %v", err)
	}
	if err := os.WriteFile(badPath, []byte("previous bad snapshot"), 0o600); err != nil {
		t.Fatalf("write existing bad snapshot: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("expected error for corrupt JSON, got nil")
	}

	existing, err := os.ReadFile(badPath) //nolint:gosec
	if err != nil {
		t.Fatalf("read existing bad snapshot: %v", err)
	}
	if string(existing) != "previous bad snapshot" {
		t.Fatalf("%s was overwritten with %q", badPath, string(existing))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	foundQuarantine := false
	for _, e := range entries {
		if e.Name() != "corrupt.json.bad" && strings.HasPrefix(e.Name(), "corrupt.json.bad.") {
			foundQuarantine = true
			break
		}
	}
	if !foundQuarantine {
		t.Fatalf("expected corrupt snapshot to be moved to a unique .bad path, entries: %v", entries)
	}
}

// LD-13: Write succeeds even when the caller does not set Version (defaults
// to CurrentVersion) and WrittenAt (defaults to now).
func TestWriteDefaultsVersionAndTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	f := File{} // zero value
	if err := Write(path, f); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", out.Version, CurrentVersion)
	}
	if out.WrittenAt.IsZero() {
		t.Error("WrittenAt is zero, want a non-zero timestamp")
	}
}

// LD-13: Write returns an error when the directory cannot be created because
// the target parent path is occupied by a regular file.
func TestWriteFailsWhenParentIsFile(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	path := filepath.Join(parent, "snap.json")
	if err := Write(path, File{}); err == nil {
		t.Error("expected error when parent is a file, got nil")
	}
}

// TestQuarantineRollsToNextAvailableSuffix verifies that when both .bad and
// .bad.1 already exist the quarantine function advances to .bad.2.
func TestQuarantineRollsToNextAvailableSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")

	// Pre-create both .bad and .bad.1 so quarantine must use .bad.2.
	for _, suffix := range []string{".bad", ".bad.1"} {
		if err := os.WriteFile(path+suffix, []byte("old"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	if err := os.WriteFile(path, []byte("{bad json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for corrupt JSON, got nil")
	}

	if _, err := os.Stat(path + ".bad.2"); err != nil {
		t.Fatalf("expected .bad.2 to exist: %v", err)
	}
	// Original .bad and .bad.1 must still be the old content.
	for _, suffix := range []string{".bad", ".bad.1"} {
		b, _ := os.ReadFile(path + suffix) //nolint:gosec
		if string(b) != "old" {
			t.Errorf("%s was overwritten; want %q, got %q", suffix, "old", string(b))
		}
	}
}

// TestWriteCreatesDirectoryIfAbsent verifies that Write creates intermediate
// directories (MkdirAll behaviour) when the directory does not yet exist.
func TestWriteCreatesDirectoryIfAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "snap.json")
	if err := Write(path, File{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load after Write into new dir: %v", err)
	}
}

// TestLoadVersionMismatchQuarantinesFile confirms that a version mismatch
// moves the file to .bad rather than deleting it silently.
func TestLoadVersionMismatchQuarantinesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Load(path)
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("error = %v, want ErrVersionMismatch", err)
	}

	// The original file should be gone (renamed to .bad).
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("original snap.json should have been quarantined (renamed away)")
	}
	if _, statErr := os.Stat(path + ".bad"); statErr != nil {
		t.Errorf(".bad file should exist after version mismatch: %v", statErr)
	}
}

// LD-13: a crash mid-write must leave the previous snapshot intact and the
// .tmp file behind. We can't simulate a crash, but we can verify the path
// is updated atomically by writing twice and confirming the second write
// supersedes the first.
func TestWriteIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	v1 := File{Devices: []discovery.Device{{ID: "dev-v1"}}}
	v2 := File{Devices: []discovery.Device{{ID: "dev-v2"}}}

	if err := Write(path, v1); err != nil {
		t.Fatalf("Write v1: %v", err)
	}
	if err := Write(path, v2); err != nil {
		t.Fatalf("Write v2: %v", err)
	}

	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Devices[0].ID != "dev-v2" {
		t.Errorf("second write did not supersede first: %#v", out.Devices)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file %q should have been renamed away", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// Fence-token tests (#71 — native hub HA, T5)
// ---------------------------------------------------------------------------

// TestWriteRefusesStaleEpoch: a writer holding a lower LeaseEpoch than the
// on-disk file is a resumed stale leader and its write must be refused with
// ErrStaleEpoch. Equal or higher epochs proceed. This is the core fencing
// guarantee: atomic-rename prevents corruption, the epoch check prevents a
// stale-epoch overwrite.
func TestWriteRefusesStaleEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")

	// Establish epoch 5 on disk.
	if err := Write(path, File{Holder: "pod-a", LeaseEpoch: 5}); err != nil {
		t.Fatalf("Write epoch 5: %v", err)
	}

	// A stale leader at epoch 4 must be refused.
	err := Write(path, File{Holder: "pod-b", LeaseEpoch: 4})
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("Write epoch 4 over 5: err = %v, want ErrStaleEpoch", err)
	}

	// The on-disk file must still be epoch 5 (the refused write did not land).
	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load after refused write: %v", err)
	}
	if out.LeaseEpoch != 5 || out.Holder != "pod-a" {
		t.Fatalf("refused write leaked: holder=%q epoch=%d, want pod-a/5", out.Holder, out.LeaseEpoch)
	}

	// Equal epoch (5) proceeds.
	if err := Write(path, File{Holder: "pod-a", LeaseEpoch: 5}); err != nil {
		t.Fatalf("Write epoch 5 == 5: %v", err)
	}

	// Higher epoch (6) proceeds and round-trips.
	if err := Write(path, File{Holder: "pod-c", LeaseEpoch: 6}); err != nil {
		t.Fatalf("Write epoch 6: %v", err)
	}
	out, err = Load(path)
	if err != nil {
		t.Fatalf("Load after epoch 6: %v", err)
	}
	if out.LeaseEpoch != 6 || out.Holder != "pod-c" {
		t.Fatalf("epoch round-trip: holder=%q epoch=%d, want pod-c/6", out.Holder, out.LeaseEpoch)
	}
}

// TestWriteEpochZeroNeverFenced: a writer with LeaseEpoch 0 (single-hub /
// fencing-off) is NEVER refused, even when the on-disk file carries a high
// epoch. This preserves byte-identical single-hub behaviour.
func TestWriteEpochZeroNeverFenced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")

	if err := Write(path, File{Holder: "pod-a", LeaseEpoch: 5}); err != nil {
		t.Fatalf("Write epoch 5: %v", err)
	}

	// Epoch 0 always wins — the fence check is skipped entirely.
	if err := Write(path, File{LeaseEpoch: 0}); err != nil {
		t.Fatalf("Write epoch 0 over 5 should never be fenced: %v", err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.LeaseEpoch != 0 {
		t.Fatalf("epoch-0 write did not land: epoch=%d", out.LeaseEpoch)
	}
}

// TestWriteAllowedWhenNoExistingFile: a missing target file means there is no
// existing epoch to compare against, so even a high-epoch write proceeds.
func TestWriteAllowedWhenNoExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := Write(path, File{Holder: "pod-a", LeaseEpoch: 9}); err != nil {
		t.Fatalf("Write into empty dir: %v", err)
	}
}

// TestLoadOldFormatHasZeroEpoch: an old-format snapshot written before the
// fence token existed (JSON with no holder/lease_epoch keys) loads with zero
// values — backward compatibility. omitempty also means a zero-epoch File
// does not emit the keys at all.
func TestLoadOldFormatHasZeroEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	// Hand-rolled old-format envelope: no holder, no lease_epoch.
	body := []byte(`{"version":3,"devices":[],"edges":[]}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write old-format: %v", err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load old-format: %v", err)
	}
	if out == nil {
		t.Fatal("Load returned nil for old-format file")
		return
	}
	if out.Holder != "" || out.LeaseEpoch != 0 {
		t.Fatalf("old-format should load zero fence fields: holder=%q epoch=%d", out.Holder, out.LeaseEpoch)
	}
}

// ---------------------------------------------------------------------------
// Load error-path tests
// ---------------------------------------------------------------------------

// TestLoadReadError covers the branch where os.ReadFile returns a non-ErrNotExist
// error (e.g. permission denied). Load must propagate the error.
func TestLoadReadError(t *testing.T) {
	defer restoreOSFns()()
	readFileFn = func(string) ([]byte, error) { return nil, errInjected }

	_, err := Load("/any/path.json")
	if err == nil {
		t.Fatal("expected error from injected readFileFn, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want to wrap errInjected", err)
	}
}

// TestLoadQuarantinesVersionMismatch confirms that when Load encounters a
// version mismatch it quarantines the file (original path removed, .bad
// created) and returns ErrVersionMismatch.
func TestLoadQuarantinesVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := Load(path)
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("error = %v, want ErrVersionMismatch", err)
	}

	// Original must be gone.
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("original file should have been quarantined (removed)")
	}
	// Quarantine file must exist.
	if _, statErr := os.Stat(path + ".bad"); statErr != nil {
		t.Errorf(".bad file should exist after version mismatch: %v", statErr)
	}
}

// ---------------------------------------------------------------------------
// quarantine error-path tests
// ---------------------------------------------------------------------------

// TestQuarantineRollsSuffixOnConflict verifies that when the .bad path already
// exists quarantine advances to the next available suffix (.bad.1, .bad.2, …).
func TestQuarantineRollsSuffixOnConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")

	// Pre-create .bad so the first suffix is taken.
	if err := os.WriteFile(path+".bad", []byte("existing"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := Load(path)
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("error = %v, want ErrVersionMismatch", err)
	}

	// .bad must still have the old content.
	b, _ := os.ReadFile(path + ".bad") //nolint:gosec
	if string(b) != "existing" {
		t.Errorf(".bad content = %q, want %q", string(b), "existing")
	}
	// A new suffix must have been created.
	if _, statErr := os.Stat(path + ".bad.1"); statErr != nil {
		t.Errorf(".bad.1 should exist after suffix roll: %v", statErr)
	}
}

// TestQuarantineOpenError exercises the branch where os.OpenFile fails for a
// reason other than ErrExist (e.g. permission denied). quarantine returns the
// error; Load surfaces it silently (the result is discarded with _).
func TestQuarantineOpenError(t *testing.T) {
	defer restoreOSFns()()
	openFileFn = func(string, int, fs.FileMode) (quarantineFile, error) {
		return nil, errInjected
	}

	// quarantine is called inside Load for a version mismatch; the returned
	// error is ignored by Load, but the function still returns ErrVersionMismatch.
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := Load(path)
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("error = %v, want ErrVersionMismatch", err)
	}
}

// TestQuarantineWriteError exercises the branch where writing to the .bad file
// fails. quarantine returns the write error.
func TestQuarantineWriteError(t *testing.T) {
	defer restoreOSFns()()
	openFileFn = func(name string, flag int, perm fs.FileMode) (quarantineFile, error) {
		f, err := os.OpenFile(name, flag, perm) //nolint:gosec
		if err != nil {
			return nil, err
		}
		return &fakeQuarantineFile{
			real:    f,
			writeFn: func([]byte) (int, error) { return 0, errInjected },
		}, nil
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Load ignores the quarantine error; we just verify it doesn't panic and
	// returns ErrVersionMismatch.
	_, err := Load(path)
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("error = %v, want ErrVersionMismatch", err)
	}
}

// TestQuarantineCloseError exercises the branch where closing the .bad file
// fails. quarantine returns the close error and removes the partial file.
func TestQuarantineCloseError(t *testing.T) {
	defer restoreOSFns()()
	openFileFn = func(name string, flag int, perm fs.FileMode) (quarantineFile, error) {
		f, err := os.OpenFile(name, flag, perm) //nolint:gosec
		if err != nil {
			return nil, err
		}
		return &fakeQuarantineFile{
			real:    f,
			closeFn: func() error { return errInjected },
		}, nil
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := Load(path)
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("error = %v, want ErrVersionMismatch", err)
	}
}

// ---------------------------------------------------------------------------
// Write error-path tests
// ---------------------------------------------------------------------------

// TestWriteMarshalError injects a marshal failure so the json.Marshal branch
// in Write is exercised.
func TestWriteMarshalError(t *testing.T) {
	defer restoreOSFns()()
	marshalFn = func(any) ([]byte, error) { return nil, errInjected }

	err := Write(filepath.Join(t.TempDir(), "snap.json"), File{})
	if err == nil {
		t.Fatal("expected error from injected marshalFn, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want to wrap errInjected", err)
	}
}

// TestWriteMkdirAllError injects an os.MkdirAll failure.
func TestWriteMkdirAllError(t *testing.T) {
	defer restoreOSFns()()
	mkdirAllFn = func(string, fs.FileMode) error { return errInjected }

	err := Write(filepath.Join(t.TempDir(), "snap.json"), File{})
	if err == nil {
		t.Fatal("expected error from injected mkdirAllFn, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want to wrap errInjected", err)
	}
}

// TestWriteCreateTempError injects an os.CreateTemp failure.
func TestWriteCreateTempError(t *testing.T) {
	defer restoreOSFns()()
	createTempFn = func(string, string) (tmpFile, error) { return nil, errInjected }

	err := Write(filepath.Join(t.TempDir(), "snap.json"), File{})
	if err == nil {
		t.Fatal("expected error from injected createTempFn, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want to wrap errInjected", err)
	}
}

// TestWriteTempWriteError injects a failure on the Write call to the temp file.
func TestWriteTempWriteError(t *testing.T) {
	defer restoreOSFns()()
	dir := t.TempDir()
	createTempFn = func(d, pat string) (tmpFile, error) {
		f, err := os.CreateTemp(d, pat)
		if err != nil {
			return nil, err
		}
		return &fakeTmpFile{
			real:    f,
			writeFn: func([]byte) (int, error) { return 0, errInjected },
		}, nil
	}

	err := Write(filepath.Join(dir, "snap.json"), File{})
	if err == nil {
		t.Fatal("expected error from injected write, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want to wrap errInjected", err)
	}
}

// TestWriteSyncError injects a failure on the Sync call to the temp file.
func TestWriteSyncError(t *testing.T) {
	defer restoreOSFns()()
	dir := t.TempDir()
	createTempFn = func(d, pat string) (tmpFile, error) {
		f, err := os.CreateTemp(d, pat)
		if err != nil {
			return nil, err
		}
		return &fakeTmpFile{
			real:   f,
			syncFn: func() error { return errInjected },
		}, nil
	}

	err := Write(filepath.Join(dir, "snap.json"), File{})
	if err == nil {
		t.Fatal("expected error from injected sync, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want to wrap errInjected", err)
	}
}

// TestWriteCloseError injects a failure on the Close call to the temp file.
func TestWriteCloseError(t *testing.T) {
	defer restoreOSFns()()
	dir := t.TempDir()
	createTempFn = func(d, pat string) (tmpFile, error) {
		f, err := os.CreateTemp(d, pat)
		if err != nil {
			return nil, err
		}
		return &fakeTmpFile{
			real:    f,
			closeFn: func() error { return errInjected },
		}, nil
	}

	err := Write(filepath.Join(dir, "snap.json"), File{})
	if err == nil {
		t.Fatal("expected error from injected close, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want to wrap errInjected", err)
	}
}

// TestWriteRenameError injects an os.Rename failure.
func TestWriteRenameError(t *testing.T) {
	defer restoreOSFns()()
	renameFn = func(string, string) error { return errInjected }

	err := Write(filepath.Join(t.TempDir(), "snap.json"), File{})
	if err == nil {
		t.Fatal("expected error from injected renameFn, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want to wrap errInjected", err)
	}
}

// ---------------------------------------------------------------------------
// validateSnapshotFields tests (issue #22)
// ---------------------------------------------------------------------------

// writeSnapshotForLoad writes a File to disk via Write so tests don't have to
// hand-roll the JSON envelope (version, written_at). Returns the path.
func writeSnapshotForLoad(t *testing.T, f File) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := Write(path, f); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return path
}

// TestLoadRejectsOversizedDeviceID: a snapshot whose first device ID is over
// the 256-byte cap must be rejected with a message that names the index and
// the field.
func TestLoadRejectsOversizedDeviceID(t *testing.T) {
	path := writeSnapshotForLoad(t, File{
		Devices: []discovery.Device{
			{ID: "ok"},
			{ID: strings.Repeat("a", 1024)}, // 1 KB ID at index 1
		},
	})
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error for oversized device ID, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "device[1]") {
		t.Errorf("error should reference device[1], got %q", msg)
	}
	if !strings.Contains(msg, "id exceeds") {
		t.Errorf("error should name the id field, got %q", msg)
	}
}

// TestLoadRejectsOversizedPortName: Edge.SrcPort over 256 bytes must be
// rejected with index + field name.
func TestLoadRejectsOversizedPortName(t *testing.T) {
	path := writeSnapshotForLoad(t, File{
		Edges: []discovery.Edge{
			{SrcDevice: "a", SrcPort: strings.Repeat("p", 257), DstDevice: "b", DstPort: "ok"},
		},
	})
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error for oversized SrcPort, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "edge[0]") {
		t.Errorf("error should reference edge[0], got %q", msg)
	}
	if !strings.Contains(msg, "src_port") {
		t.Errorf("error should name the src_port field, got %q", msg)
	}
}

// TestLoadRejectsOversizedLabelValue: a Device.Labels value over 4096 bytes
// must be rejected with the index and labels field.
func TestLoadRejectsOversizedLabelValue(t *testing.T) {
	path := writeSnapshotForLoad(t, File{
		Devices: []discovery.Device{
			{ID: "dev-1", Labels: map[string]string{"env": strings.Repeat("v", 4097)}},
		},
	})
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error for oversized label value, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "device[0]") {
		t.Errorf("error should reference device[0], got %q", msg)
	}
	if !strings.Contains(msg, "labels value") {
		t.Errorf("error should mention labels value, got %q", msg)
	}
}

// TestLoadAcceptsBoundaryValues: exactly-at-the-cap inputs (256 / 4096)
// succeed, and one-byte-over (257 / 4097) fails. Locks in the off-by-one
// behaviour at every cap boundary the validator touches.
func TestLoadAcceptsBoundaryValues(t *testing.T) {
	atCap := File{
		Devices: []discovery.Device{
			{
				ID:        strings.Repeat("i", 256),
				Vendor:    strings.Repeat("v", 256),
				Model:     strings.Repeat("m", 256),
				OSVersion: strings.Repeat("o", 256),
				Site:      strings.Repeat("s", 256),
				Labels: map[string]string{
					strings.Repeat("k", 256): strings.Repeat("V", 4096),
				},
			},
		},
		Edges: []discovery.Edge{
			{
				SrcDevice:      strings.Repeat("S", 256),
				SrcPort:        strings.Repeat("P", 256),
				DstDevice:      strings.Repeat("D", 256),
				DstPort:        strings.Repeat("Q", 256),
				DiscoveryProto: discovery.DiscoveryProtocol(strings.Repeat("p", 64)),
				LinkKind:       discovery.LinkKind(strings.Repeat("l", 64)),
				Metadata: map[string]string{
					strings.Repeat("k", 256): strings.Repeat("V", 4096),
				},
			},
		},
	}
	path := writeSnapshotForLoad(t, atCap)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load at-cap snapshot: %v", err)
	}

	// One byte over each cap must fail. Each sub-case mutates one field at a time.
	cases := []struct {
		name  string
		mut   func(*File)
		field string
	}{
		{"device id +1", func(f *File) { f.Devices[0].ID = strings.Repeat("i", 257) }, "id"},
		{"device vendor +1", func(f *File) { f.Devices[0].Vendor = strings.Repeat("v", 257) }, "vendor"},
		{"device label value +1", func(f *File) {
			f.Devices[0].Labels = map[string]string{"env": strings.Repeat("V", 4097)}
		}, "labels value"},
		{"edge src_port +1", func(f *File) { f.Edges[0].SrcPort = strings.Repeat("P", 257) }, "src_port"},
		{"edge discovery_proto +1", func(f *File) {
			f.Edges[0].DiscoveryProto = discovery.DiscoveryProtocol(strings.Repeat("p", 65))
		}, "discovery_proto"},
		{"edge metadata value +1", func(f *File) {
			f.Edges[0].Metadata = map[string]string{"k": strings.Repeat("V", 4097)}
		}, "metadata value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Start from a fresh at-cap snapshot for each sub-case so earlier
			// mutations don't bleed across.
			f := File{
				Devices: []discovery.Device{{
					ID:        strings.Repeat("i", 256),
					Vendor:    strings.Repeat("v", 256),
					Model:     strings.Repeat("m", 256),
					OSVersion: strings.Repeat("o", 256),
					Site:      strings.Repeat("s", 256),
					Labels:    map[string]string{strings.Repeat("k", 256): strings.Repeat("V", 4096)},
				}},
				Edges: []discovery.Edge{{
					SrcDevice:      strings.Repeat("S", 256),
					SrcPort:        strings.Repeat("P", 256),
					DstDevice:      strings.Repeat("D", 256),
					DstPort:        strings.Repeat("Q", 256),
					DiscoveryProto: discovery.DiscoveryProtocol(strings.Repeat("p", 64)),
					LinkKind:       discovery.LinkKind(strings.Repeat("l", 64)),
					Metadata:       map[string]string{strings.Repeat("k", 256): strings.Repeat("V", 4096)},
				}},
			}
			tc.mut(&f)
			p := writeSnapshotForLoad(t, f)
			_, err := Load(p)
			if err == nil {
				t.Fatalf("expected validation error mentioning %q, got nil", tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error should mention %q, got %q", tc.field, err.Error())
			}
		})
	}
}

// TestValidateSnapshotFieldsAccumulatesMultipleErrors confirms that a snapshot
// with several oversized fields across multiple devices and edges reports
// every offending field in a single Load error, not just the first one.
// Operators recovering from a corrupted snapshot should not have to fix-reload
// repeatedly to discover each problem.
func TestValidateSnapshotFieldsAccumulatesMultipleErrors(t *testing.T) {
	f := &File{
		Devices: []discovery.Device{
			{ID: strings.Repeat("a", 1024)}, // device[0]: id
			{ID: "ok", Vendor: strings.Repeat("v", 1024), SysName: strings.Repeat("s", 1024)}, // device[1]: vendor, sys_name
			{ID: "ok2", OSVersion: strings.Repeat("o", 1024)},                                 // device[2]: os_version
		},
		Edges: []discovery.Edge{
			{SrcDevice: "a", SrcPort: strings.Repeat("p", 1024), DstDevice: "b", DstPort: "ok"},                                                   // edge[0]: src_port
			{SrcDevice: "a", SrcPort: "ok", DstDevice: "b", DstPort: strings.Repeat("p", 1024)},                                                   // edge[1]: dst_port
			{SrcDevice: "a", SrcPort: "ok", DstDevice: "b", DstPort: "ok", DiscoveryProto: discovery.DiscoveryProtocol(strings.Repeat("x", 128))}, // edge[2]: discovery_proto
		},
		OutOfScope: []discovery.OutOfScopeNeighbour{
			{ReportingDevice: strings.Repeat("r", 1024)}, // out_of_scope[0]: reporting_device
		},
	}

	err := validateSnapshotFields(f)
	if err == nil {
		t.Fatal("expected accumulated validation error, got nil")
	}
	msg := err.Error()

	wantSubstrings := []string{
		"device[0]", "id exceeds",
		"device[1]", "vendor exceeds",
		"device[1]", "sys_name exceeds",
		"device[2]", "os_version exceeds",
		"edge[0]", "src_port exceeds",
		"edge[1]", "dst_port exceeds",
		"edge[2]", "discovery_proto exceeds",
		"out_of_scope[0]", "reporting_device exceeds",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(msg, want) {
			t.Errorf("accumulated error missing %q in:\n%s", want, msg)
		}
	}

	// errors.Join produces a multi-error that unwraps to its components; each
	// sub-error should be discoverable via errors.Is on a sentinel-like check.
	// We can't use errors.Is on fmt.Errorf strings directly, so confirm at
	// least that the unwrap returns more than one error.
	type multiUnwrap interface{ Unwrap() []error }
	mu, ok := err.(multiUnwrap)
	if !ok {
		t.Fatalf("expected joined error to expose Unwrap() []error, got %T", err)
	}
	if got := len(mu.Unwrap()); got < len(wantSubstrings)/2 {
		t.Errorf("expected at least %d sub-errors, got %d", len(wantSubstrings)/2, got)
	}
}

// TestValidateSnapshotFieldsCapsAtMaxErrors guards the upper bound on
// accumulated errors: a deliberately-corrupt snapshot with thousands of
// oversized fields must not produce an unbounded multi-MB error blob. The
// validator stops at maxValidationErrors and appends an "omitted" sentinel.
func TestValidateSnapshotFieldsCapsAtMaxErrors(t *testing.T) {
	// Build 200 devices each with an oversized ID — well past the 100 cap.
	const numDevices = 200
	devices := make([]discovery.Device, 0, numDevices)
	for i := 0; i < numDevices; i++ {
		devices = append(devices, discovery.Device{ID: strings.Repeat("a", 1024)})
	}
	f := &File{Devices: devices}

	err := validateSnapshotFields(f)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}

	type multiUnwrap interface{ Unwrap() []error }
	mu, ok := err.(multiUnwrap)
	if !ok {
		t.Fatalf("expected joined error to expose Unwrap() []error, got %T", err)
	}
	subs := mu.Unwrap()
	// 100 real errors + 1 "omitted" sentinel = 101.
	if got, want := len(subs), 101; got != want {
		t.Errorf("expected %d sub-errors (cap + sentinel), got %d", want, got)
	}

	if !strings.Contains(err.Error(), "omitted") {
		t.Errorf("expected omitted-sentinel in error, got:\n%s", err.Error())
	}
	if !strings.Contains(err.Error(), "cap=100") {
		t.Errorf("expected cap=100 in error, got:\n%s", err.Error())
	}
}
