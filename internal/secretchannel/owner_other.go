//go:build !unix

package secretchannel

import "os"

// ownerUID is unavailable here: W1 then never passes.
func ownerUID(os.FileInfo) (int, bool) { return 0, false }
