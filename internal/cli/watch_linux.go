package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func watchStore(ctx context.Context, storeDir string, onValid func(path string), watching func()) (err error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify_init: %w", err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, storeDir, syscall.IN_DELETE); err != nil {
		return fmt.Errorf("inotify_add_watch %s: %w", storeDir, err)
	}
	wakeFd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return fmt.Errorf("eventfd: %w", err)
	}
	defer syscall.Close(wakeFd)
	ep, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("epoll_create: %w", err)
	}
	defer syscall.Close(ep)
	for _, watched := range []int{fd, wakeFd} {
		if err := syscall.EpollCtl(ep, syscall.EPOLL_CTL_ADD, watched, &syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(watched)}); err != nil {
			return fmt.Errorf("epoll_ctl %s: %w", storeDir, err)
		}
	}
	defer wakeOnDone(ctx, storeDir, func() error {
		var one [8]byte
		binary.NativeEndian.PutUint64(one[:], 1)
		_, err := unix.Write(wakeFd, one[:])
		return err
	})()

	lister, err := openStoreLister(storeDir)
	if err != nil {
		return err
	}
	defer lister.close()
	scan, err := newStoreScan(storeDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, scan.Close()) }()
	if err := lister.list(scan, onValid); err != nil {
		return err
	}
	watching()

	ready := make([]syscall.EpollEvent, 2)
	buf := make([]byte, 64*1024)
	for {
		n, err := syscall.EpollWait(ep, ready, -1)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("epoll_wait %s: %w", storeDir, err)
		}
		for _, ev := range ready[:n] {
			if int(ev.Fd) == wakeFd {
				return nil
			}
		}
		n, err = syscall.Read(fd, buf)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read inotify events for %s: %w", storeDir, err)
		}
		overflowed := false
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameLen := int(ev.Len)
			name := strings.TrimRight(string(buf[off+syscall.SizeofInotifyEvent:off+syscall.SizeofInotifyEvent+nameLen]), "\x00")
			off += syscall.SizeofInotifyEvent + nameLen
			if ev.Mask&syscall.IN_Q_OVERFLOW != 0 {
				overflowed = true
				continue
			}
			base, ok := strings.CutSuffix(name, lockSuffix)
			if !ok {
				continue
			}
			if err := scan.saw(base); err != nil {
				return err
			}
			if path, ok := storePath(storeDir, base); ok {
				onValid(path)
			}
		}
		if overflowed {
			fmt.Fprintf(os.Stderr, "xilo watch: inotify queue for %s overflowed; rescanning the store for what it dropped\n", storeDir)
			if err := lister.list(scan, onValid); err != nil {
				return err
			}
		}
	}
}

func direntNameLen(rec []byte) int {
	return bytes.IndexByte(rec[direntName:], 0)
}

func (l *storeLister) each(fn func(name []byte) error) error {
	if err := l.rewind(); err != nil {
		return err
	}
	for {
		n, err := syscall.ReadDirent(l.fd, l.buf)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("list %s: %w", l.dir, err)
		}
		if n == 0 {
			return nil
		}
		if err := l.entries(l.buf[:n], fn); err != nil {
			return err
		}
	}
}
