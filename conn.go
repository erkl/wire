package wire

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erkl/xo"
)

const bufferSize = 8 * 1024

// Global buffer pool.
var buffers = &sync.Pool{
	New: func() interface{} {
		return make([]byte, 2*bufferSize)
	},
}

type conn struct {
	// Buffered I/O primitives.
	xo.Reader
	xo.Writer

	// Buffer used for this conn's xo.Reader and xo.Writer instances.
	buf []byte

	// The actual connection.
	raw net.Conn

	// Owning Transport.
	t *Transport

	// Variable used for atomic operations when synchronizing
	// connection shutdown.
	state uint32

	// Connection identifiers.
	tls  bool
	addr string

	// How long has this connection been idle?
	idleSince time.Time

	// Linked list pointer.
	next *conn
}

func (c *conn) maybeClose(reuse bool) {
	const halfReused = 1
	const halfClosed = 2

	// As far as the caller is concerned, can the connection be kept alive
	// and reused for another round-trip?
	var next uint32

	if reuse {
		next = halfReused
	} else {
		next = halfClosed
	}

	// Use an atomic swap to make sure we only close the connection once.
	prev := atomic.SwapUint32(&c.state, next)

	// Don't do anything unless we're the latter of the two "ends" (reading
	// and writing) to finish with the connection.
	if prev == 0 {
		return
	}

	// Either reuse or close the connection.
	if reuse && prev == halfReused {
		c.raw.SetReadDeadline(time.Time{})
		c.state = 0
		c.t.putIdle(c)
	} else {
		c.Close()
	}
}

func (c *conn) Close() error {
	// Allow the connection's buffer to be reused.
	buffers.Put(c.buf)

	c.raw.Close()
	return nil
}

func newConn(raw net.Conn, t *Transport, tls bool, addr string) *conn {
	buf := buffers.Get().([]byte)

	return &conn{
		Reader: xo.NewReader(raw, buf[:bufferSize]),
		Writer: xo.NewWriter(raw, buf[bufferSize:]),
		raw:    raw,
		buf:    buf,
		t:      t,
		tls:    tls,
		addr:   addr,
	}
}
