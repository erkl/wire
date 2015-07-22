package wire

import (
	"net"
	"sync"
	"sync/atomic"

	"github.com/erkl/xo"
)

const bufferSize = 8 * 1024

// Global pool of buffers.
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
}

const (
	stateKeepAlive = 1
	stateClose     = 2
)

func (c *conn) maybeClose(isKeepAlive bool) {
	// As far as the caller is concerned, can the connection be kept alive
	// and reused for another round-trip?
	var next uint32

	if isKeepAlive {
		next = stateKeepAlive
	} else {
		next = stateClose
	}

	// Use an atomic swap to determine whether we're done (both reading and
	// writing) with this connection.
	prev := atomic.SwapUint32(&c.state, next)

	if prev == stateKeepAlive && isKeepAlive {
		// TODO: Recycle the connection.
		atomic.StoreUint32(&c.state, 0)
		c.Close()
	} else if prev != 0 {
		c.Close()
	}
}

func (c *conn) Close() error {
	// Allow the connection's buffer to be reused.
	buffers.Put(c.buf)

	c.raw.Close()
	return nil
}

func newConn(raw net.Conn, t *Transport) *conn {
	buf := buffers.Get().([]byte)

	return &conn{
		Reader: xo.NewReader(raw, buf[:bufferSize]),
		Writer: xo.NewWriter(raw, buf[bufferSize:]),
		raw:    raw,
		buf:    buf,
		t:      t,
	}
}
