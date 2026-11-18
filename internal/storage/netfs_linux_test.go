package storage

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestNetworkMagic(t *testing.T) {
	for _, c := range []struct {
		name  string
		magic uint32
		want  bool
	}{
		{"cifs", cifsMagic, true},
		{"smb2", smb2Magic, true},
		{"nfs", unix.NFS_SUPER_MAGIC, true},
		{"fuse", unix.FUSE_SUPER_MAGIC, true},
		{"ceph", unix.CEPH_SUPER_MAGIC, true},
		{"ext4", unix.EXT4_SUPER_MAGIC, false},
		{"xfs", unix.XFS_SUPER_MAGIC, false},
		{"btrfs", unix.BTRFS_SUPER_MAGIC, false},
		{"zfs", 0x2fc12fc1, false},
		{"tmpfs", unix.TMPFS_MAGIC, false},
		{"overlay", unix.OVERLAYFS_SUPER_MAGIC, false},
	} {
		if got := networkMagic(c.magic); got != c.want {
			t.Errorf("networkMagic(%s) = %v want %v", c.name, got, c.want)
		}
	}
}
