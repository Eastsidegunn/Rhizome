package blob

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreFRRHZ055(t *testing.T) {
	d := t.TempDir()
	s := FileStore{Dir: d}
	id, e := s.Put([]byte("hello"))
	if e != nil {
		t.Fatal(e)
	}
	id2, e := s.Put([]byte("hello"))
	if e != nil || id != id2 {
		t.Fatal(id, id2, e)
	}
	if b, e := s.Get(id); e != nil || string(b) != "hello" {
		t.Fatal(string(b), e)
	}
	if _, e := s.Put(nil); e == nil {
		t.Fatal("empty accepted")
	}
	h := id[len("sha256:"):]
	if e = os.WriteFile(filepath.Join(d, h), []byte("bad"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Get(id); e == nil {
		t.Fatal("corrupt accepted")
	}
}
