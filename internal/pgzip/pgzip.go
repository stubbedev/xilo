package pgzip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"runtime"
	"sync"

	"github.com/klauspost/compress/flate"
)

const (
	level     = flate.BestSpeed
	blockSize = 1 << 20
	dictSize  = 32 << 10
)

var errClosed = errors.New("pgzip: write to closed writer")

var header = [10]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 4, 255}

type block struct {
	in   []byte
	dict []byte
	out  bytes.Buffer
	last bool
	err  error
	done chan struct{}
}

var blockPool = sync.Pool{New: func() any {
	return &block{in: make([]byte, 0, blockSize), dict: make([]byte, 0, dictSize)}
}}

var flatePool = sync.Pool{New: func() any {
	fw, err := flate.NewWriter(nil, level)
	if err != nil {
		panic(err)
	}
	return fw
}}

func (z *Writer) newBlock(prev *block) *block {
	var b *block
	if n := len(z.free); n > 0 {
		b, z.free = z.free[n-1], z.free[:n-1]
	} else {
		b = blockPool.Get().(*block)
	}
	b.in = b.in[:0]
	b.dict = b.dict[:0]
	if prev != nil {
		b.dict = append(b.dict, prev.in[max(len(prev.in)-dictSize, 0):]...)
	}
	b.out.Reset()
	b.last = false
	b.err = nil
	b.done = make(chan struct{})
	return b
}

func (b *block) compress() {
	defer close(b.done)
	fw := flatePool.Get().(*flate.Writer)
	defer flatePool.Put(fw)
	fw.ResetDict(&b.out, b.dict)
	if _, err := fw.Write(b.in); err != nil {
		b.err = fmt.Errorf("pgzip: deflate %d-byte block: %w", len(b.in), err)
		return
	}
	end := fw.Flush
	if b.last {
		end = fw.Close
	}
	if err := end(); err != nil {
		b.err = fmt.Errorf("pgzip: finish deflate block: %w", err)
	}
}

type Writer struct {
	w       io.Writer
	workers int
	cur     *block
	pending []*block
	free    []*block
	crc     uint32
	size    uint32
	closed  bool
	err     error
}

func NewWriter(w io.Writer) *Writer {
	return newWriter(w, runtime.GOMAXPROCS(0))
}

func newWriter(w io.Writer, workers int) *Writer {
	z := &Writer{w: w, workers: workers, pending: make([]*block, 0, workers), free: make([]*block, 0, workers+1)}
	z.cur = z.newBlock(nil)
	z.cur.out.Write(header[:])
	return z
}

func (z *Writer) Write(p []byte) (int, error) {
	if z.closed {
		return 0, errClosed
	}
	if z.err != nil {
		return 0, z.err
	}
	z.crc = crc32.Update(z.crc, crc32.IEEETable, p)
	z.size += uint32(len(p))
	n := len(p)
	for len(p) > 0 && z.err == nil {
		k := min(blockSize-len(z.cur.in), len(p))
		z.cur.in = append(z.cur.in, p[:k]...)
		p = p[k:]
		if len(z.cur.in) == blockSize {
			z.submit(z.cur)
			z.cur = z.newBlock(z.cur)
		}
	}
	return n, z.err
}

func (z *Writer) submit(b *block) {
	if len(z.pending) == z.workers {
		z.drainOldest()
	}
	z.pending = append(z.pending, b)
	go b.compress()
}

func (z *Writer) drainOldest() {
	b := z.pending[0]
	z.pending = append(z.pending[:0], z.pending[1:]...)
	<-b.done
	defer func() { z.free = append(z.free, b) }()
	if z.err != nil {
		return
	}
	if b.err != nil {
		z.err = b.err
		return
	}
	if _, err := z.w.Write(b.out.Bytes()); err != nil {
		z.err = fmt.Errorf("pgzip: write %d compressed bytes: %w", b.out.Len(), err)
	}
}

func (z *Writer) Close() error {
	if z.closed {
		return z.err
	}
	z.closed = true
	z.cur.last = true
	z.submit(z.cur)
	z.cur = nil
	for len(z.pending) > 0 {
		z.drainOldest()
	}
	for _, b := range z.free {
		blockPool.Put(b)
	}
	z.free = nil
	if z.err != nil {
		return z.err
	}
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[:4], z.crc)
	binary.LittleEndian.PutUint32(trailer[4:], z.size)
	if _, err := z.w.Write(trailer[:]); err != nil {
		z.err = fmt.Errorf("pgzip: write gzip trailer: %w", err)
	}
	return z.err
}
