package errs

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// Location is the origin of an error: where errs.New / errs.Wrap was first called.
type Location struct {
	Function string
	File     string // relative to the module root, slash separated
	Line     int
}

// moduleRoot is derived from this file's own path (…/internal/platform/errs/location.go).
var moduleRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	file = filepath.ToSlash(file)
	return strings.TrimSuffix(file, "internal/platform/errs/location.go")
}()

// caller captures the location skip frames above the caller of caller().
func caller(skip int) Location {
	pc, file, line, ok := runtime.Caller(skip + 1)
	if !ok {
		return Location{}
	}
	loc := Location{File: strings.TrimPrefix(filepath.ToSlash(file), moduleRoot), Line: line}
	if fn := runtime.FuncForPC(pc); fn != nil {
		loc.Function = fn.Name()
	}
	return loc
}

// LocationOf returns the origin location recorded by the innermost errs.New/Wrap
// in err's chain.
func LocationOf(err error) (Location, bool) {
	var found Location
	ok := false
	for ; err != nil; err = errors.Unwrap(err) {
		var e *Error
		if errors.As(err, &e) && e.hasLoc {
			found, ok = e.loc, true
		}
	}
	return found, ok
}
