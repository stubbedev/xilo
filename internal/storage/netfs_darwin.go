package storage

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func onNetworkFS(dir string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false, fmt.Errorf("statfs: %w", err)
	}
	return st.Flags&unix.MNT_LOCAL == 0, nil
}
