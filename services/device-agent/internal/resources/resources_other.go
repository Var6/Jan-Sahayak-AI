//go:build !linux && !darwin

package resources

import (
	"context"
	"errors"
)

func procRSS(context.Context) (uint64, string)          { return 0, "" }
func sysMemory() (total, avail uint64)                  { return 0, 0 }
func cpuTemp() (float64, bool, error)                   { return 0, false, nil }
func throttled(context.Context) (Throttle, bool, error) { return Throttle{}, false, nil }
func statfs(string) (uint64, uint64, error)             { return 0, 0, errors.New("statfs not supported") }
