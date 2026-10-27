// Command semd is the Kim Quy daemon: it indexes the configured
// directories and serves the API on a Unix socket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/msongd/kimquy/internal/api"
	"github.com/msongd/kimquy/internal/config"
	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/engine"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/fsutil"
	mdl "github.com/msongd/kimquy/internal/model"
	"github.com/msongd/kimquy/internal/power"
	"github.com/msongd/kimquy/internal/watch"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "semd:", err)
		os.Exit(1)
	}
}

func run() error {
	defState, err := config.DefaultStateDir()
	if err != nil {
		return err
	}
	state := flag.String("state", defState, "state directory (config, catalog, vectors)")
	socket := flag.String("socket", "", "API socket path (default <state>/semd.sock)")
	embedder := flag.String("embedder", "sidecar", "embedder: sidecar | fake (testing only: bag-of-words, not semantic)")
	sidecarPath := flag.String("sidecar", "", "semd-embed executable (default: next to semd)")
	modelDir := flag.String("model", "", "model directory holding text-q8/ (and vision-q8/, audio-bf16/) (default: the app bundle's Resources/models, else <state>/models)")
	nice := flag.Int("nice", 10, "scheduling niceness for the daemon")
	watchFS := flag.Bool("watch", true, "follow file changes with FSEvents (false: full scan at every start)")
	powerPolicy := flag.Bool("power-policy", true, "pause indexing on battery, in Low Power Mode, or when hot")
	verbose := flag.Bool("v", false, "debug logging")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	var emb embed.Embedder
	switch *embedder {
	case "fake":
		log.Warn("using the fake embedder: results are keyword-based, not semantic")
		emb = &embed.Fake{Dims: 768}
	case "sidecar":
		sc, err := startSidecar(*sidecarPath, *modelDir, *state, log)
		if err != nil {
			return err
		}
		defer func() {
			if err := sc.Close(); err != nil {
				log.Error("sidecar close", "err", err)
			}
		}()
		emb = sc
	default:
		return fmt.Errorf("unknown embedder %q", *embedder)
	}

	if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, *nice); err != nil {
		return fmt.Errorf("setpriority: %w", err)
	}
	syscall.Umask(0o077)
	if err := os.MkdirAll(*state, 0o700); err != nil {
		return err
	}
	lock, err := fsutil.LockDir(*state)
	if err != nil {
		return fmt.Errorf("another semd is running? %w", err)
	}
	defer lock.Close()

	opts := engine.Options{StateDir: *state, Embedder: emb, Logger: log, Bus: events.NewBus()}
	if *watchFS {
		if opts.Watch, err = watch.Native(); err != nil {
			return fmt.Errorf("%w (use -watch=false)", err)
		}
	}
	if *powerPolicy {
		if _, err := power.Probe(); err != nil {
			return fmt.Errorf("%w (use -power-policy=false)", err)
		}
		opts.Power = power.Probe
	}
	eng, err := engine.Open(opts)
	if err != nil {
		return err
	}
	defer func() {
		if err := eng.Close(); err != nil {
			log.Error("close", "err", err)
		}
	}()

	sock := *socket
	if sock == "" {
		sock = filepath.Join(*state, "semd.sock")
	}
	// sockaddr_un.sun_path is 104 bytes on macOS, including the NUL; bind
	// only reports EINVAL, so say what is wrong.
	if len(sock) >= 104 {
		return fmt.Errorf("socket path is %d bytes, the limit is 103: %s (use -socket)", len(sock), sock)
	}
	// We hold the instance lock, so an existing socket file is stale.
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{
		Handler:           api.Handler(eng, log),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancelCause(ctx)
	// Long-lived requests (SSE) end when the daemon stops.
	srv.BaseContext = func(net.Listener) context.Context { return ctx }
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel(fmt.Errorf("api server: %w", err))
		}
	}()
	log.Info("semd ready", "socket", sock, "state", *state, "model", emb.Info().Model)

	runErr := eng.Run(ctx)
	if cause := context.Cause(ctx); runErr == nil && cause != nil && !errors.Is(cause, context.Canceled) {
		runErr = cause
	}
	shutCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Warn("api shutdown", "err", err)
	}
	os.Remove(sock)
	log.Info("semd stopped")
	return runErr
}

// startSidecar launches semd-embed. Output dims are fixed to what the store
// keeps, so vectors cross the pipe already truncated and re-normalized.
func startSidecar(path, model, state string, log *slog.Logger) (*embed.Sidecar, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	if model == "" {
		// A full Kim Quy.app carries the weights (Contents/Resources/models);
		// a slim one has the app download them into the state directory.
		model = mdl.Find(exe, state)
	}
	if model, err = filepath.Abs(model); err != nil {
		return nil, err
	}
	have, err := mdl.Installed(model)
	if err != nil {
		return nil, fmt.Errorf("%w: open Kim Quy to download it again", err)
	}
	if !have[mdl.Parts[0].Dir] {
		return nil, fmt.Errorf("no model in %s (%s/ is missing): open Kim Quy to download it", model, mdl.Parts[0].Dir)
	}
	log.Info("model", "dir", model, "parts", have)
	if path == "" {
		path = filepath.Join(filepath.Dir(exe), "semd-embed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return embed.StartSidecar(ctx, embed.SidecarConfig{
		Path:   path,
		Args:   []string{"--model", model, "--dims", strconv.Itoa(engine.StoreDim)},
		Logger: log,
	})
}
