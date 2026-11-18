package storage

import (
	"net"
	"syscall"
)

func promptAcks(c net.Conn) net.Conn {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return c
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return c
	}
	return &promptAckConn{TCPConn: tcp, raw: raw}
}

type promptAckConn struct {
	*net.TCPConn
	raw syscall.RawConn
}

func (c *promptAckConn) Read(b []byte) (int, error) {
	n, err := c.TCPConn.Read(b)
	if n > 0 {
		_ = c.raw.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)
		})
	}
	return n, err
}
