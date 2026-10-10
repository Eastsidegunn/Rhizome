//go:build !unix

package server

import "os"

func ownedBySelf(os.FileInfo) bool { return false }

func lockSocketDir(string) (*os.File, error) { return nil, ErrListen }
