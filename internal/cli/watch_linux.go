package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

const watchPollMillis = 500

func watchStore(ctx context.Context, storeDir string, onValid func(path string)) error {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify_init: %w", err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, storeDir, syscall.IN_DELETE); err != nil {
		return fmt.Errorf("inotify_add_watch %s: %w", storeDir, err)
	}
	ep, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("epoll_create: %w", err)
	}
	defer syscall.Close(ep)
	if err := syscall.EpollCtl(ep, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}); err != nil {
		return fmt.Errorf("epoll_ctl %s: %w", storeDir, err)
	}

	ready := make([]syscall.EpollEvent, 1)
	buf := make([]byte, 64*1024)
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, err := syscall.EpollWait(ep, ready, watchPollMillis)
		if errors.Is(err, syscall.EINTR) || n == 0 {
			continue
		}
		if err != nil {
			return fmt.Errorf("epoll_wait %s: %w", storeDir, err)
		}
		n, err = syscall.Read(fd, buf)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read inotify events for %s: %w", storeDir, err)
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			if ev.Mask&syscall.IN_Q_OVERFLOW != 0 {
				return fmt.Errorf("inotify queue for %s overflowed: store paths built meanwhile were not seen and must be pushed with `xilo push`", storeDir)
			}
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
