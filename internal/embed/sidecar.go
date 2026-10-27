package embed

// Sidecar protocol (semd ⇄ semd-embed), NDJSON over the child's stdin/stdout.
// Stderr is free-form logging. One JSON object per line, UTF-8, at most
// MaxLine bytes.
//
// Requests carry a client-chosen positive "id"; every response echoes it.
// Responses may arrive in any order (queries jump ahead of indexing).
//
//	→ {"id":1,"op":"hello","protocol":1}
//	← {"id":1,"protocol":1,"model":"embeddinggemma-2@914f7f8","precision":"q8",
//	   "dims":256,"modalities":["text","image","audio"],"max_batch":32,"max_tokens":8192}
//
//	→ {"id":2,"op":"embed_text","priority":"index"|"query","texts":["…",…]}
//	← {"id":2,"vectors":["<base64 little-endian float32 × dims>",…]}
//
//	→ {"id":3,"op":"embed_image","priority":"index","path":"/abs/photo.heic"}
//	← {"id":3,"vectors":["…"]}
//
//	→ {"id":4,"op":"extract_pdf","priority":"index","path":"/abs/a.pdf","from":1,"count":8,"render":8}
//	← {"id":4,"page_count":12,"pages":[{"page":1,"text":"…"},{"page":2,"text":"","vector":"…"},
//	   {"page":3,"text":" ","unrendered":true},…]}
//
//	→ {"id":5,"op":"probe_media","priority":"index","path":"/abs/talk.mp4"}
//	← {"id":5,"duration_ms":95000,"audio":true,"video":true}
//
//	→ {"id":6,"op":"embed_audio","priority":"index","path":"/abs/talk.mp4","start_ms":0,"end_ms":30000}
//	← {"id":6,"vectors":["…"]}  or  {"id":6,"vectors":[],"silent":true}
//
//	→ {"id":7,"op":"embed_frames","priority":"index","path":"/abs/talk.mp4","times_ms":[2500,7500]}
//	← {"id":7,"vectors":["…","…"]}
//
// embed_image and extract_pdf are offered when hello lists the "image" and
// "pdf" modalities. Images are decoded with EXIF orientation applied and
// transparency flattened onto white. extract_pdf returns pages
// [from, from+count) clipped to the document; a page with fewer than
// PDFRenderMinChars non-space characters is rendered and embedded as an
// image while the "render" budget lasts, and marked unrendered after.
// Page text over MaxPDFPageText bytes, encrypted or unreadable files are
// "invalid".
//
// probe_media is offered with the "audio" or "video" modality, embed_audio
// with "audio", embed_frames with "video". embed_audio mixes all audio
// tracks down to 16 kHz mono; a window that is quiet, or past the end of
// the audio, is "silent" and has no vector. embed_frames grabs the frames
// nearest each time (display-oriented, within a second). DRM-protected
// or unreadable files are "invalid".
//
// Failures: {"id":N,"error":{"code":"invalid"|"unavailable"|"internal","message":"…"}}
//   - invalid: this input cannot be embedded (e.g. too many tokens); the
//     file is recorded as failed.
//   - unavailable: transient (thermal, memory pressure); retried later.
//   - internal or anything else: a sidecar bug; semd stops loudly.
//
// Texts arrive fully formatted (EmbeddingGemma prompts applied by semd);
// the sidecar must not add prefixes. Vectors must be L2-normalized and
// finite. The sidecar exits on stdin EOF, and may exit with status 0 when
// idle; semd restarts it on demand and verifies that hello is unchanged.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/msongd/kimquy/internal/kind"
)

var errCallTimeout = errors.New("sidecar call timed out")

const (
	ProtocolVersion = 1
	// MaxLine bounds one protocol line; a 64-text batch of 768-d vectors is ~270 KiB.
	MaxLine = 16 << 20
)

// SidecarConfig describes how to run semd-embed.
type SidecarConfig struct {
	Path         string
	Args         []string
	Env          []string // extra KEY=VALUE entries appended to the environment
	StartTimeout time.Duration
	CallTimeout  time.Duration // a call slower than this means a hung sidecar
	Logger       *slog.Logger
}

// Sidecar is an Embedder backed by a supervised semd-embed process.
type Sidecar struct {
	cfg  SidecarConfig
	log  *slog.Logger
	info Info
	// hello is the first handshake; every restart must reproduce it exactly.
	hello helloReply

	// indexSlot admits one index call at a time, so a query waits for at
	// most one index batch inside the sidecar.
	indexSlot chan struct{}

	mu       sync.Mutex
	p        *proc
	closed   bool
	nextID   uint64
	failures int       // consecutive start failures or crashes
	retryAt  time.Time // no restart before this
}

type proc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	wmu     sync.Mutex
	mu      sync.Mutex
	pending map[uint64]chan reply
	done    chan struct{} // closed when the reader exits
	err     error         // why it ended; set before done closes
}

type request struct {
	ID       uint64   `json:"id"`
	Op       string   `json:"op"`
	Protocol int      `json:"protocol,omitempty"`
	Priority string   `json:"priority,omitempty"`
	Texts    []string `json:"texts,omitempty"`
	Path     string   `json:"path,omitempty"`
	From     int      `json:"from,omitempty"`
	Count    int      `json:"count,omitempty"`
	Render   int      `json:"render,omitempty"`
	StartMS  *int64   `json:"start_ms,omitempty"`
	EndMS    *int64   `json:"end_ms,omitempty"`
	TimesMS  []int64  `json:"times_ms,omitempty"`
}

type helloReply struct {
	Protocol   int      `json:"protocol"`
	Model      string   `json:"model"`
	Precision  string   `json:"precision"`
	Dims       int      `json:"dims"`
	Modalities []string `json:"modalities"`
	MaxBatch   int      `json:"max_batch"`
	MaxTokens  int      `json:"max_tokens"`
}

type replyError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type reply struct {
	ID        uint64          `json:"id"`
	Error     *replyError     `json:"error"`
	Vectors   []string        `json:"vectors"`
	PageCount int             `json:"page_count"`
	Pages     []pageReply     `json:"pages"`
	Duration  int64           `json:"duration_ms"`
	Audio     bool            `json:"audio"`
	Video     bool            `json:"video"`
	Silent    bool            `json:"silent"`
	raw       json.RawMessage // the full line, for op-specific decoding
}

type pageReply struct {
	Page       int    `json:"page"`
	Text       string `json:"text"`
	Vector     string `json:"vector"`
	Unrendered bool   `json:"unrendered"`
}

// StartSidecar launches semd-embed and performs the handshake. It fails if the
// sidecar cannot start or announces something semd cannot use.
func StartSidecar(ctx context.Context, cfg SidecarConfig) (*Sidecar, error) {
	if cfg.Path == "" {
		return nil, errors.New("sidecar: no executable path")
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 2 * time.Minute
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 2 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Sidecar{cfg: cfg, log: cfg.Logger.With("component", "sidecar"), indexSlot: make(chan struct{}, 1)}
	p, h, err := s.spawn(ctx)
	if err != nil {
		return nil, err
	}
	info, err := h.info()
	if err != nil {
		p.kill()
		return nil, fmt.Errorf("sidecar: %w", err)
	}
	s.p, s.hello, s.info = p, h, info
	s.log.Info("sidecar ready", "model", h.Model, "precision", h.Precision, "dims", h.Dims, "modalities", h.Modalities)
	return s, nil
}

func (h helloReply) info() (Info, error) {
	switch {
	case h.Protocol != ProtocolVersion:
		return Info{}, fmt.Errorf("protocol %d, want %d", h.Protocol, ProtocolVersion)
	case h.Model == "" || h.Precision == "":
		return Info{}, errors.New("hello lacks model or precision")
	case h.Dims < 128 || h.Dims > 8192:
		return Info{}, fmt.Errorf("hello: dims %d out of range", h.Dims)
	case h.MaxBatch < 1 || h.MaxBatch > 1024:
		return Info{}, fmt.Errorf("hello: max_batch %d out of range", h.MaxBatch)
	case !slices.Contains(h.Modalities, "text"):
		return Info{}, errors.New("hello: sidecar cannot embed text")
	case slices.Contains(h.Modalities, "pdf") && !slices.Contains(h.Modalities, "image"):
		return Info{}, errors.New("hello: pdf needs the image modality to render pages")
	case slices.Contains(h.Modalities, "video") && !slices.Contains(h.Modalities, "image"):
		return Info{}, errors.New("hello: video needs the image modality to embed frames")
	}
	// Office documents are extracted in Go and embedded as text.
	kinds := []kind.Kind{kind.Text, kind.Office}
	for _, m := range h.Modalities {
		switch m {
		case "text":
		case "image":
			kinds = append(kinds, kind.Image)
		case "pdf":
			kinds = append(kinds, kind.PDF)
		case "audio":
			kinds = append(kinds, kind.Audio)
		case "video":
			kinds = append(kinds, kind.Video)
		default:
			// A newer sidecar may offer more; use what we know.
		}
	}
	return Info{Model: h.Model, Precision: h.Precision, Dims: h.Dims,
		Kinds: kind.SetOf(kinds...), MaxBatch: h.MaxBatch}, nil
}

func (h helloReply) equal(o helloReply) bool {
	return h.Protocol == o.Protocol && h.Model == o.Model && h.Precision == o.Precision &&
		h.Dims == o.Dims && slices.Equal(h.Modalities, o.Modalities) &&
		h.MaxBatch == o.MaxBatch && h.MaxTokens == o.MaxTokens
}

func (s *Sidecar) Info() Info { return s.info }

// EmbedTexts embeds pre-formatted texts.
func (s *Sidecar) EmbedTexts(ctx context.Context, p Priority, texts []string) ([][]float32, error) {
	if len(texts) == 0 || len(texts) > s.info.MaxBatch {
		return nil, fmt.Errorf("sidecar: batch of %d, allowed 1..%d", len(texts), s.info.MaxBatch)
	}
	prio, release, err := s.admit(ctx, p)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := s.call(ctx, request{Op: "embed_text", Priority: prio, Texts: texts})
	if err != nil {
		return nil, err
	}
	return s.vectors(r, len(texts))
}

// EmbedImage embeds the image file at path.
func (s *Sidecar) EmbedImage(ctx context.Context, p Priority, path string) ([]float32, error) {
	if !s.info.Kinds.Has(kind.Image) || s.info.Kinds == 0 {
		return nil, errors.New("sidecar: image modality not available")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("sidecar: image path %q is not absolute", path)
	}
	prio, release, err := s.admit(ctx, p)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := s.call(ctx, request{Op: "embed_image", Priority: prio, Path: path})
	if err != nil {
		return nil, err
	}
	vs, err := s.vectors(r, 1)
	if err != nil {
		return nil, err
	}
	return vs[0], nil
}

// ExtractPDF returns the text of a run of pages and embeds text-less ones.
func (s *Sidecar) ExtractPDF(ctx context.Context, p Priority, req PDFRequest) (PDFPages, error) {
	if !s.info.Kinds.Has(kind.PDF) || s.info.Kinds == 0 {
		return PDFPages{}, errors.New("sidecar: pdf modality not available")
	}
	if err := req.validate(); err != nil {
		return PDFPages{}, fmt.Errorf("sidecar: %w", err)
	}
	prio, release, err := s.admit(ctx, p)
	if err != nil {
		return PDFPages{}, err
	}
	defer release()
	r, err := s.call(ctx, request{Op: "extract_pdf", Priority: prio, Path: req.Path,
		From: req.From, Count: req.Count, Render: req.Render})
	if err != nil {
		return PDFPages{}, err
	}
	out := PDFPages{PageCount: r.PageCount, Pages: make([]PDFPage, len(r.Pages))}
	for i, pg := range r.Pages {
		out.Pages[i] = PDFPage{Page: pg.Page, Text: pg.Text, Unrendered: pg.Unrendered}
		if pg.Vector != "" {
			v, err := decodeVector(pg.Vector, s.info.Dims)
			if err != nil {
				return PDFPages{}, s.violation(fmt.Errorf("pdf page %d vector: %w", pg.Page, err))
			}
			out.Pages[i].Vec = v
		}
	}
	if err := out.check(req, s.info.Dims); err != nil {
		return PDFPages{}, s.violation(err)
	}
	return out, nil
}

// ProbeMedia reports the duration and tracks of an audio or video file.
func (s *Sidecar) ProbeMedia(ctx context.Context, p Priority, path string) (MediaInfo, error) {
	if s.info.Kinds == 0 || (!s.info.Kinds.Has(kind.Audio) && !s.info.Kinds.Has(kind.Video)) {
		return MediaInfo{}, errors.New("sidecar: audio/video modality not available")
	}
	if !filepath.IsAbs(path) {
		return MediaInfo{}, fmt.Errorf("sidecar: media path %q is not absolute", path)
	}
	prio, release, err := s.admit(ctx, p)
	if err != nil {
		return MediaInfo{}, err
	}
	defer release()
	r, err := s.call(ctx, request{Op: "probe_media", Priority: prio, Path: path})
	if err != nil {
		return MediaInfo{}, err
	}
	m := MediaInfo{DurationMS: r.Duration, Audio: r.Audio, Video: r.Video}
	if err := m.check(); err != nil {
		return MediaInfo{}, s.violation(err)
	}
	return m, nil
}

// EmbedAudio embeds a window of a file's audio; nil means silent.
func (s *Sidecar) EmbedAudio(ctx context.Context, p Priority, req AudioRequest) ([]float32, error) {
	if s.info.Kinds == 0 || !s.info.Kinds.Has(kind.Audio) {
		return nil, errors.New("sidecar: audio modality not available")
	}
	if err := req.validate(); err != nil {
		return nil, fmt.Errorf("sidecar: %w", err)
	}
	prio, release, err := s.admit(ctx, p)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := s.call(ctx, request{Op: "embed_audio", Priority: prio, Path: req.Path, StartMS: &req.StartMS, EndMS: &req.EndMS})
	if err != nil {
		return nil, err
	}
	if r.Silent {
		if len(r.Vectors) != 0 {
			return nil, s.violation(errors.New("silent audio window with a vector"))
		}
		return nil, nil
	}
	vs, err := s.vectors(r, 1)
	if err != nil {
		return nil, err
	}
	return vs[0], nil
}

// EmbedFrames embeds the video frames nearest the given times.
func (s *Sidecar) EmbedFrames(ctx context.Context, p Priority, path string, timesMS []int64) ([][]float32, error) {
	if s.info.Kinds == 0 || !s.info.Kinds.Has(kind.Video) {
		return nil, errors.New("sidecar: video modality not available")
	}
	if err := validateFrames(path, timesMS); err != nil {
		return nil, fmt.Errorf("sidecar: %w", err)
	}
	prio, release, err := s.admit(ctx, p)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := s.call(ctx, request{Op: "embed_frames", Priority: prio, Path: path, TimesMS: timesMS})
	if err != nil {
		return nil, err
	}
	return s.vectors(r, len(timesMS))
}

// admit maps a priority to the wire value and, for index work, takes the
// index slot so queries wait behind at most one index call.
func (s *Sidecar) admit(ctx context.Context, p Priority) (string, func(), error) {
	if p != Index {
		return "query", func() {}, nil
	}
	select {
	case s.indexSlot <- struct{}{}:
		return "index", func() { <-s.indexSlot }, nil
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
}

// vectors decodes and validates exactly n vectors from r.
func (s *Sidecar) vectors(r reply, n int) ([][]float32, error) {
	if len(r.Vectors) != n {
		return nil, s.violation(fmt.Errorf("%d vectors for %d inputs", len(r.Vectors), n))
	}
	out := make([][]float32, n)
	for i, b64 := range r.Vectors {
		v, err := decodeVector(b64, s.info.Dims)
		if err != nil {
			return nil, s.violation(fmt.Errorf("vector %d: %w", i, err))
		}
		out[i] = v
	}
	return out, nil
}

func decodeVector(b64 string, dims int) ([]float32, error) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(b) != 4*dims {
		return nil, fmt.Errorf("%d bytes, want %d", len(b), 4*dims)
	}
	v := make([]float32, dims)
	var norm float64
	for i := range v {
		x := math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, errors.New("non-finite value")
		}
		v[i] = x
		norm += float64(x) * float64(x)
	}
	if math.Abs(norm-1) > 1e-2 {
		return nil, fmt.Errorf("not normalized (|v|²=%.4f)", norm)
	}
	return v, nil
}

// violation means the sidecar broke the protocol: a bug that restarting will
// not fix. The process is killed and the error is fatal.
func (s *Sidecar) violation(err error) error {
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	if p != nil {
		p.kill()
	}
	s.log.Error("sidecar protocol violation", "err", err)
	return fmt.Errorf("%w: sidecar protocol violation: %v", ErrFatal, err)
}

// call sends req to a live sidecar (restarting it if needed) and waits.
func (s *Sidecar) call(ctx context.Context, req request) (reply, error) {
	p, err := s.live(ctx)
	if err != nil {
		return reply{}, err
	}
	s.mu.Lock()
	s.nextID++
	req.ID = s.nextID
	s.mu.Unlock()

	ctx, cancel := context.WithTimeoutCause(ctx, s.cfg.CallTimeout, errCallTimeout)
	defer cancel()
	r, err := p.roundTrip(ctx, req)
	switch {
	case errors.Is(err, errCallTimeout):
		// Our deadline, not the caller's: the sidecar is hung.
		s.log.Error("sidecar call timed out; killing it", "op", req.Op, "timeout", s.cfg.CallTimeout)
		p.kill()
		return reply{}, fmt.Errorf("%w: call timed out", ErrUnavailable)
	case err != nil:
		return reply{}, err
	}
	if r.Error != nil {
		switch r.Error.Code {
		case "invalid":
			return reply{}, fmt.Errorf("%w: %s", ErrInvalidInput, r.Error.Message)
		case "unavailable":
			return reply{}, fmt.Errorf("%w: %s", ErrUnavailable, r.Error.Message)
		default:
			return reply{}, s.violation(fmt.Errorf("error %q: %s", r.Error.Code, r.Error.Message))
		}
	}
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()
	return r, nil
}

// live returns the running process, restarting it if it has exited.
func (s *Sidecar) live(ctx context.Context) (*proc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("%w: closed", ErrUnavailable)
	}
	if s.p != nil {
		select {
		case <-s.p.done:
			s.reap(s.p)
			s.p = nil
		default:
			return s.p, nil
		}
	}
	if now := time.Now(); now.Before(s.retryAt) {
		return nil, fmt.Errorf("%w: restarting in %v", ErrUnavailable, s.retryAt.Sub(now).Round(time.Second))
	}
	// Starting under the lock is deliberate: concurrent callers wait for one
	// start instead of racing several processes.
	p, h, err := s.spawn(ctx)
	if err != nil {
		s.backoff()
		return nil, fmt.Errorf("%w: restart: %v", ErrUnavailable, err)
	}
	if !h.equal(s.hello) {
		p.kill()
		return nil, fmt.Errorf("%w: sidecar changed across restart: was %+v, now %+v", ErrFatal, s.hello, h)
	}
	s.log.Info("sidecar restarted")
	s.p = p
	return p, nil
}

// reap classifies how a process ended. A clean exit (idle shutdown) is
// normal; anything else counts toward restart backoff. Caller holds s.mu.
func (s *Sidecar) reap(p *proc) {
	if p.err == nil {
		s.log.Debug("sidecar exited cleanly")
		return
	}
	s.log.Warn("sidecar died", "err", p.err)
	s.backoff()
}

// backoff delays the next start: 1s, 2s, 4s … capped at one minute. Caller holds s.mu.
func (s *Sidecar) backoff() {
	s.failures++
	d := min(time.Minute, time.Second<<min(s.failures-1, 6))
	s.retryAt = time.Now().Add(d)
}

// spawn starts the process and completes the hello handshake.
func (s *Sidecar) spawn(ctx context.Context) (*proc, helloReply, error) {
	cmd := exec.Command(s.cfg.Path, s.cfg.Args...)
	cmd.Env = append(os.Environ(), s.cfg.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, helloReply{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, helloReply{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, helloReply{}, err
	}
	if err := cmd.Start(); err != nil {
		return nil, helloReply{}, fmt.Errorf("start %s: %w", s.cfg.Path, err)
	}
	p := &proc{cmd: cmd, stdin: stdin, pending: map[uint64]chan reply{}, done: make(chan struct{})}
	go s.logStderr(stderr)
	go p.read(stdout, s.log)

	ctx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
	defer cancel()
	// Handshakes use id 0, which normal calls never do.
	r, err := p.roundTrip(ctx, request{ID: 0, Op: "hello", Protocol: ProtocolVersion})
	if err == nil && r.Error != nil {
		err = fmt.Errorf("hello failed: %s: %s", r.Error.Code, r.Error.Message)
	}
	var h helloReply
	if err == nil {
		if err = json.Unmarshal(r.raw, &h); err != nil {
			err = fmt.Errorf("hello: %w", err)
		}
	}
	if err != nil {
		p.kill()
		return nil, helloReply{}, err
	}
	return p, h, nil
}

func (s *Sidecar) logStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 64<<10)
	for sc.Scan() {
		s.log.Info("semd-embed: " + sc.Text())
	}
	if err := sc.Err(); err != nil {
		// Keep draining: a full stderr pipe would block the sidecar.
		s.log.Warn("semd-embed stderr unreadable, discarding the rest", "err", err)
		if _, err := io.Copy(io.Discard, r); err != nil {
			s.log.Warn("semd-embed stderr", "err", err)
		}
	}
}

// read dispatches responses until stdout closes, then fails everything pending.
func (p *proc) read(stdout io.Reader, log *slog.Logger) {
	br := bufio.NewReaderSize(stdout, 64<<10)
	var err error
	for {
		var line []byte
		line, err = readLine(br)
		if err != nil {
			break
		}
		var r reply
		if err = json.Unmarshal(line, &r); err != nil {
			err = fmt.Errorf("bad response line: %w", err)
			break
		}
		r.raw = line
		p.mu.Lock()
		ch, ok := p.pending[r.ID]
		delete(p.pending, r.ID)
		p.mu.Unlock()
		if !ok {
			// The caller gave up (cancelled or timed out) before the reply.
			log.Debug("dropping late sidecar reply", "id", r.ID)
			continue
		}
		ch <- r // buffered
	}
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		p.kill() // broken stream: make sure Wait returns
	}
	if werr := p.cmd.Wait(); werr != nil && err == nil {
		err = werr
	}
	p.mu.Lock()
	if n := len(p.pending); n > 0 && err == nil {
		err = fmt.Errorf("exited with %d requests pending", n)
	}
	p.err = err
	p.pending = nil // waiters observe done
	p.mu.Unlock()
	close(p.done)
}

// readLine reads one '\n'-terminated line of at most MaxLine bytes.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > MaxLine {
			return nil, fmt.Errorf("response line over %d bytes", MaxLine)
		}
		switch {
		case err == nil:
			return buf[:len(buf)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(buf) > 0:
			return nil, errors.New("truncated response line")
		default:
			return nil, err
		}
	}
}

func (p *proc) roundTrip(ctx context.Context, req request) (reply, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return reply{}, err
	}
	ch := make(chan reply, 1)
	p.mu.Lock()
	if p.pending == nil {
		p.mu.Unlock()
		return reply{}, fmt.Errorf("%w: sidecar exited", ErrUnavailable)
	}
	p.pending[req.ID] = ch
	p.mu.Unlock()
	forget := func() {
		p.mu.Lock()
		if p.pending != nil {
			delete(p.pending, req.ID)
		}
		p.mu.Unlock()
	}

	p.wmu.Lock()
	_, err = p.stdin.Write(append(b, '\n'))
	p.wmu.Unlock()
	if err != nil {
		forget()
		return reply{}, fmt.Errorf("%w: write: %v", ErrUnavailable, err)
	}
	select {
	case r := <-ch:
		return r, nil
	case <-p.done:
		select { // the reply may have raced the exit
		case r := <-ch:
			return r, nil
		default:
		}
		msg := "sidecar exited"
		if p.err != nil {
			msg = "sidecar died: " + p.err.Error()
		}
		return reply{}, fmt.Errorf("%w: %s", ErrUnavailable, msg)
	case <-ctx.Done():
		forget()
		return reply{}, context.Cause(ctx)
	}
}

func (p *proc) kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// Close asks the sidecar to exit (stdin EOF) and kills it if it lingers.
func (s *Sidecar) Close() error {
	s.mu.Lock()
	s.closed = true
	p := s.p
	s.p = nil
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	p.wmu.Lock()
	p.stdin.Close()
	p.wmu.Unlock()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		p.kill()
		<-p.done
		return errors.New("sidecar: did not exit on stdin EOF; killed")
	}
	if p.err != nil {
		return fmt.Errorf("sidecar: %w", p.err)
	}
	return nil
}
