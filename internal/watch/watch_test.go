package watch

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestBufferOverflow(t *testing.T) {
	b := newBuffer(3)
	b.add([]Event{{ID: 1}, {ID: 2}})
	b.add([]Event{{ID: 3}, {ID: 4}})
	evs, over := b.Take()
	if len(evs) != 3 || !over {
		t.Fatalf("%v %v", evs, over)
	}
	select {
	case <-b.Ready():
	default:
		t.Fatal("not signalled")
	}
	if evs, over := b.Take(); len(evs) != 0 || over {
		t.Fatalf("second take %v %v", evs, over)
	}
}

func TestFlagsString(t *testing.T) {
	if s := (Created | IsFile | 0x80000000).String(); s != "Created|IsFile|0x80000000" {
		t.Error(s)
	}
	if !KernelDropped.Lost() || Created.Lost() {
		t.Error("Lost")
	}
}

// collect gathers events until want reports true or the deadline passes.
func collect(t *testing.T, s Stream, d time.Duration, want func([]Event) bool) []Event {
	t.Helper()
	var all []Event
	deadline := time.After(d)
	for {
		select {
		case <-s.Ready():
			evs, over := s.Take()
			if over {
				t.Fatal("overflow")
			}
			all = append(all, evs...)
			if want(all) {
				return all
			}
		case <-deadline:
			t.Fatalf("timed out; got %v", all)
		}
	}
}

func has(evs []Event, path string, f Flags) bool {
	for _, e := range evs {
		if e.Path == path && e.Flags&f == f {
			return true
		}
	}
	return false
}

func native(t *testing.T) System {
	if runtime.GOOS != "darwin" {
		t.Skip("needs macOS")
	}
	sys, err := Native()
	if err != nil {
		t.Fatal(err)
	}
	return sys
}

func TestFSEventsLive(t *testing.T) {
	sys := native(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	if _, err := sys.History(dir); err != nil {
		t.Fatal(err)
	}
	s, err := sys.Watch([]string{dir}, sys.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, b, sub := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt"), filepath.Join(dir, "sub")
	t0 := time.Now()
	os.WriteFile(a, []byte("x"), 0o600)
	collect(t, s, 5*time.Second, func(e []Event) bool { return has(e, a, Created|IsFile) })
	t.Logf("first event after %v", time.Since(t0).Round(time.Millisecond))
	os.Rename(a, b)
	os.Mkdir(sub, 0o700)
	collect(t, s, 5*time.Second, func(e []Event) bool {
		return has(e, a, Renamed) && has(e, b, Renamed) && has(e, sub, Created|IsDir)
	})
	os.Remove(b)
	collect(t, s, 5*time.Second, func(e []Event) bool { return has(e, b, Removed) })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
}

// Changes made while nobody watched are replayed from a saved event ID.
func TestFSEventsReplay(t *testing.T) {
	sys := native(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	since := sys.Now()
	p := filepath.Join(dir, "offline.txt")
	os.WriteFile(p, []byte("written while semd was down"), 0o600)
	time.Sleep(time.Second) // let fseventsd record it
	s, err := sys.Watch([]string{dir}, since)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	evs := collect(t, s, 10*time.Second, func(e []Event) bool { return has(e, p, Created) && done(e) })
	for _, e := range evs {
		if e.Flags&HistoryDone == 0 && e.ID <= since {
			t.Errorf("replayed event %d at or before %d", e.ID, since)
		}
	}
}

func done(evs []Event) bool {
	for _, e := range evs {
		if e.Flags&HistoryDone != 0 {
			return true
		}
	}
	return false
}

func TestWatchRejects(t *testing.T) {
	sys := native(t)
	if _, err := sys.Watch(nil, 0); err == nil {
		t.Error("no dirs accepted")
	}
	if _, err := sys.Watch([]string{"relative"}, 0); err == nil {
		t.Error("relative dir accepted")
	}
}
