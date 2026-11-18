package storage

import (
	"io"
	"net"
	"syscall"
	"testing"
)

func TestPromptAcksIsSetAgainAfterEveryRead(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("one"))
		_, _ = io.Copy(io.Discard, c)
	}()

	dialed, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dialed.Close()
	raw, err := dialed.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	quickAck := func() int {
		t.Helper()
		var v int
		var serr error
		if err := raw.Control(func(fd uintptr) {
			v, serr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_QUICKACK)
		}); err != nil || serr != nil {
			t.Fatal(err, serr)
		}
		return v
	}
	clear := func() {
		t.Helper()
		var serr error
		if err := raw.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 0)
		}); err != nil || serr != nil {
			t.Fatal(err, serr)
		}
	}

	c := promptAcks(dialed)
	clear()
	if quickAck() != 0 {
		t.Fatal("could not clear TCP_QUICKACK, so the test proves nothing")
	}
	if _, err := io.ReadFull(c, make([]byte, 3)); err != nil {
		t.Fatal(err)
	}
	if quickAck() != 1 {
		t.Fatal("TCP_QUICKACK not set after a read")
	}
}
