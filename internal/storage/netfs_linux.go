package storage

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	cifsMagic = 0xFF534D42
	smb2Magic = 0xFE534D42
)

func onNetworkFS(dir string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false, fmt.Errorf("statfs: %w", err)
	}
	return networkMagic(uint32(st.Type)), nil
}

// networkMagic reports whether a statfs f_type names a filesystem whose reads
// cross a network. FUSE counts: sshfs, rclone, s3fs and the like are its
// common use as a chunk root, and a deep window over a local FUSE mount costs
// only memory.
func networkMagic(magic uint32) bool {
	switch magic {
	case cifsMagic, smb2Magic, unix.SMB_SUPER_MAGIC, unix.NFS_SUPER_MAGIC,
		unix.CEPH_SUPER_MAGIC, unix.AFS_SUPER_MAGIC, unix.AFS_FS_MAGIC,
		unix.CODA_SUPER_MAGIC, unix.V9FS_MAGIC, unix.FUSE_SUPER_MAGIC:
		return true
	}
	return false
}
