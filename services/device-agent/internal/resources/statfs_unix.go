//go:build linux || darwin

package resources

import "syscall"

func statfs(path string) (total, free uint64, err error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0, err
	}
	bs := uint64(s.Bsize) //nolint:gosec // block size is positive
	return s.Blocks * bs, s.Bavail * bs, nil
}
