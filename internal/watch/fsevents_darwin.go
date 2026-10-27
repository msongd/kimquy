package watch

/*
#cgo LDFLAGS: -framework CoreServices
#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdint.h>
#include <stdlib.h>
#include <sys/stat.h>

extern void semdFSEvents(uintptr_t h, size_t n, char **paths, FSEventStreamEventFlags *flags, FSEventStreamEventId *ids);

static void semdCallback(ConstFSEventStreamRef s, void *info, size_t n, void *paths,
		const FSEventStreamEventFlags flags[], const FSEventStreamEventId ids[]) {
	semdFSEvents((uintptr_t)info, n, (char **)paths, (FSEventStreamEventFlags *)flags, (FSEventStreamEventId *)ids);
}

// semdStart returns NULL on failure. Paths must be UTF-8.
static FSEventStreamRef semdStart(uintptr_t h, char **paths, int n, FSEventStreamEventId since,
		double latency, dispatch_queue_t q) {
	CFMutableArrayRef arr = CFArrayCreateMutable(NULL, n, &kCFTypeArrayCallBacks);
	if (arr == NULL) return NULL;
	for (int i = 0; i < n; i++) {
		CFStringRef s = CFStringCreateWithCString(NULL, paths[i], kCFStringEncodingUTF8);
		if (s == NULL) { CFRelease(arr); return NULL; }
		CFArrayAppendValue(arr, s);
		CFRelease(s);
	}
	FSEventStreamContext ctx = {0, (void *)h, NULL, NULL, NULL};
	FSEventStreamRef st = FSEventStreamCreate(NULL, semdCallback, &ctx, arr, since, latency,
		kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer | kFSEventStreamCreateFlagWatchRoot);
	CFRelease(arr);
	if (st == NULL) return NULL;
	FSEventStreamSetDispatchQueue(st, q);
	if (!FSEventStreamStart(st)) {
		FSEventStreamInvalidate(st);
		FSEventStreamRelease(st);
		return NULL;
	}
	return st;
}

static void semdNoop(void *ctx) {}

// semdStop returns once no callback is running or can run.
static void semdStop(FSEventStreamRef st, dispatch_queue_t q) {
	FSEventStreamStop(st);
	FSEventStreamInvalidate(st);
	FSEventStreamRelease(st);
	dispatch_sync_f(q, NULL, semdNoop);
}

static dispatch_queue_t semdQueue(void) { return dispatch_queue_create("semd.fsevents", DISPATCH_QUEUE_SERIAL); }
static void semdRelease(dispatch_queue_t q) { dispatch_release(q); }

// semdHistory writes the UUID of the event history of device dev into out
// (37 bytes). Returns 0 if the volume keeps no history.
static int semdHistory(dev_t dev, char *out) {
	CFUUIDRef u = FSEventsCopyUUIDForDevice(dev);
	if (u == NULL) return 0;
	CFStringRef s = CFUUIDCreateString(NULL, u);
	CFRelease(u);
	if (s == NULL) return 0;
	Boolean ok = CFStringGetCString(s, out, 37, kCFStringEncodingASCII);
	CFRelease(s);
	return ok ? 1 : 0;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime/cgo"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

// maxBuffered bounds events waiting for the consumer (~100 B each).
const maxBuffered = 100_000

// FSEvents is the macOS System. Latency is how long FSEvents gathers
// events before delivering them.
type FSEvents struct {
	Latency time.Duration
}

// Native returns the platform's System.
func Native() (System, error) { return FSEvents{Latency: 500 * time.Millisecond}, nil }

func (FSEvents) Now() uint64 { return uint64(C.FSEventsGetCurrentEventId()) }

func (FSEvents) History(path string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "", err
	}
	var out [37]C.char
	if C.semdHistory(C.dev_t(st.Dev), &out[0]) == 0 {
		return "", fmt.Errorf("%s: volume keeps no FSEvents history", path)
	}
	return C.GoString(&out[0]), nil
}

type fsStream struct {
	*buffer
	h     cgo.Handle
	q     C.dispatch_queue_t
	ref   C.FSEventStreamRef
	close sync.Once
}

func (f FSEvents) Watch(dirs []string, since uint64) (Stream, error) {
	if len(dirs) == 0 {
		return nil, errors.New("watch: no directories")
	}
	if f.Latency < 0 || f.Latency > time.Minute {
		return nil, fmt.Errorf("watch: latency %v out of range", f.Latency)
	}
	cpaths := make([]*C.char, len(dirs))
	for i, d := range dirs {
		if !filepath.IsAbs(d) || !utf8.ValidString(d) {
			return nil, fmt.Errorf("watch: %q is not an absolute UTF-8 path", d)
		}
		cpaths[i] = C.CString(d)
	}
	defer func() {
		for _, p := range cpaths {
			C.free(unsafe.Pointer(p))
		}
	}()
	s := &fsStream{buffer: newBuffer(maxBuffered), q: C.semdQueue()}
	s.h = cgo.NewHandle(s.buffer)
	cArr := (**C.char)(C.malloc(C.size_t(len(cpaths)) * C.size_t(unsafe.Sizeof(cpaths[0]))))
	defer C.free(unsafe.Pointer(cArr))
	copy(unsafe.Slice(cArr, len(cpaths)), cpaths)
	s.ref = C.semdStart(C.uintptr_t(s.h), cArr, C.int(len(dirs)), C.FSEventStreamEventId(since),
		C.double(f.Latency.Seconds()), s.q)
	if s.ref == nil {
		s.h.Delete()
		C.semdRelease(s.q)
		return nil, fmt.Errorf("watch: cannot start an FSEvents stream on %v", dirs)
	}
	return s, nil
}

func (s *fsStream) Close() error {
	s.close.Do(func() {
		C.semdStop(s.ref, s.q)
		C.semdRelease(s.q)
		s.h.Delete()
	})
	return nil
}

//export semdFSEvents
func semdFSEvents(h C.uintptr_t, n C.size_t, paths **C.char, flags *C.FSEventStreamEventFlags, ids *C.FSEventStreamEventId) {
	b := cgo.Handle(h).Value().(*buffer)
	ps := unsafe.Slice(paths, int(n))
	fs := unsafe.Slice(flags, int(n))
	is := unsafe.Slice(ids, int(n))
	evs := make([]Event, n)
	for i := range evs {
		evs[i] = Event{Path: C.GoString(ps[i]), Flags: Flags(fs[i]), ID: uint64(is[i])}
	}
	b.add(evs)
}
