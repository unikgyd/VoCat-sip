//go:build !linux

package hostims

import "io"

func lockClaimAt(string, string) (io.Closer, error) { return nil, ErrJournal }
