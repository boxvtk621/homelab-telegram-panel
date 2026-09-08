//go:build windows

package main

// Windows is not a supported serve target. Returning an unknown identity
// keeps serve fail-closed while validate and version remain usable.
func currentEffectiveUID() int { return -1 }
