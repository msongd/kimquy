package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"

	"github.com/msongd/kimquy/internal/fsutil"
	"github.com/msongd/kimquy/internal/kind"
)

// On-disk segment layout. Each segment is a directory holding two files that
// always contain the same number of records:
//
//	vectors.bin  header | {scale f32, q [dim]int8, crc32}...
//	meta.bin     header | {fileID u64, gen u64, kind u8, lockind u8, page u16, start i64, end i64, crc32}...
//
// header = magic [4]byte | version u32 | dim u32 | crc32 (16 bytes). All integers little endian.
const (
	segVersion  = 1
	headerSize  = 16
	metaRecSize = 40
	vecFile     = "vectors.bin"
	metaFile    = "meta.bin"
)

var (
	vecMagic  = [4]byte{'S', 'S', 'V', 'V'}
	metaMagic = [4]byte{'S', 'S', 'V', 'M'}
	le        = binary.LittleEndian
)

// ErrCorrupt reports on-disk damage that cannot be repaired automatically.
var ErrCorrupt = errors.New("store: corrupt data")

func vecRecSize(dim int) int { return dim + 8 }

type meta struct {
	fileID uint64
	gen    uint64
	kind   kind.Kind
	loc    kind.Locator
}

type segment struct {
	id     uint64
	dir    string
	dim    int
	vecs   []int8 // n*dim
	scales []float32
	metas  []meta
	dead   int // records not belonging to a live generation
	sealed bool
	vf, mf *os.File // open only while the segment is active
}

func (g *segment) len() int { return len(g.metas) }

func segDirName(id uint64) string { return fmt.Sprintf("seg-%08d", id) }

func encodeHeader(magic [4]byte, dim int) []byte {
	b := make([]byte, headerSize)
	copy(b, magic[:])
	le.PutUint32(b[4:], segVersion)
	le.PutUint32(b[8:], uint32(dim))
	le.PutUint32(b[12:], crc32.ChecksumIEEE(b[:12]))
	return b
}

func checkHeader(b []byte, magic [4]byte, dim int) error {
	if len(b) < headerSize {
		return fmt.Errorf("%w: short header", ErrCorrupt)
	}
	if !bytes.Equal(b[:4], magic[:]) {
		return fmt.Errorf("%w: bad magic %q", ErrCorrupt, b[:4])
	}
	if crc32.ChecksumIEEE(b[:12]) != le.Uint32(b[12:]) {
		return fmt.Errorf("%w: header checksum mismatch", ErrCorrupt)
	}
	if v := le.Uint32(b[4:]); v != segVersion {
		return fmt.Errorf("%w: unsupported segment version %d", ErrCorrupt, v)
	}
	if d := int(le.Uint32(b[8:])); d != dim {
		return fmt.Errorf("%w: segment dim %d != store dim %d", ErrCorrupt, d, dim)
	}
	return nil
}

func appendVecRec(buf []byte, scale float32, q []int8) []byte {
	start := len(buf)
	buf = le.AppendUint32(buf, math.Float32bits(scale))
	for _, x := range q {
		buf = append(buf, byte(x))
	}
	return le.AppendUint32(buf, crc32.ChecksumIEEE(buf[start:]))
}

func appendMetaRec(buf []byte, m meta) []byte {
	start := len(buf)
	buf = le.AppendUint64(buf, m.fileID)
	buf = le.AppendUint64(buf, m.gen)
	buf = append(buf, byte(m.kind), byte(m.loc.Kind))
	buf = le.AppendUint16(buf, m.loc.Page)
	buf = le.AppendUint64(buf, uint64(m.loc.Start))
	buf = le.AppendUint64(buf, uint64(m.loc.End))
	return le.AppendUint32(buf, crc32.ChecksumIEEE(buf[start:]))
}

// createSegment makes an empty segment directory with both files and fsyncs it.
func createSegment(root string, id uint64, dim int) (*segment, error) {
	dir := filepath.Join(root, segDirName(id))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create segment: %w", err)
	}
	g := &segment{id: id, dir: dir, dim: dim}
	for _, f := range []struct {
		name  string
		magic [4]byte
	}{{vecFile, vecMagic}, {metaFile, metaMagic}} {
		if err := fsutil.WriteFileSync(filepath.Join(dir, f.name), encodeHeader(f.magic, dim)); err != nil {
			return nil, err
		}
	}
	if err := fsutil.SyncDir(dir); err != nil {
		return nil, err
	}
	if err := fsutil.SyncDir(root); err != nil {
		return nil, err
	}
	return g, nil
}

// loadSegment reads a segment fully into memory. A sealed segment must be
// intact. An active segment may end in a torn write; its files are truncated
// back to the longest valid common prefix and the number of dropped records is
// returned so the caller can log it.
func loadSegment(root string, id uint64, dim int, active bool) (g *segment, dropped int, err error) {
	dir := filepath.Join(root, segDirName(id))
	vb, err := os.ReadFile(filepath.Join(dir, vecFile))
	if err != nil {
		return nil, 0, fmt.Errorf("load segment %d: %w", id, err)
	}
	mb, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return nil, 0, fmt.Errorf("load segment %d: %w", id, err)
	}
	if err := checkHeader(vb, vecMagic, dim); err != nil {
		return nil, 0, fmt.Errorf("segment %d %s: %w", id, vecFile, err)
	}
	if err := checkHeader(mb, metaMagic, dim); err != nil {
		return nil, 0, fmt.Errorf("segment %d %s: %w", id, metaFile, err)
	}
	vrs := vecRecSize(dim)
	vbody, mbody := vb[headerSize:], mb[headerSize:]
	nv := validPrefix(vbody, vrs)
	nm := validPrefix(mbody, metaRecSize)
	n := min(nv, nm)
	clean := len(vbody) == n*vrs && len(mbody) == n*metaRecSize
	if !clean && !active {
		return nil, 0, fmt.Errorf("%w: sealed segment %d damaged (vectors %d valid of %d bytes, meta %d valid of %d bytes)",
			ErrCorrupt, id, nv, len(vbody), nm, len(mbody))
	}

	g = &segment{
		id: id, dir: dir, dim: dim, sealed: !active,
		vecs:   make([]int8, n*dim),
		scales: make([]float32, n),
		metas:  make([]meta, n),
	}
	for i := range n {
		r := vbody[i*vrs:]
		g.scales[i] = math.Float32frombits(le.Uint32(r))
		for j := range dim {
			g.vecs[i*dim+j] = int8(r[4+j])
		}
		m := mbody[i*metaRecSize:]
		g.metas[i] = meta{
			fileID: le.Uint64(m),
			gen:    le.Uint64(m[8:]),
			kind:   kind.Kind(m[16]),
			loc: kind.Locator{Kind: kind.LocKind(m[17]), Page: le.Uint16(m[18:]),
				Start: int64(le.Uint64(m[20:])), End: int64(le.Uint64(m[28:]))},
		}
	}
	if !active {
		return g, 0, nil
	}

	if !clean {
		dropped = max(len(vbody)/vrs, len(mbody)/metaRecSize) - n
		if err := fsutil.TruncateSync(filepath.Join(dir, vecFile), int64(headerSize+n*vrs)); err != nil {
			return nil, 0, err
		}
		if err := fsutil.TruncateSync(filepath.Join(dir, metaFile), int64(headerSize+n*metaRecSize)); err != nil {
			return nil, 0, err
		}
	}
	if err := g.openForAppend(); err != nil {
		return nil, 0, err
	}
	return g, dropped, nil
}

// validPrefix returns how many leading fixed-size records carry a valid crc32.
func validPrefix(b []byte, size int) int {
	n := 0
	for off := 0; off+size <= len(b); off += size {
		r := b[off : off+size]
		if crc32.ChecksumIEEE(r[:size-4]) != le.Uint32(r[size-4:]) {
			break
		}
		n++
	}
	return n
}

func (g *segment) openForAppend() error {
	vf, err := os.OpenFile(filepath.Join(g.dir, vecFile), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	mf, err := os.OpenFile(filepath.Join(g.dir, metaFile), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		vf.Close()
		return err
	}
	g.vf, g.mf = vf, mf
	return nil
}

// write appends encoded records to disk and fsyncs both files. The in-memory
// arrays are only extended after the data is durable.
func (g *segment) write(scales []float32, vecs []int8, metas []meta) error {
	if g.sealed || g.vf == nil {
		return fmt.Errorf("store: write to sealed segment %d", g.id)
	}
	vbuf := make([]byte, 0, len(metas)*vecRecSize(g.dim))
	mbuf := make([]byte, 0, len(metas)*metaRecSize)
	for i, m := range metas {
		vbuf = appendVecRec(vbuf, scales[i], vecs[i*g.dim:(i+1)*g.dim])
		mbuf = appendMetaRec(mbuf, m)
	}
	if _, err := g.vf.Write(vbuf); err != nil {
		return fmt.Errorf("segment %d write vectors: %w", g.id, err)
	}
	if _, err := g.mf.Write(mbuf); err != nil {
		return fmt.Errorf("segment %d write meta: %w", g.id, err)
	}
	if err := g.vf.Sync(); err != nil {
		return fmt.Errorf("segment %d sync vectors: %w", g.id, err)
	}
	if err := g.mf.Sync(); err != nil {
		return fmt.Errorf("segment %d sync meta: %w", g.id, err)
	}
	g.scales = append(g.scales, scales...)
	g.vecs = append(g.vecs, vecs...)
	g.metas = append(g.metas, metas...)
	return nil
}

func (g *segment) seal() error {
	if g.sealed {
		return nil
	}
	var errs []error
	for _, f := range []*os.File{g.vf, g.mf} {
		if f == nil {
			continue
		}
		errs = append(errs, f.Sync(), f.Close())
	}
	g.vf, g.mf = nil, nil
	g.sealed = true
	return errors.Join(errs...)
}

// writeSealedSegment writes a complete segment in one go (used by compaction).
func writeSealedSegment(root string, id uint64, dim int, scales []float32, vecs []int8, metas []meta) (*segment, error) {
	g, err := createSegment(root, id, dim)
	if err != nil {
		return nil, err
	}
	if err := g.openForAppend(); err != nil {
		return nil, err
	}
	if err := g.write(scales, vecs, metas); err != nil {
		g.seal()
		return nil, err
	}
	return g, g.seal()
}
