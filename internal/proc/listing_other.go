//go:build !darwin

package proc

import "errors"

// sysListing reads the process table in-process on macOS only; elsewhere
// listing runs ps.
var sysListing = func() ([]psRow, error) { return nil, errNoSysListing }

var errNoSysListing = errors.New("no in-process listing here")
