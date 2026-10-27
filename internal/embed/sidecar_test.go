package embed

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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The test binary doubles as a fake semd-embed when SEMD_FAKE_SIDECAR=1.
// Behaviour switches (env):
//
//	FAKE_STATE    file counting launches (required)
//	FAKE_DIE      exit 1 after this many embed replies (first launch only)
//	FAKE_IDLE     exit 0 after this many embed replies
//	FAKE_HANG     never answer embed_text
//	FAKE_BAD      dims | nan | count | unnorm | garbage | internal | invalid | silentvec
//	FAKE_MODALITIES comma-separated hello modalities (default "text"); media
//	              ops are served by Fake
//	FAKE_RENAME   report a different model on relaunch
//	FAKE_DELAY_MS sleep per embed request
//	FAKE_TRACE    file to append "+index"/"-index" markers to
func TestMain(m *testing.M) {
	if os.Getenv("SEMD_FAKE_SIDECAR") == "1" {
		fakeSidecar()
		return
	}
	os.Exit(m.Run())
}

func envInt(k string) int {
	n, _ := strconv.Atoi(os.Getenv(k))
	return n
}

func fakeSidecar() {
	launch := 1
	if b, err := os.ReadFile(os.Getenv("FAKE_STATE")); err == nil {
		launch, _ = strconv.Atoi(string(b))
		launch++
	}
	if err := os.WriteFile(os.Getenv("FAKE_STATE"), []byte(strconv.Itoa(launch)), 0o600); err != nil {
		panic(err)
	}
	model := "fake-gemma"
	if os.Getenv("FAKE_RENAME") != "" && launch > 1 {
		model = "other-gemma"
	}
	const dims = 256
	f := &Fake{Dims: dims}
	var wmu sync.Mutex
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		out.Write(append(b, '\n'))
		out.Flush()
		wmu.Unlock()
	}
	var served atomic.Int32
	var wg sync.WaitGroup
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, MaxLine)
	for sc.Scan() {
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, "bad request:", err)
			os.Exit(2)
		}
		if req.Op == "hello" {
			modalities := []string{"text"}
			if m := os.Getenv("FAKE_MODALITIES"); m != "" {
				modalities = strings.Split(m, ",")
			}
			send(map[string]any{"id": req.ID, "protocol": ProtocolVersion, "model": model, "precision": "fp32",
				"dims": dims, "modalities": modalities, "max_batch": 8, "max_tokens": 8192})
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if os.Getenv("FAKE_HANG") != "" {
				select {}
			}
			if req.Priority == "index" {
				trace("+index")
				defer trace("-index")
			}
			time.Sleep(time.Duration(envInt("FAKE_DELAY_MS")) * time.Millisecond)
			ctx := context.Background()
			fail := func(err error) {
				code := "internal"
				if errors.Is(err, ErrInvalidInput) {
					code = "invalid"
				}
				send(map[string]any{"id": req.ID, "error": map[string]string{"code": code, "message": err.Error()}})
			}
			var (
				vecs [][]float32
				err  error
			)
			resp := map[string]any{"id": req.ID}
			switch req.Op {
			case "probe_media":
				m, err := f.ProbeMedia(ctx, Index, req.Path)
				if err != nil {
					fail(err)
					return
				}
				send(map[string]any{"id": req.ID, "duration_ms": m.DurationMS, "audio": m.Audio, "video": m.Video})
				return
			case "embed_audio":
				var v []float32
				if v, err = f.EmbedAudio(ctx, Index, AudioRequest{req.Path, *req.StartMS, *req.EndMS}); v != nil {
					vecs = [][]float32{v}
				} else if err == nil {
					resp["silent"] = true
					if os.Getenv("FAKE_BAD") == "silentvec" {
						vecs = [][]float32{f.vector("noise")}
					}
				}
			case "embed_frames":
				vecs, err = f.EmbedFrames(ctx, Index, req.Path, req.TimesMS)
			default:
				vecs, err = f.EmbedTexts(ctx, Index, req.Texts)
			}
			if err != nil {
				fail(err)
				return
			}
			enc := make([]string, len(vecs))
			for i, v := range vecs {
				b := make([]byte, 4*len(v))
				for j, x := range v {
					binary.LittleEndian.PutUint32(b[4*j:], math.Float32bits(x))
				}
				switch os.Getenv("FAKE_BAD") {
				case "dims":
					b = b[:len(b)-4]
				case "nan":
					binary.LittleEndian.PutUint32(b, math.Float32bits(float32(math.NaN())))
				case "unnorm":
					binary.LittleEndian.PutUint32(b, math.Float32bits(5))
				}
				enc[i] = base64.StdEncoding.EncodeToString(b)
			}
			switch os.Getenv("FAKE_BAD") {
			case "count":
				enc = enc[1:]
			case "garbage":
				wmu.Lock()
				out.WriteString("{not json\n")
				out.Flush()
				wmu.Unlock()
				return
			case "internal", "invalid":
				send(map[string]any{"id": req.ID, "error": map[string]string{"code": os.Getenv("FAKE_BAD"), "message": "nope"}})
				return
			}
			resp["vectors"] = enc
			send(resp)
			n := int(served.Add(1))
			if d := envInt("FAKE_DIE"); d > 0 && n >= d && launch == 1 {
				os.Exit(1)
			}
			if d := envInt("FAKE_IDLE"); d > 0 && n >= d {
				os.Exit(0)
			}
		}()
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "stdin:", err)
		os.Exit(2)
	}
	wg.Wait()
	os.Exit(0) // stdin EOF
}

var traceMu sync.Mutex

func trace(s string) {
	p := os.Getenv("FAKE_TRACE")
	if p == "" {
		return
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(f, s)
	f.Close()
}

func startFake(t *testing.T, env ...string) *Sidecar {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env = append(env, "SEMD_FAKE_SIDECAR=1", "FAKE_STATE="+filepath.Join(t.TempDir(), "launches"))
	s, err := StartSidecar(context.Background(), SidecarConfig{
		Path: exe, Env: env, StartTimeout: 10 * time.Second, CallTimeout: 2 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func embedOK(t *testing.T, s *Sidecar, texts ...string) [][]float32 {
	t.Helper()
	v, err := s.EmbedTexts(context.Background(), Query, texts)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSidecarRoundTrip(t *testing.T) {
	s := startFake(t)
	if i := s.Info(); i.Model != "fake-gemma" || i.Dims != 256 || i.MaxBatch != 8 {
		t.Fatalf("info %+v", i)
	}
	vs := embedOK(t, s, "alpha beta", "gamma")
	want, _ := (&Fake{Dims: 256}).EmbedTexts(context.Background(), Query, []string{"alpha beta", "gamma"})
	for i := range vs {
		for j := range vs[i] {
			if vs[i][j] != want[i][j] {
				t.Fatalf("vector %d differs at %d", i, j)
			}
		}
	}
	if _, err := s.EmbedTexts(context.Background(), Query, make([]string, 9)); err == nil {
		t.Fatal("oversize batch accepted")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := s.EmbedTexts(context.Background(), Query, []string{"x"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("after close: %v", err)
	}
}

func TestSidecarBadOutputIsFatal(t *testing.T) {
	for _, bad := range []string{"dims", "nan", "count", "unnorm", "garbage", "internal"} {
		t.Run(bad, func(t *testing.T) {
			s := startFake(t, "FAKE_BAD="+bad)
			_, err := s.EmbedTexts(context.Background(), Query, []string{"a", "b"})
			if bad == "garbage" {
				// A broken stream kills the process; the next call restarts it.
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("got %v", err)
				}
				return
			}
			if !errors.Is(err, ErrFatal) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestSidecarInvalidInputIsPerFile(t *testing.T) {
	s := startFake(t, "FAKE_BAD=invalid")
	_, err := s.EmbedTexts(context.Background(), Index, []string{"a"})
	if err == nil || errors.Is(err, ErrFatal) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestSidecarCrashRestarts(t *testing.T) {
	s := startFake(t, "FAKE_DIE=1")
	embedOK(t, s, "first") // the process exits right after replying
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := s.EmbedTexts(context.Background(), Query, []string{"second"})
		if err == nil {
			break
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("want transient error, got %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("never recovered: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSidecarIdleExitRestartsImmediately(t *testing.T) {
	s := startFake(t, "FAKE_IDLE=1")
	embedOK(t, s, "one")
	<-waitDone(s)
	embedOK(t, s, "two") // clean exit: no backoff
}

func waitDone(s *Sidecar) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.p.done
}

func TestSidecarModelChangeIsFatal(t *testing.T) {
	s := startFake(t, "FAKE_IDLE=1", "FAKE_RENAME=1")
	embedOK(t, s, "one")
	<-waitDone(s)
	if _, err := s.EmbedTexts(context.Background(), Query, []string{"two"}); !errors.Is(err, ErrFatal) {
		t.Fatalf("got %v", err)
	}
}

func TestSidecarHangIsKilled(t *testing.T) {
	s := startFake(t, "FAKE_HANG=1")
	t0 := time.Now()
	_, err := s.EmbedTexts(context.Background(), Query, []string{"x"})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	select {
	case <-waitDone(s):
	case <-time.After(5 * time.Second):
		t.Fatal("hung sidecar not killed")
	}
}

func TestSidecarCallerCancel(t *testing.T) {
	s := startFake(t, "FAKE_DELAY_MS=300")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.EmbedTexts(ctx, Query, []string{"x"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	embedOK(t, s, "still alive") // late reply to the cancelled call is dropped
}

// Index calls are admitted one at a time so queries never queue behind more
// than one index batch; queries are not limited.
func TestSidecarIndexSerializedQueriesNot(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "trace")
	s := startFake(t, "FAKE_DELAY_MS=50", "FAKE_TRACE="+tr)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if _, err := s.EmbedTexts(context.Background(), Index, []string{"doc"}); err != nil {
				t.Error(err)
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	t0 := time.Now()
	embedOK(t, s, "query")
	if d := time.Since(t0); d > 250*time.Millisecond {
		t.Errorf("query waited %v behind indexing", d)
	}
	wg.Wait()
	b, _ := os.ReadFile(tr)
	depth := 0
	for _, l := range strings.Fields(string(b)) {
		if l == "+index" {
			depth++
		} else {
			depth--
		}
		if depth > 1 {
			t.Fatalf("index calls overlapped:\n%s", b)
		}
	}
}

func TestStartSidecarRejectsBadHello(t *testing.T) {
	_, err := StartSidecar(context.Background(), SidecarConfig{Path: "/nonexistent/semd-embed"})
	if err == nil {
		t.Fatal("started a missing binary")
	}
	if _, err := (helloReply{Protocol: 1, Model: "m", Precision: "p", Dims: 256, MaxBatch: 8, Modalities: []string{"image"}}).info(); err == nil {
		t.Fatal("accepted a sidecar without text")
	}
	if _, err := (helloReply{Protocol: 2, Model: "m", Precision: "p", Dims: 256, MaxBatch: 8, Modalities: []string{"text"}}).info(); err == nil {
		t.Fatal("accepted protocol 2")
	}
}
