package main

// RHZ-093 (FR-RHZ-119): -janus-idle-timeout flag — default 10m,
// 0 = off, negative rejected, wired into the loop only with a JANUS config.

import (
	"bytes"
	"flag"
	"path/filepath"
	"testing"
	"time"

	"rhizome/internal/events"
)

func TestServeFlagsIdleTimeoutDefaultFRRHZ119(t *testing.T) {
	if defaultJanusIdleTimeout != 10*time.Minute {
		t.Fatalf("default: %s", defaultJanusIdleTimeout)
	}
	// The flag parses like serve declares it: default 10m, 0 and durations accepted.
	for args, want := range map[string]time.Duration{"": 10 * time.Minute, "-janus-idle-timeout=0": 0, "-janus-idle-timeout=90s": 90 * time.Second} {
		f := flag.NewFlagSet("serve", flag.ContinueOnError)
		d := f.Duration("janus-idle-timeout", defaultJanusIdleTimeout, "")
		var argv []string
		if args != "" {
			argv = []string{args}
		}
		if err := f.Parse(argv); err != nil {
			t.Fatal(err)
		}
		got, err := janusIdleTimeout(*d)
		if err != nil || got != want {
			t.Fatalf("%q: %s %v", args, got, err)
		}
	}
	if _, err := janusIdleTimeout(-time.Second); err == nil {
		t.Fatal("negative accepted")
	}
	// serve itself rejects a negative value as a configuration error (exit 2).
	var eout bytes.Buffer
	if code := serve([]string{"-journal", filepath.Join(t.TempDir(), "j.ndjson"), "-janus-idle-timeout=-1s"}, &bytes.Buffer{}, &eout); code != 2 || !bytes.Contains(eout.Bytes(), []byte("janus-idle-timeout")) {
		t.Fatalf("exit %d: %s", code, eout.String())
	}
}

func TestServeIdleTimeoutWiredIntoLoopFRRHZ119(t *testing.T) {
	s := &events.Store{}
	jc, err := janusServeFromFlags(fakeHX(t), filepath.Join(t.TempDir(), "absent.sock"), "/p.yaml", "/ar", "/w.json", "", defaultJanusEnvMode, false, "", 5*time.Second)
	if err != nil || jc == nil {
		t.Fatal(jc, err)
	}
	jc.IdleTimeout = 7 * time.Minute
	_, loop := assembleServe(s, nil, "", "", jc, &bytes.Buffer{}, false)
	if loop == nil || loop.IdleTimeout != 7*time.Minute || loop.Now != nil {
		t.Fatalf("loop wiring: %+v", loop)
	}
	jc.IdleTimeout = 0
	if _, loop = assembleServe(s, nil, "", "", jc, &bytes.Buffer{}, false); loop.IdleTimeout != 0 {
		t.Fatal("0 must stay 0 (off)")
	}
}
