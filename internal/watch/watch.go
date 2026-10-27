// Package watch reports file system changes under a set of directories.
// On macOS it uses FSEvents, whose event IDs let a restarted daemon replay
// only what changed while it was down (see Native).
package watch

import (
	"fmt"
	"strings"
	"sync"
)

// Flags describe an event; the values are FSEvents' own.
type Flags uint32

const (
	MustScanSubDirs Flags = 0x00000001 // coalesced: rescan below Path
	UserDropped     Flags = 0x00000002 // events were lost: rescan everything
	KernelDropped   Flags = 0x00000004
	IDsWrapped      Flags = 0x00000008
	HistoryDone     Flags = 0x00000010 // replay of past events is complete
	RootChanged     Flags = 0x00000020 // a watched root moved or vanished
	Mount           Flags = 0x00000040
	Unmount         Flags = 0x00000080
	Created         Flags = 0x00000100
	Removed         Flags = 0x00000200
	InodeMetaMod    Flags = 0x00000400
	Renamed         Flags = 0x00000800
	Modified        Flags = 0x00001000
	FinderInfoMod   Flags = 0x00002000
	ChangeOwner     Flags = 0x00004000
	XattrMod        Flags = 0x00008000
	IsFile          Flags = 0x00010000
	IsDir           Flags = 0x00020000
	IsSymlink       Flags = 0x00040000
	Cloned          Flags = 0x00400000
)

// Lost reports that events were dropped and the whole tree needs a rescan.
func (f Flags) Lost() bool { return f&(UserDropped|KernelDropped|IDsWrapped) != 0 }

func (f Flags) String() string {
	names := []struct {
		f Flags
		s string
	}{
		{MustScanSubDirs, "MustScanSubDirs"}, {UserDropped, "UserDropped"}, {KernelDropped, "KernelDropped"},
		{IDsWrapped, "IDsWrapped"}, {HistoryDone, "HistoryDone"}, {RootChanged, "RootChanged"},
		{Mount, "Mount"}, {Unmount, "Unmount"}, {Created, "Created"}, {Removed, "Removed"},
		{InodeMetaMod, "InodeMetaMod"}, {Renamed, "Renamed"}, {Modified, "Modified"},
		{FinderInfoMod, "FinderInfoMod"}, {ChangeOwner, "ChangeOwner"}, {XattrMod, "XattrMod"},
		{IsFile, "IsFile"}, {IsDir, "IsDir"}, {IsSymlink, "IsSymlink"}, {Cloned, "Cloned"},
	}
	var out []string
	for _, n := range names {
		if f&n.f != 0 {
			out = append(out, n.s)
			f &^= n.f
		}
	}
	if f != 0 {
		out = append(out, fmt.Sprintf("%#x", uint32(f)))
	}
	return strings.Join(out, "|")
}

// Event is one change. ID orders events host-wide.
type Event struct {
	Path  string
	Flags Flags
	ID    uint64
}

// Stream delivers events for the directories it was started on.
type Stream interface {
	// Ready is signalled when Take has something to return.
	Ready() <-chan struct{}
	// Take returns the events received since the last call. overflow
	// reports that events were discarded because nobody took them in time;
	// the caller must then rescan everything.
	Take() (events []Event, overflow bool)
	Close() error
}

// System is the platform's change notification service.
type System interface {
	// Watch starts a stream over dirs (absolute, symlink-free) delivering
	// events after since; Now() means "from here on".
	Watch(dirs []string, since uint64) (Stream, error)
	// Now returns the latest event ID issued so far.
	Now() uint64
	// History identifies the event history of the volume holding path. A
	// saved event ID can only be replayed while this stays the same.
	History(path string) (string, error)
}

// buffer is the bounded hand-off from the notification thread, which must
// never block, to the consumer.
type buffer struct {
	mu       sync.Mutex
	max      int
	events   []Event
	overflow bool
	ready    chan struct{}
}

func newBuffer(max int) *buffer {
	if max < 1 {
		panic("watch: buffer size must be ≥ 1")
	}
	return &buffer{max: max, ready: make(chan struct{}, 1)}
}

// add never blocks: past max, events are dropped and overflow is flagged.
func (b *buffer) add(evs []Event) {
	b.mu.Lock()
	if room := b.max - len(b.events); len(evs) > room {
		evs = evs[:max(0, room)]
		b.overflow = true
	}
	b.events = append(b.events, evs...)
	b.mu.Unlock()
	select {
	case b.ready <- struct{}{}:
	default:
	}
}

func (b *buffer) Ready() <-chan struct{} { return b.ready }

func (b *buffer) Take() ([]Event, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	evs, over := b.events, b.overflow
	b.events, b.overflow = nil, false
	return evs, over
}
