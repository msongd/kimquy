package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/msongd/kimquy/internal/fsutil"
)

// staging is the folder of a model directory where parts are assembled.
const staging = ".staging"

// Progress is told how many bytes of how many are done.
type Progress func(done, total int64)

// Installer puts parts into a model directory. Only one at a time works on
// a directory: it holds dir/LOCK.
type Installer struct {
	Dir string // the model directory; created if missing
	// Base is the URL the parts' files are under (BaseURL; tests use their
	// own server), and Client what fetches them.
	Base   string
	Client *http.Client
	// Idle is how long a download may receive nothing before it fails.
	Idle time.Duration
}

// NewInstaller downloads from BaseURL into dir.
func NewInstaller(dir string) *Installer {
	return &Installer{
		Dir:  dir,
		Base: BaseURL,
		Client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}},
		Idle: 60 * time.Second,
	}
}

func (in *Installer) check(parts []Part) error {
	if !filepath.IsAbs(in.Dir) || filepath.Clean(in.Dir) != in.Dir {
		return fmt.Errorf("model: directory %q is not a clean absolute path", in.Dir)
	}
	if len(parts) == 0 {
		return errors.New("model: no parts")
	}
	for _, p := range parts {
		if q, ok := PartByDir(p.Dir); !ok || !slices.Equal(q.Files, p.Files) {
			return fmt.Errorf("model: unknown part %q", p.Dir)
		}
	}
	return nil
}

// lock creates the directory and takes its lock.
func (in *Installer) lock() (*os.File, error) {
	if err := os.MkdirAll(in.Dir, 0o700); err != nil {
		return nil, err
	}
	l, err := fsutil.LockDir(in.Dir)
	if err != nil {
		return nil, fmt.Errorf("model: another download or import is in progress (%w)", err)
	}
	return l, nil
}

// missing returns the parts not yet whole in the directory. A damaged one
// is deleted, to be fetched again: the directory is the installer's own.
func (in *Installer) missing(parts []Part) ([]Part, error) {
	var out []Part
	for _, p := range parts {
		ok, err := Check(in.Dir, p)
		if err != nil {
			if err := os.RemoveAll(filepath.Join(in.Dir, p.Dir)); err != nil {
				return nil, err
			}
		}
		if !ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// Download fetches the parts the directory does not have yet, verifying
// every file. An interrupted download resumes where it stopped.
func (in *Installer) Download(ctx context.Context, parts []Part, progress Progress) error {
	if err := in.check(parts); err != nil {
		return err
	}
	if in.Client == nil || in.Idle <= 0 || !strings.HasPrefix(in.Base, "http") {
		return errors.New("model: installer has no client, idle timeout or base URL")
	}
	l, err := in.lock()
	if err != nil {
		return err
	}
	defer l.Close()
	if parts, err = in.missing(parts); err != nil || len(parts) == 0 {
		return err
	}
	var total, done int64
	for _, p := range parts {
		total += p.Size()
		for _, f := range p.Files {
			done += in.staged(p, f)
		}
	}
	if err := enoughSpace(in.Dir, total-done); err != nil {
		return err
	}
	report := func(n int64) {
		done += n
		if progress != nil {
			progress(done, total)
		}
	}
	report(0)
	for _, p := range parts {
		for _, f := range p.Files {
			if err := in.fetch(ctx, p, f, report); err != nil {
				return err
			}
		}
		if err := in.place(p); err != nil {
			return err
		}
	}
	return in.cleanStaging()
}

// staged is how many bytes of f are already downloaded.
func (in *Installer) staged(p Part, f File) int64 {
	dir := filepath.Join(in.Dir, staging, p.Dir)
	if fi, err := os.Stat(filepath.Join(dir, f.Name)); err == nil && fi.Size() == f.Size {
		return f.Size
	}
	if fi, err := os.Stat(filepath.Join(dir, f.Name+".part")); err == nil && fi.Size() <= f.Size {
		return fi.Size()
	}
	return 0
}

var errStalled = errors.New("model: the download stalled")

// fetch downloads f into the staging folder, resuming a partial file, and
// verifies it.
func (in *Installer) fetch(ctx context.Context, p Part, f File, report func(int64)) error {
	dir := filepath.Join(in.Dir, staging, p.Dir)
	final := filepath.Join(dir, f.Name)
	if fi, err := os.Stat(final); err == nil && fi.Size() == f.Size {
		if err := verifyFile(final, f, nil); err == nil {
			return nil // downloaded before (and counted as done)
		}
		report(-f.Size)
		if err := os.Remove(final); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	partial := final + ".part"
	out, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	off, err := out.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if off > f.Size { // not ours to resume
		report(-min(off, f.Size))
		if err := out.Truncate(0); err != nil {
			return err
		}
		off = 0
	}
	if off < f.Size {
		if err := in.get(ctx, p, f, out, off, report); err != nil {
			return err
		}
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := verifyFile(partial, f, nil); err != nil {
		report(-f.Size)
		os.Remove(partial) // corrupt: the next try starts over
		return err
	}
	return os.Rename(partial, final)
}

// get appends the bytes of f from off to out.
func (in *Installer) get(ctx context.Context, p Part, f File, out *os.File, off int64, report func(int64)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	url := in.Base + "/" + p.Dir + "/" + f.Name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if off > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-")
	}
	resp, err := in.Client.Do(req)
	if err != nil {
		return fmt.Errorf("model: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		if off > 0 { // the server sends it all again
			report(-off)
			if err := out.Truncate(0); err != nil {
				return err
			}
			if _, err := out.Seek(0, io.SeekStart); err != nil {
				return err
			}
			off = 0
		}
	case resp.StatusCode == http.StatusPartialContent && off > 0:
		want := fmt.Sprintf("bytes %d-%d/%d", off, f.Size-1, f.Size)
		if got := resp.Header.Get("Content-Range"); got != want {
			return fmt.Errorf("model: download %s: Content-Range %q, want %q", url, got, want)
		}
	default:
		return fmt.Errorf("model: download %s: %s", url, resp.Status)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != f.Size-off {
		return fmt.Errorf("model: download %s: %d bytes offered, want %d", url, resp.ContentLength, f.Size-off)
	}
	stall := time.AfterFunc(in.Idle, func() { cancel(errStalled) })
	defer stall.Stop()
	body := &counter{r: io.LimitReader(resp.Body, f.Size-off), add: func(n int64) {
		stall.Reset(in.Idle)
		report(n)
	}}
	n, err := io.Copy(out, body)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(err, cause) {
			err = fmt.Errorf("%w (%w)", cause, err)
		}
		return fmt.Errorf("model: download %s: %w", url, err)
	}
	if off+n != f.Size {
		return fmt.Errorf("model: download %s: cut short at %d of %d bytes; try again to resume", url, off+n, f.Size)
	}
	return nil
}

// place moves a part whose files are all staged and verified into the
// model directory.
func (in *Installer) place(p Part) error {
	src := filepath.Join(in.Dir, staging, p.Dir)
	names, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range names { // only the part's files go in
		if !slices.ContainsFunc(p.Files, func(f File) bool { return f.Name == e.Name() }) {
			if err := os.RemoveAll(filepath.Join(src, e.Name())); err != nil {
				return err
			}
		}
	}
	if err := fsutil.SyncDir(src); err != nil {
		return err
	}
	if err := os.Rename(src, filepath.Join(in.Dir, p.Dir)); err != nil {
		return fmt.Errorf("model: install %s: %w", p.Dir, err)
	}
	return fsutil.SyncDir(in.Dir)
}

// cleanStaging removes the staging folder once nothing is left in it.
func (in *Installer) cleanStaging() error {
	err := os.Remove(filepath.Join(in.Dir, staging))
	if err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTEMPTY) {
		return nil
	}
	return err
}

// enoughSpace fails when the volume of dir has less than need bytes free,
// with some room to spare.
func enoughSpace(dir string, need int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if spare := int64(256 << 20); free < need+spare {
		return fmt.Errorf("model: %s free on this disk; the model needs %s and a little room to spare", Bytes(free), Bytes(need))
	}
	return nil
}

// Source finds the model directory in what the user chose: a model
// directory, one of its part folders, or an app with a bundled model.
func Source(path string) (string, error) {
	path = filepath.Clean(path)
	if strings.HasSuffix(path, ".app") {
		path = filepath.Join(path, "Contents", "Resources", "models")
	}
	if _, ok := PartByDir(filepath.Base(path)); ok {
		path = filepath.Dir(path)
	}
	have, err := Installed(path)
	if err != nil {
		return "", err
	}
	for _, ok := range have {
		if ok {
			return path, nil
		}
	}
	return "", fmt.Errorf("model: no EmbeddingGemma 2 model in %s: choose the folder that holds %s", path, Parts[0].Dir)
}

// Import copies the parts that src (see Source) has and the directory does
// not, verifying every file, and returns the parts it copied. On the same
// volume the copies are clones: they take no more space.
func (in *Installer) Import(ctx context.Context, src string, progress Progress) ([]Part, error) {
	src, err := Source(src)
	if err != nil {
		return nil, err
	}
	have, _ := Installed(src) // Source checked it
	var parts []Part
	for _, p := range Parts {
		if have[p.Dir] {
			parts = append(parts, p)
		}
	}
	if err := in.check(parts); err != nil {
		return nil, err
	}
	if same, err := sameDir(src, in.Dir); err != nil || same {
		if err == nil {
			err = fmt.Errorf("model: %s is the model already in use", src)
		}
		return nil, err
	}
	l, err := in.lock()
	if err != nil {
		return nil, err
	}
	defer l.Close()
	if parts, err = in.missing(parts); err != nil || len(parts) == 0 {
		return nil, err
	}
	var total, done int64
	for _, p := range parts {
		total += p.Size()
	}
	report := func(n int64) {
		done += n
		if progress != nil {
			progress(done, total)
		}
	}
	report(0)
	for _, p := range parts {
		dir := filepath.Join(in.Dir, staging, p.Dir)
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		for _, f := range p.Files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			from, to := filepath.Join(src, p.Dir, f.Name), filepath.Join(dir, f.Name)
			if err := copyFile(ctx, from, to); err != nil {
				return nil, fmt.Errorf("model: copy %s: %w", from, err)
			}
			if err := verifyFile(to, f, report); err != nil {
				return nil, err
			}
		}
		if err := in.place(p); err != nil {
			return nil, err
		}
	}
	return parts, in.cleanStaging()
}

// copyFile clones from to to (cp -c: APFS clonefile), copying the bytes
// when it cannot (another volume or file system).
func copyFile(ctx context.Context, from, to string) error {
	fi, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", from)
	}
	if err := exec.CommandContext(ctx, "/bin/cp", "-c", "--", from, to).Run(); err == nil {
		return nil
	} else if ctx.Err() != nil {
		return ctx.Err()
	}
	os.Remove(to) // clonefile failed (another volume): copy the bytes
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

// sameDir reports whether a and b are the same directory.
func sameDir(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// Bytes formats n for people: 305 MB, 1.1 GB.
func Bytes(n int64) string {
	switch {
	case n >= 1e9:
		return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + " GB"
	case n >= 1e6:
		return strconv.FormatInt((n+5e5)/1e6, 10) + " MB"
	case n >= 1e3:
		return strconv.FormatInt((n+500)/1e3, 10) + " KB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}
