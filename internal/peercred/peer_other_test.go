//go:build !linux && !darwin

package peercred_test

import (
	"errors"
	"testing"

	"rhizome/internal/peercred"
)

func TestVerifyUnsupportedFRRHZ130(t *testing.T) {
	if _, err := peercred.Verify(nil); !errors.Is(err, peercred.ErrUnsupported) {
		t.Fatalf("Verify error = %v, want ErrUnsupported", err)
	}
	if (peercred.Verified{}).SameUser() {
		t.Fatal("unsupported zero value was accepted")
	}
}
