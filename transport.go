package wire

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erkl/heat"
)

var ErrUnsupportedScheme = errors.New("unsupported scheme in request")
var ErrNilCancel = errors.New("round-trip cancelled with nil error")

type Transport struct {
	// Dial specifies the function used to establish plain TCP connections
	// with remote hosts.
	Dial func(addr string) (net.Conn, error)

	// DialTLS specifies the function used to establish TLS connections with
	// remote hosts.
	DialTLS func(addr string) (net.Conn, error)

	// KeepAliveTimeout specifies how long keep-alive connections should be
	// allowed to sit idle before being automatically terminated.
	KeepAliveTimeout time.Duration

	// Mutex protecting internal fields.
	mu sync.Mutex

	// Idle TCP and TLS connections. Keyed by hostname and stored in simple
	// singly-linked lists, the most recently used connection first.
	idleTCP map[string]*conn
	idleTLS map[string]*conn

	// True if the goroutine responsible for reaping old idle connections
	// is currently running.
	cleaning bool
}

func (t *Transport) RoundTrip(req *heat.Request, cancel <-chan error) (*heat.Response, error) {
	if req.Body != nil {
		defer req.Body.Close()
	}

	// Validate the request body size.
	wsize, err := heat.RequestBodySize(req)
	if err != nil {
		return nil, err
	}

	// Only make the round-trip cancellable (by doing the work in a separate
	// goroutine) if we were actually provided a cancel channel.
	if cancel != nil {
		return t.roundTripCancel(req, wsize, cancel)
	}

	// Establish a connection.
	c, err := t.dial(req.Scheme, req.Remote)
	if err != nil {
		return nil, err
	}

	// Issue the request and read the response.
	resp, err := roundTrip(c, req, wsize)
	if err != nil {
		c.Close()
		return nil, err
	}

	return resp, err
}

type baton struct {
	c *conn
	r *heat.Response
	e error
}

func (t *Transport) roundTripCancel(req *heat.Request, wsize heat.BodySize, cancel <-chan error) (*heat.Response, error) {
	var ch = make(chan baton, 1)
	var syn uint32
	var c *conn

	// Establish a connection.
	go func() {
		c, err := t.dial(req.Scheme, req.Remote)
		if atomic.CompareAndSwapUint32(&syn, 0, 1) {
			ch <- baton{c: c, e: err}
		} else if err == nil {
			t.putIdle(c)
		}
	}()

	// Wait for the connection to be established.
	select {
	case err := <-cancel:
		// If the dial has already completed, recycle the connection.
		if !atomic.CompareAndSwapUint32(&syn, 0, 1) {
			if b := <-ch; b.c != nil {
				t.putIdle(b.c)
			}
		}

		// We can't return nil errors.
		if err == nil {
			return nil, ErrNilCancel
		} else {
			return nil, err
		}

	case b := <-ch:
		if b.e != nil {
			return nil, b.e
		}

		// Write the request and read the response using a separate
		// goroutine, as to not block this one.
		go func() {
			resp, err := roundTrip(b.c, req, wsize)
			ch <- baton{r: resp, e: err}
		}()
	}

	// Wait for the response to come back.
	select {
	case err := <-cancel:
		c.Close()

		// We can't return nil errors.
		if err == nil {
			return nil, ErrNilCancel
		} else {
			return nil, err
		}

	case b := <-ch:
		return b.r, b.e
	}
}

func roundTrip(c *conn, req *heat.Request, wsize heat.BodySize) (*heat.Response, error) {
	// TODO: Add support for Expect: 100-continue.

	// Write the request header.
	if err := heat.WriteRequestHeader(c, req); err != nil {
		return nil, err
	}
	if err := c.Flush(); err != nil {
		return nil, err
	}

	// Did the user explicitly disable keep-alive for this request?
	reuse := !heat.Closing(req.Major, req.Minor, req.Fields)

	// Transmit the request body.
	if wsize != 0 {
		go func(reuse bool) {
			err := heat.WriteBody(c, req.Body, wsize)
			if err == nil {
				err = c.Flush()
			}
			c.maybeClose(err == nil && reuse)
		}(reuse)
	} else {
		c.maybeClose(reuse)
	}

	// Read the response.
	resp, err := heat.ReadResponseHeader(c)
	if err != nil {
		return nil, err
	}

	rsize, err := heat.ResponseBodySize(resp, req.Method)
	if err != nil {
		return nil, err
	}

	// Is the server cool with us potentially reusing this connection?
	reuse = !heat.Closing(resp.Major, resp.Minor, resp.Fields)

	// Attach a reader for the response body (if there is one).
	if rsize != 0 {
		r, _ := heat.OpenBody(c, rsize)
		resp.Body = &body{
			r:     r,
			c:     c,
			reuse: reuse && rsize != heat.Unbounded,
		}
	} else {
		c.maybeClose(reuse)
	}

	return resp, nil
}

func (t *Transport) dial(scheme, addr string) (*conn, error) {
	var dial func(addr string) (net.Conn, error)

	// Scheme-specific rules.
	switch scheme {
	case "http":
		addr = defaultPort(addr, "80")
		if c := t.takeIdle(t.idleTCP, addr); c != nil {
			return c, nil
		}
		dial = t.Dial

	case "https":
		addr = defaultPort(addr, "443")
		if c := t.takeIdle(t.idleTLS, addr); c != nil {
			return c, nil
		}
		dial = t.DialTLS

	default:
		return nil, ErrUnsupportedScheme
	}

	// Invoke the real dial function.
	raw, err := dial(addr)
	if err != nil {
		return nil, err
	}

	return newConn(raw, t, scheme == "https", addr), nil
}

func defaultPort(addr, port string) string {
	if !hasPort(addr) {
		addr = net.JoinHostPort(addr, "80")
	}
	return addr
}

func hasPort(addr string) bool {
	if len(addr) == 0 {
		return false
	}

	var colons int
	var rbrack bool

	for i, c := range addr {
		if c == ':' {
			colons++
			rbrack = addr[i-1] == ']'
		}
	}

	switch colons {
	case 0:
		return false
	case 1:
		return true
	default:
		return addr[0] == '[' && rbrack
	}
}

func (t *Transport) takeIdle(m map[string]*conn, addr string) *conn {
	t.mu.Lock()
	defer t.mu.Unlock()

	c := m[addr]
	if c == nil {
		return nil
	}

	// Unlink the connection.
	if c.next != nil {
		m[addr] = c.next
		c.next = nil
	} else {
		delete(m, addr)
	}

	return c
}

func (t *Transport) putIdle(c *conn) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Update the idle timestamp.
	c.idleSince = time.Now()

	// Put the connection in the relevant map.
	if !c.tls {
		put(&t.idleTCP, c)
	} else {
		put(&t.idleTLS, c)
	}

	// Start the garbage collection goroutine.
	if !t.cleaning && t.KeepAliveTimeout > 0 {
		t.cleaning = true
		go t.clean()
	}
}

func put(m *map[string]*conn, c *conn) {
	if *m == nil {
		*m = make(map[string]*conn)
	}

	c.next = (*m)[c.addr]
	(*m)[c.addr] = c
}

func (t *Transport) clean() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	// Continually loop and close connections that have been idle
	// for at least KeepAliveTimeout.
	for _ = range ticker.C {
		t.mu.Lock()

		cutoff := time.Now().Add(-t.KeepAliveTimeout)
		drop(t.idleTCP, cutoff)
		drop(t.idleTLS, cutoff)

		// When all idle connections have been closed, halt.
		if len(t.idleTCP) == 0 && len(t.idleTLS) == 0 {
			t.idleTCP = nil
			t.idleTLS = nil
			t.cleaning = false

			t.mu.Unlock()
			break
		}

		t.mu.Unlock()
	}
}

func drop(m map[string]*conn, cutoff time.Time) {
	for h, conn := range m {
		// Because connections are ordered by their last-use time in descending
		// order, we can quickly discard the whole chain if the first connection
		// has sat idle for too long.
		if conn.idleSince.Before(cutoff) {
			for conn != nil {
				conn.Close()
				conn = conn.next
			}

			delete(m, h)
			continue
		}

		last := conn
		conn = conn.next

		// Fast forward through the linked list until we reach the first
		// connection that is due to be closed (if any).
		for conn != nil && !conn.idleSince.Before(cutoff) {
			last = conn
			conn = conn.next
		}

		// Close all connections after last in the linked list, then reset
		// last.next to let them be garbage collected.
		for conn != nil {
			conn.Close()
			conn = conn.next
		}

		last.next = nil
	}
}
