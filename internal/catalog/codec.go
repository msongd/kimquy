package catalog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/msongd/kimquy/internal/kind"
)

// Frame layout shared by the WAL and the snapshot:
//
//	len u32 | crc32 u32 | payload[len]
//
// payload[0] is the op code.
const (
	opPut    byte = 1
	opDelete byte = 2
	opMeta   byte = 3

	frameHeader = 8
	maxFrame    = 1 << 20 // far larger than any legitimate record
	maxPath     = 4096
	maxErr      = 4096
)

var (
	le         = binary.LittleEndian
	errTorn    = errors.New("torn or corrupt frame")
	errDecoded = errors.New("malformed record")
)

func appendFrame(buf, payload []byte) []byte {
	buf = le.AppendUint32(buf, uint32(len(payload)))
	buf = le.AppendUint32(buf, crc32.ChecksumIEEE(payload))
	return append(buf, payload...)
}

// readFrame returns the next payload in b and the bytes consumed.
// io.EOF means b is exhausted exactly at a frame boundary.
func readFrame(b []byte) (payload []byte, n int, err error) {
	if len(b) == 0 {
		return nil, 0, io.EOF
	}
	if len(b) < frameHeader {
		return nil, 0, errTorn
	}
	size := le.Uint32(b)
	if size == 0 || size > maxFrame || int(size) > len(b)-frameHeader {
		return nil, 0, errTorn
	}
	payload = b[frameHeader : frameHeader+int(size)]
	if crc32.ChecksumIEEE(payload) != le.Uint32(b[4:]) {
		return nil, 0, errTorn
	}
	return payload, frameHeader + int(size), nil
}

func appendString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func encodePut(f *File) []byte {
	b := []byte{opPut}
	b = binary.AppendUvarint(b, f.ID)
	b = appendString(b, f.Path)
	b = append(b, byte(f.Kind), byte(f.Status))
	b = binary.AppendVarint(b, f.Size)
	b = binary.AppendVarint(b, f.MTime)
	b = append(b, f.Hash[:]...)
	b = appendString(b, f.Err)
	b = binary.AppendUvarint(b, f.Gen)
	b = binary.AppendUvarint(b, uint64(f.NVec))
	b = binary.AppendVarint(b, f.Updated)
	return b
}

func encodeDelete(id uint64) []byte {
	return binary.AppendUvarint([]byte{opDelete}, id)
}

func encodeMeta(key, val string) []byte {
	return appendString(appendString([]byte{opMeta}, key), val)
}

// decoder reads fields with bounds checks; the first failure sticks.
type decoder struct {
	b   []byte
	err error
}

func (d *decoder) fail(what string) {
	if d.err == nil {
		d.err = fmt.Errorf("%w: %s", errDecoded, what)
	}
}

func (d *decoder) byte() byte {
	if d.err != nil || len(d.b) < 1 {
		d.fail("byte")
		return 0
	}
	c := d.b[0]
	d.b = d.b[1:]
	return c
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.fail("uvarint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) varint() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.fail("varint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) bytes(n int) []byte {
	if d.err != nil || n < 0 || len(d.b) < n {
		d.fail("bytes")
		return nil
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v
}

func (d *decoder) string(limit int) string {
	n := d.uvarint()
	if n > uint64(limit) {
		d.fail("string too long")
		return ""
	}
	return string(d.bytes(int(n)))
}

func (d *decoder) done() error {
	if d.err == nil && len(d.b) != 0 {
		d.fail("trailing bytes")
	}
	return d.err
}

func decodePut(p []byte) (File, error) {
	d := decoder{b: p}
	var f File
	f.ID = d.uvarint()
	f.Path = d.string(maxPath)
	f.Kind = kind.Kind(d.byte())
	f.Status = Status(d.byte())
	f.Size = d.varint()
	f.MTime = d.varint()
	copy(f.Hash[:], d.bytes(32))
	f.Err = d.string(maxErr)
	f.Gen = d.uvarint()
	nvec := d.uvarint()
	f.Updated = d.varint()
	if err := d.done(); err != nil {
		return File{}, err
	}
	if nvec > 1<<32-1 {
		return File{}, fmt.Errorf("%w: nvec %d", errDecoded, nvec)
	}
	f.NVec = uint32(nvec)
	if err := f.validate(); err != nil {
		return File{}, fmt.Errorf("%w: %v", errDecoded, err)
	}
	return f, nil
}
