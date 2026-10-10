//go:build !unix

package secretplane

import "os"

func ownedBySelf(os.FileInfo) bool { return false }

func lockDir(*os.File) error { return ErrUnsupported }

const oNoFollow = 0

func ownerUID(os.FileInfo) (uint32, bool) { return 0, false }
