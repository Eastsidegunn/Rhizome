//go:build darwin

package peercred_test

import (
	"os"
	"testing"

	"rhizome/internal/peercred"
)

func TestVerifyPIDFRRHZ130(t *testing.T) {
	server := connectedUnixConn(t)
	got, err := peercred.Verify(server)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID() != 0 && got.PID() != int32(os.Getpid()) {
		t.Fatalf("PID = %d, want 0 or %d", got.PID(), os.Getpid())
	}
}
