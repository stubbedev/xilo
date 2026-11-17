package cli

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

func watchStore(ctx context.Context, storeDir string, onValid func(path string)) error {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("inotify_init: %w", err)
	}
	defer syscall.Close(fd)
	// Watch for the removal of lock files (a store path becoming valid).
	if _, err := syscall.InotifyAddWatch(fd, storeDir, syscall.IN_DELETE); err != nil {
		return fmt.Errorf("inotify_add_watch %s: %w", storeDir, err)
	}

	go func() { <-ctx.Done(); syscall.Close(fd) }()

	buf := make([]byte, 64*1024)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameLen := int(ev.Len)
			nameBytes := buf[off+syscall.SizeofInotifyEvent : off+syscall.SizeofInotifyEvent+nameLen]
			name := strings.TrimRight(string(nameBytes), "\x00")
			if path, ok := lockToStorePath(storeDir, name); ok {
				onValid(path)
			}
			off += syscall.SizeofInotifyEvent + nameLen
		}
	}
}
