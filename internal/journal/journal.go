package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"rhizome/internal/events"
	"sync"
	"sync/atomic"
	"time"
)

type journalFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type Journal struct {
	mu       sync.RWMutex
	file     journalFile
	store    *events.Store
	guard    events.Guard
	readOnly bool
	poisoned atomic.Bool
	// OnPoison is invoked exactly once, while mu is held, when a file Write or
	// Sync first poisons the journal. The callback must not call List or All.
	OnPoison func(cause error)
}

var _ events.Port = (*Journal)(nil)
var _ events.PoisonLatch = (*Journal)(nil)

func OpenGuarded(path string, guard events.Guard) (*Journal, error) {
	return open(path, guard, false)
}

func OpenReadOnly(path string, guard events.Guard) (*Journal, error) {
	return open(path, guard, true)
}

func open(path string, guard events.Guard, readOnly bool) (*Journal, error) {
	if guard == nil {
		return nil, fmt.Errorf("nil journal guard")
	}
	flags := os.O_RDWR | os.O_CREATE | os.O_APPEND
	if readOnly {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	s := &events.Store{}
	r := bufio.NewReader(f)
	var seq uint64
	for {
		line, er := r.ReadBytes('\n')
		if er == io.EOF {
			if len(line) != 0 {
				f.Close()
				return nil, fmt.Errorf("journal truncated line")
			}
			break
		}
		if er != nil {
			f.Close()
			return nil, er
		}
		var e events.Event
		if er = json.Unmarshal(line, &e); er != nil {
			f.Close()
			return nil, fmt.Errorf("decode journal: %w", er)
		}
		if e.Sequence != seq+1 {
			f.Close()
			return nil, fmt.Errorf("sequence gap")
		}
		if er = guard.CheckReplay(s, e); er != nil {
			f.Close()
			return nil, er
		}
		// RHZ-133 (FR-RHZ-173): a line written before Append stamped the
		// envelope time carries the zero time on disk. Restore it as the Unix
		// epoch so the store does not re-stamp it with the load time: replay
		// stays deterministic and the projection reads "unknown" (ms <= 0).
		// Readers of the envelope time must treat ms <= 0 as unknown.
		if e.CreatedAt.IsZero() {
			e.CreatedAt = time.Unix(0, 0).UTC()
		}
		if er = s.AppendRevision(e); er != nil {
			f.Close()
			return nil, er
		}
		seq++
	}
	return &Journal{file: f, store: s, guard: guard, readOnly: readOnly}, nil
}

func (j *Journal) Poisoned() bool {
	return j != nil && j.poisoned.Load()
}

func (j *Journal) poison(cause error) error {
	if j.poisoned.CompareAndSwap(false, true) && j.OnPoison != nil {
		j.OnPoison(cause)
	}
	return events.ErrPoisoned
}

func (j *Journal) Append(expected uint64, e events.Event) error {
	if j == nil {
		return fmt.Errorf("closed journal")
	}
	if j.Poisoned() {
		return events.ErrPoisoned
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	e.Payload = bytes.Clone(e.Payload)
	if j.Poisoned() {
		return events.ErrPoisoned
	}
	if j.file == nil {
		return fmt.Errorf("closed journal")
	}
	if j.readOnly {
		return fmt.Errorf("read-only journal")
	}
	if e.Revision != expected+1 {
		return events.ErrRevisionConflict
	}
	// Index lookups (RHZ-054). Revision returns 0 when the aggregate has no
	// events, so this one check covers both the "exists but wrong revision" and
	// the "no events yet but expected != 0" rejections of the prior List scan.
	if j.store.Revision(e.AggregateType, e.AggregateID) != expected {
		return events.ErrRevisionConflict
	}
	e.Sequence = uint64(j.store.Len() + 1)
	now := time.Now().UTC()
	// RHZ-133 (FR-RHZ-173): stamp the envelope time BEFORE the line is
	// written. Previously the store stamped it after the write, so the
	// in-memory event and the durable line disagreed (zero time on disk) and
	// every reopen re-stamped it with the load time.
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	if err := j.guard.CheckAppend(j.store, e, now); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err = j.file.Write(append(raw, '\n')); err != nil {
		return j.poison(err)
	}
	if err = j.file.Sync(); err != nil {
		return j.poison(err)
	}
	return j.store.Append(expected, e)
}
func (j *Journal) List(t, id string) []events.Event {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.store.List(t, id)
}
func (j *Journal) All() []events.Event { j.mu.RLock(); defer j.mu.RUnlock(); return j.store.All() }
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j == nil || j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}
