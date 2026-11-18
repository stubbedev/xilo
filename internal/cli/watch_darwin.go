package cli

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

const (
	watchSettle    = 100 * time.Millisecond
	watchScanShare = 10
	watchWakeIdent = 1

	direntNamlen = unsafe.Offsetof(syscall.Dirent{}.Namlen)
	direntMax    = int(unsafe.Sizeof(syscall.Dirent{}))
)

func watchStore(ctx context.Context, storeDir string, onValid func(path string), watching func()) (err error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return fmt.Errorf("kqueue: %w", err)
	}
	defer syscall.Close(kq)
	evt, err := syscall.Open(storeDir, syscall.O_EVTONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s for events: %w", storeDir, err)
	}
	defer syscall.Close(evt)
	lister, err := openStoreLister(storeDir)
	if err != nil {
		return err
	}
	defer lister.close()

	changes := make([]syscall.Kevent_t, 2)
	syscall.SetKevent(&changes[0], evt, syscall.EVFILT_VNODE, syscall.EV_ADD|syscall.EV_CLEAR)
	changes[0].Fflags = syscall.NOTE_WRITE
	syscall.SetKevent(&changes[1], watchWakeIdent, syscall.EVFILT_USER, syscall.EV_ADD|syscall.EV_CLEAR)
	if _, err := syscall.Kevent(kq, changes, nil, nil); err != nil {
		return fmt.Errorf("kevent register %s: %w", storeDir, err)
	}
	defer wakeOnDone(ctx, storeDir, func() error {
		var wake syscall.Kevent_t
		syscall.SetKevent(&wake, watchWakeIdent, syscall.EVFILT_USER, 0)
		wake.Fflags = syscall.NOTE_TRIGGER
		_, err := syscall.Kevent(kq, []syscall.Kevent_t{wake}, nil, nil)
		return err
	})()

	scan, err := newStoreScan(storeDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, scan.Close()) }()
	if err := lister.list(scan, onValid); err != nil {
		return err
	}
	watching()

	events := make([]syscall.Kevent_t, 2)
	settle := watchSettle
	for {
		n, err := syscall.Kevent(kq, nil, events, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("kevent wait on %s: %w", storeDir, err)
		}
		for _, ev := range events[:n] {
			if ev.Filter == syscall.EVFILT_USER {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(settle):
		}
		start := time.Now()
		if err := lister.list(scan, onValid); err != nil {
			return err
		}
		settle = max(watchSettle, watchScanShare*time.Since(start))
	}
}

func direntNameLen(rec []byte) int {
	return int(binary.NativeEndian.Uint16(rec[direntNamlen:]))
}

func (l *storeLister) each(fn func(name []byte) error) error {
	for {
		if err := l.rewind(); err != nil {
			return err
		}
		var base uintptr
		n, err := syscall.Getdirentries(l.fd, l.buf, &base)
		if err != nil {
			return fmt.Errorf("list %s: %w", l.dir, err)
		}
		if len(l.buf)-n >= direntMax {
			return l.entries(l.buf[:n], fn)
		}
		l.buf = make([]byte, 2*len(l.buf))
	}
}
