//go:build !linux

package storage

import "net"

func promptAcks(c net.Conn) net.Conn { return c }
