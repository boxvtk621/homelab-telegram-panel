//go:build !windows

package main

import "os"

func currentEffectiveUID() int { return os.Geteuid() }
