package fixture

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/jpeg"
	"os"
)

// AudioRate is the sample rate of fixture audio: 16 kHz mono, 16-bit.
const AudioRate = 16000

// Scene is a stretch of video showing one still drawing.
type Scene struct {
	MS     int // how long the scene lasts
	Shapes []Shape
}

// MOV writes a QuickTime movie: each scene is one Photo-JPEG frame held for
// its duration, and audio (16 kHz mono, may be empty) is a PCM soundtrack.
// AVFoundation decodes both natively, so no encoder is needed.
func MOV(path string, w, h int, scenes []Scene, audio []int16) error {
	if len(scenes) == 0 {
		return errors.New("mov needs at least one scene")
	}
	var frames [][]byte
	videoMS := 0
	for i, s := range scenes {
		if s.MS < 1 || s.MS > 3_600_000 {
			return fmt.Errorf("scene %d lasts %d ms", i, s.MS)
		}
		img, err := draw(w, h, s.Shapes)
		if err != nil {
			return fmt.Errorf("scene %d: %w", i, err)
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
			return err
		}
		frames = append(frames, buf.Bytes())
		videoMS += s.MS
	}
	if w > 0xffff || h > 0xffff {
		return fmt.Errorf("mov size %d×%d too large", w, h)
	}
	audioMS := len(audio) * 1000 / AudioRate

	// Layout: ftyp, then mdat holding the frames and the samples, then moov.
	ftyp := atom("ftyp", []byte("qt  "), u32(0x20050300), []byte("qt  "))
	mdatStart := len(ftyp) + 8
	var offsets []uint32
	var data bytes.Buffer
	for _, f := range frames {
		offsets = append(offsets, uint32(mdatStart+data.Len()))
		data.Write(f)
	}
	audioOffset := uint32(mdatStart + data.Len())
	for _, s := range audio {
		data.Write(binary.LittleEndian.AppendUint16(nil, uint16(s)))
	}
	if mdatStart+data.Len() > 1<<31 {
		return errors.New("mov too large for 32-bit offsets")
	}
	mdat := atom("mdat", data.Bytes())

	// Video track: one sample per scene, each held for the scene's duration.
	var stts, stsz, stco []byte
	stts = append(u32(0), u32(uint32(len(scenes)))...)
	stsz = append(u32(0), u32(0)...)
	stsz = append(stsz, u32(uint32(len(frames)))...)
	stco = append(u32(0), u32(uint32(len(frames)))...)
	for i, s := range scenes {
		stts = append(stts, u32(1)...)
		stts = append(stts, u32(uint32(s.MS))...)
		stsz = append(stsz, u32(uint32(len(frames[i])))...)
		stco = append(stco, u32(offsets[i])...)
	}
	sampleEntry := concat(
		make([]byte, 6), u16(1), // reserved, data reference index
		u16(0), u16(0), []byte{0, 0, 0, 0}, // version, revision, vendor
		u32(0), u32(512), // temporal, spatial quality
		u16(uint16(w)), u16(uint16(h)),
		u32(72<<16), u32(72<<16), u32(0), u16(1), // resolution, data size, frames per sample
		pascal32("Photo - JPEG"), u16(24), u16(0xffff), // depth, no color table
	)
	vstbl := atom("stbl",
		atom("stsd", u32(0), u32(1), atom("jpeg", sampleEntry)),
		atom("stts", stts),
		atom("stsc", u32(0), u32(1), u32(1), u32(1), u32(1)),
		atom("stsz", stsz),
		atom("stco", stco),
	)
	video := track(1, videoMS, w, h, 1000, uint32(videoMS), "vide",
		atom("vmhd", u32(1), u16(0x40), u16(0x8000), u16(0x8000), u16(0x8000)), vstbl)

	tracks := [][]byte{video}
	if len(audio) > 0 {
		// One chunk of 16-bit little-endian ('sowt') samples.
		soundEntry := concat(
			make([]byte, 6), u16(1),
			u16(0), u16(0), []byte{0, 0, 0, 0}, // version 0, revision, vendor
			u16(1), u16(16), u16(0), u16(0), // channels, bits, compression id, packet size
			u32(AudioRate<<16),
		)
		astbl := atom("stbl",
			atom("stsd", u32(0), u32(1), atom("sowt", soundEntry)),
			atom("stts", u32(0), u32(1), u32(uint32(len(audio))), u32(1)),
			atom("stsc", u32(0), u32(1), u32(1), u32(uint32(len(audio))), u32(1)),
			atom("stsz", u32(0), u32(1), u32(uint32(len(audio)))),
			atom("stco", u32(0), u32(1), u32(audioOffset)),
		)
		tracks = append(tracks, track(2, audioMS, 0, 0, AudioRate, uint32(len(audio)), "soun",
			atom("smhd", u32(0), u16(0), u16(0)), astbl))
	}
	mvhd := atom("mvhd", u32(0), u32(0), u32(0), u32(1000), u32(uint32(max(videoMS, audioMS))),
		u32(0x00010000), u16(0x0100), make([]byte, 10), matrix(), make([]byte, 24), u32(uint32(len(tracks)+1)))
	moov := atom("moov", append([][]byte{mvhd}, tracks...)...)
	return os.WriteFile(path, concat(ftyp, mdat, moov), 0o600)
}

// track builds a trak atom. durMS is in movie time (ms); mediaDur is in
// the media's own timescale.
func track(id uint32, durMS, w, h int, timescale, mediaDur uint32, handler string, mediaHeader, stbl []byte) []byte {
	volume := uint16(0)
	if handler == "soun" {
		volume = 0x0100
	}
	tkhd := atom("tkhd", u32(0x0000000f), u32(0), u32(0), u32(id), u32(0), u32(uint32(durMS)),
		make([]byte, 8), u16(0), u16(0), u16(volume), u16(0), matrix(), u32(uint32(w)<<16), u32(uint32(h)<<16))
	mdhd := atom("mdhd", u32(0), u32(0), u32(0), u32(timescale), u32(mediaDur), u16(0), u16(0))
	hdlr := atom("hdlr", u32(0), []byte("mhlr"), []byte(handler), make([]byte, 12), []byte{0})
	dinf := atom("dinf", atom("dref", u32(0), u32(1), atom("alis", u32(1))))
	dhlr := atom("hdlr", u32(0), []byte("dhlr"), []byte("alis"), make([]byte, 12), []byte{0})
	minf := atom("minf", mediaHeader, dhlr, dinf, stbl)
	return atom("trak", tkhd, atom("mdia", mdhd, hdlr, minf))
}

func atom(typ string, parts ...[]byte) []byte {
	body := concat(parts...)
	return concat(u32(uint32(8+len(body))), []byte(typ), body)
}

func concat(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// matrix is the identity transform of a QuickTime header.
func matrix() []byte {
	return concat(u32(0x00010000), u32(0), u32(0), u32(0), u32(0x00010000), u32(0), u32(0), u32(0), u32(0x40000000))
}

func pascal32(s string) []byte {
	b := make([]byte, 32)
	b[0] = byte(copy(b[1:], s))
	return b
}

// ReadWAV reads a 16 kHz mono 16-bit PCM WAV file, such as
// `say --file-format=WAVE --data-format=LEI16@16000` writes.
func ReadWAV(path string) ([]int16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF WAVE file")
	}
	le := binary.LittleEndian
	var pcm []byte
	format := false
	for off := 12; off+8 <= len(data); {
		n := int(le.Uint32(data[off+4:]))
		body := off + 8
		if n < 0 || n > len(data)-body {
			return nil, errors.New("truncated WAV chunk")
		}
		switch string(data[off : off+4]) {
		case "fmt ":
			if n < 16 || le.Uint16(data[body:]) != 1 || le.Uint16(data[body+2:]) != 1 ||
				le.Uint32(data[body+4:]) != AudioRate || le.Uint16(data[body+14:]) != 16 {
				return nil, errors.New("WAV is not 16 kHz mono 16-bit PCM")
			}
			format = true
		case "data":
			pcm = data[body : body+n]
		}
		off = body + n + n%2
	}
	if !format || pcm == nil || len(pcm)%2 != 0 {
		return nil, errors.New("WAV lacks format or PCM data")
	}
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(le.Uint16(pcm[2*i:]))
	}
	return out, nil
}
