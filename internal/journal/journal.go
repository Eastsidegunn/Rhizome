package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"rhizome/internal/events"
	"sync"
	"sync/atomic"
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
	poisoned atomic.Bool
	// OnPoison is invoked exactly once, while mu is held, when a file Write or
	// Sync first poisons the journal. The callback must not call List or All.
	OnPoison func(cause error)
}

var _ events.Port = (*Journal)(nil)
var _ events.PoisonLatch = (*Journal)(nil)

func Open(path string) (*Journal, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
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
		if er = s.AppendRevision(e); er != nil {
			f.Close()
			return nil, er
		}
		seq++
	}
	return &Journal{file: f, store: s}, nil
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
	if j.Poisoned() {
		return events.ErrPoisoned
	}
	if j.file == nil {
		return fmt.Errorf("closed journal")
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
