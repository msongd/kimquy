// Package catalog records every file semd knows about: where it is, what it
// looked like when last indexed, and which vector generation is live for it.
//
// Durability: every mutation is appended to a write-ahead log and fsynced
// before it becomes visible. When the log grows past a threshold the full
// state is written to a snapshot (temp file + rename) and the log is reset.
// All ops are idempotent full-state writes, so replaying a log over a newer
// snapshot is harmless. On open, a torn or corrupt log tail is truncated and
// logged; a corrupt snapshot is a hard error.
package catalog

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/msongd/kimquy/internal/fsutil"
	"github.com/msongd/kimquy/internal/kind"
)

const (
	walFile  = "catalog.wal"
	snapFile = "catalog.snap"
)

var snapMagic = []byte("SSCS\x01\x00\x00\x00")

// ErrCorrupt reports catalog damage that recovery cannot repair.
var ErrCorrupt = errors.New("catalog: corrupt")

// Status is a file's indexing state.
type Status uint8

const (
	Pending Status = iota + 1
	Indexed
	Failed
)

func (s Status) String() string {
	switch s {
	case Pending:
		return "pending"
	case Indexed:
		return "indexed"
	case Failed:
		return "failed"
	}
	return fmt.Sprintf("status(%d)", uint8(s))
}

// File is one catalog record.
type File struct {
	ID      uint64
	Path    string // absolute, clean
	Kind    kind.Kind
	Size    int64
	MTime   int64 // unix nanoseconds
	Hash    [sha256.Size]byte
	Status  Status
	Err     string // why the last attempt failed (Status == Failed)
	Gen     uint64 // live vector generation in the store, 0 if none
	NVec    uint32 // vectors in Gen, cross-checked against the store at startup
	Updated int64  // unix nanoseconds of the last status change
}

func (f *File) validate() error {
	switch {
	case f.ID == 0:
		return errors.New("id 0")
	case f.Path == "" || len(f.Path) > maxPath || !filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path:
		return fmt.Errorf("path %q must be absolute and clean", f.Path)
	case bytes.IndexByte([]byte(f.Path), 0) >= 0:
		return fmt.Errorf("path %q contains NUL", f.Path)
	case !f.Kind.Valid():
		return fmt.Errorf("invalid kind %d", f.Kind)
	case f.Status < Pending || f.Status > Failed:
		return fmt.Errorf("invalid status %d", f.Status)
	case len(f.Err) > maxErr:
		return errors.New("error text too long")
	case f.Size < 0:
		return fmt.Errorf("negative size %d", f.Size)
	}
	return nil
}

// Options tune the catalog.
type Options struct {
	SnapshotBytes int64 // WAL size that triggers a snapshot (default 8 MiB)
	Logger        *slog.Logger
}

// Catalog is safe for concurrent use.
type Catalog struct {
	dir  string
	opt  Options
	log  *slog.Logger
	lock *os.File

	mu      sync.RWMutex
	byID    map[uint64]*File
	byPath  map[string]uint64
	meta    map[string]string
	nextID  uint64
	wal     *os.File
	walSize int64
	broken  error
	closed  bool
}

// Open loads or creates the catalog in dir.
func Open(dir string, opt Options) (*Catalog, error) {
	if opt.SnapshotBytes <= 0 {
		opt.SnapshotBytes = 8 << 20
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	lock, err := fsutil.LockDir(dir)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	c := &Catalog{dir: dir, opt: opt, log: opt.Logger.With("component", "catalog"), lock: lock,
		byID: map[uint64]*File{}, byPath: map[string]uint64{}, meta: map[string]string{}, nextID: 1}
	if err := c.load(); err != nil {
		lock.Close()
		return nil, err
	}
	return c, nil
}

func (c *Catalog) load() error {
	snap, err := os.ReadFile(filepath.Join(c.dir, snapFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("catalog: %w", err)
	default:
		if err := c.loadSnapshot(snap); err != nil {
			return err
		}
	}

	walPath := filepath.Join(c.dir, walFile)
	wal, err := os.ReadFile(walPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("catalog: %w", err)
	}
	off := 0
	for {
		p, n, err := readFrame(wal[off:])
		if err == io.EOF {
			break
		}
		if err == nil {
			err = c.apply(p)
		}
		if err != nil {
			c.log.Warn("truncating catalog WAL at damaged record", "offset", off, "dropped_bytes", len(wal)-off, "err", err)
			if err := fsutil.TruncateSync(walPath, int64(off)); err != nil {
				return fmt.Errorf("catalog: truncate wal: %w", err)
			}
			break
		}
		off += n
	}
	c.wal, err = os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	c.walSize = int64(off)
	return fsutil.SyncDir(c.dir)
}

func (c *Catalog) loadSnapshot(b []byte) error {
	if !bytes.HasPrefix(b, snapMagic) || len(b) < len(snapMagic)+8 {
		return fmt.Errorf("%w: snapshot header", ErrCorrupt)
	}
	b = b[len(snapMagic):]
	count := le.Uint64(b)
	b = b[8:]
	for i := range count {
		p, n, err := readFrame(b)
		if err != nil {
			return fmt.Errorf("%w: snapshot record %d of %d: %v", ErrCorrupt, i, count, err)
		}
		if err := c.apply(p); err != nil {
			return fmt.Errorf("%w: snapshot record %d: %v", ErrCorrupt, i, err)
		}
		b = b[n:]
	}
	if len(b) != 0 {
		return fmt.Errorf("%w: %d trailing snapshot bytes", ErrCorrupt, len(b))
	}
	return nil
}

// apply mutates in-memory state from one payload.
func (c *Catalog) apply(p []byte) error {
	if len(p) == 0 {
		return errDecoded
	}
	switch p[0] {
	case opPut:
		f, err := decodePut(p[1:])
		if err != nil {
			return err
		}
		c.setFile(&f)
	case opDelete:
		d := decoder{b: p[1:]}
		id := d.uvarint()
		if err := d.done(); err != nil {
			return err
		}
		c.removeFile(id)
	case opMeta:
		d := decoder{b: p[1:]}
		k, v := d.string(256), d.string(maxErr)
		if err := d.done(); err != nil {
			return err
		}
		c.meta[k] = v
	default:
		return fmt.Errorf("%w: unknown op %d", errDecoded, p[0])
	}
	return nil
}

func (c *Catalog) setFile(f *File) {
	if old, ok := c.byID[f.ID]; ok && old.Path != f.Path {
		delete(c.byPath, old.Path)
	}
	if other, ok := c.byPath[f.Path]; ok && other != f.ID {
		// A path moved to a new ID; the newer record wins.
		delete(c.byID, other)
	}
	c.byID[f.ID] = f
	c.byPath[f.Path] = f.ID
	c.nextID = max(c.nextID, f.ID+1)
}

func (c *Catalog) removeFile(id uint64) {
	if f, ok := c.byID[id]; ok {
		delete(c.byPath, f.Path)
		delete(c.byID, id)
	}
}

// writeLocked appends payloads as one fsynced WAL write. Caller holds mu.
func (c *Catalog) writeLocked(payloads ...[]byte) error {
	if c.closed {
		return errors.New("catalog: closed")
	}
	if c.broken != nil {
		return fmt.Errorf("catalog: disabled after write failure: %w", c.broken)
	}
	var buf []byte
	for _, p := range payloads {
		buf = appendFrame(buf, p)
	}
	if _, err := c.wal.Write(buf); err != nil {
		c.broken = err
		return fmt.Errorf("catalog: wal write: %w", err)
	}
	if err := c.wal.Sync(); err != nil {
		c.broken = err
		return fmt.Errorf("catalog: wal sync: %w", err)
	}
	c.walSize += int64(len(buf))
	return nil
}

// Put inserts or replaces files in one durable write. A zero ID is assigned
// (and written back into fs). Inserting a path that already exists under a
// different ID is an error.
func (c *Catalog) Put(fs ...*File) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.nextID
	seen := map[string]bool{}
	payloads := make([][]byte, 0, len(fs))
	ids := make([]uint64, len(fs))
	for i, f := range fs {
		id := f.ID
		if id == 0 {
			id = next
			next++
		}
		cp := *f
		cp.ID = id
		if err := cp.validate(); err != nil {
			return fmt.Errorf("catalog: put: %w", err)
		}
		if other, ok := c.byPath[cp.Path]; ok && other != id {
			return fmt.Errorf("catalog: put: path %q already has id %d", cp.Path, other)
		}
		if seen[cp.Path] {
			return fmt.Errorf("catalog: put: path %q twice in one batch", cp.Path)
		}
		seen[cp.Path] = true
		payloads = append(payloads, encodePut(&cp))
		ids[i] = id
	}
	if err := c.writeLocked(payloads...); err != nil {
		return err
	}
	for i, f := range fs {
		f.ID = ids[i]
		cp := *f
		c.setFile(&cp)
	}
	return c.maybeSnapshotLocked()
}

// NewID reserves an ID for a file about to be inserted, so its vectors can be
// written before the catalog record. The reservation is not durable: after a
// crash the ID may be handed out again, which is safe because the store only
// shows records whose generation the catalog committed.
func (c *Catalog) NewID() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID
	c.nextID++
	return id
}

// Delete removes files by ID; unknown IDs are ignored.
func (c *Catalog) Delete(ids ...uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var payloads [][]byte
	for _, id := range ids {
		if _, ok := c.byID[id]; ok {
			payloads = append(payloads, encodeDelete(id))
		}
	}
	if len(payloads) == 0 {
		return nil
	}
	if err := c.writeLocked(payloads...); err != nil {
		return err
	}
	for _, id := range ids {
		c.removeFile(id)
	}
	return c.maybeSnapshotLocked()
}

// SetMeta durably stores a small key/value pair (e.g. the FSEvents event ID).
func (c *Catalog) SetMeta(key, val string) error {
	if key == "" || len(key) > 256 || len(val) > maxErr {
		return fmt.Errorf("catalog: meta %q: bad size", key)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.writeLocked(encodeMeta(key, val)); err != nil {
		return err
	}
	c.meta[key] = val
	return c.maybeSnapshotLocked()
}

// Meta returns a value stored with SetMeta.
func (c *Catalog) Meta(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.meta[key]
	return v, ok
}

// Get returns a copy of the file with id.
func (c *Catalog) Get(id uint64) (File, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	f, ok := c.byID[id]
	if !ok {
		return File{}, false
	}
	return *f, true
}

// Lookup returns a copy of the file at path.
func (c *Catalog) Lookup(path string) (File, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.byPath[path]
	if !ok {
		return File{}, false
	}
	return *c.byID[id], true
}

// Each calls fn with a copy of every file, in ID order, until fn returns false.
// fn runs without the catalog lock held, so it may call back into the catalog.
func (c *Catalog) Each(fn func(File) bool) {
	c.mu.RLock()
	files := make([]File, 0, len(c.byID))
	for _, id := range slices.Sorted(maps.Keys(c.byID)) {
		files = append(files, *c.byID[id])
	}
	c.mu.RUnlock()
	for _, f := range files {
		if !fn(f) {
			return
		}
	}
}

// LiveGens maps fileID to live generation, for opening the vector store.
func (c *Catalog) LiveGens() map[uint64]uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m := make(map[uint64]uint64, len(c.byID))
	for id, f := range c.byID {
		if f.Gen != 0 {
			m[id] = f.Gen
		}
	}
	return m
}

// Counts returns how many files are in each status.
func (c *Catalog) Counts() map[Status]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m := map[Status]int{}
	for _, f := range c.byID {
		m[f.Status]++
	}
	return m
}

func (c *Catalog) maybeSnapshotLocked() error {
	if c.walSize < c.opt.SnapshotBytes {
		return nil
	}
	return c.snapshotLocked()
}

// Snapshot writes the full state and resets the WAL.
func (c *Catalog) Snapshot() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *Catalog) snapshotLocked() error {
	if c.closed {
		return errors.New("catalog: closed")
	}
	buf := slices.Clone(snapMagic)
	buf = le.AppendUint64(buf, uint64(len(c.byID)+len(c.meta)))
	for _, k := range slices.Sorted(maps.Keys(c.meta)) {
		buf = appendFrame(buf, encodeMeta(k, c.meta[k]))
	}
	for _, id := range slices.Sorted(maps.Keys(c.byID)) {
		buf = appendFrame(buf, encodePut(c.byID[id]))
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(c.dir, snapFile), buf); err != nil {
		return fmt.Errorf("catalog: snapshot: %w", err)
	}
	// The snapshot now covers everything in the WAL; a crash before the
	// truncate below just replays idempotent records.
	if err := c.wal.Truncate(0); err != nil {
		c.broken = err
		return fmt.Errorf("catalog: reset wal: %w", err)
	}
	if err := c.wal.Sync(); err != nil {
		c.broken = err
		return fmt.Errorf("catalog: reset wal: %w", err)
	}
	c.walSize = 0
	return nil
}

// Close syncs and releases the catalog.
func (c *Catalog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return errors.Join(c.wal.Sync(), c.wal.Close(), c.lock.Close())
}
