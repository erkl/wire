package wire

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/erkl/heat"
)

var ErrUnsupportedScheme = errors.New("unsupported scheme in request")

type Transport struct {
	// Dial specifies the function used to establish plain TCP connections
	// with remote hosts.
	Dial func(addr string, deadline time.Time) (net.Conn, error)

	// DialTLS specifies the function used to establish TLS connections with
	// remote hosts.
	DialTLS func(addr string, deadline time.Time) (net.Conn, error)

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

// Make sure Transport implements the RoundTripper interface.
var _ RoundTripper = new(Transport)

func (t *Transport) RoundTrip(req *heat.Request, deadline time.Time) (*heat.Response, error) {
	if req.Body != nil {
		defer req.Body.Close()
	}

	// Validate the request body size.
	wsize, err := heat.RequestBodySize(req)
	if err != nil {
		return nil, err
	}

	// Establish a connection.
	c, err := t.dial(req.Scheme, req.Remote, deadline)
	if err != nil {
		return nil, err
	}

	// Enforce the deadline.
	if !deadline.IsZero() {
		err = c.raw.SetReadDeadline(deadline)
		if err != nil {
			return nil, err
		}
	}

	// Write the request header.
	err = heat.WriteRequestHeader(c, req)
	if err != nil {
		c.Close()
		return nil, err
	}

	err = c.Flush()
	if err != nil {
		c.Close()
		return nil, err
	}

	// Did the user explicitly disable keep-alive for this request?
	reqKeepAlive := !heat.Closing(req.Major, req.Minor, req.Fields)

	// Transmit the request body.
	if wsize == 0 {
		c.maybeClose(reqKeepAlive)
	} else {
		go func() {
			isKeepAlive := heat.WriteBody(c, req.Body, wsize) == nil &&
				c.Flush() == nil && reqKeepAlive
			c.maybeClose(isKeepAlive)
		}()
	}

	// Read the response.
	resp, err := heat.ReadResponseHeader(c)
	if err != nil {
		c.Close()
		return nil, err
	}

	rsize, err := heat.ResponseBodySize(resp, req.Method)
	if err != nil {
		c.Close()
		return nil, err
	}

	// Clear the deadline we set earlier.
	if !deadline.IsZero() {
		err = c.raw.SetReadDeadline(deadline)
		if err != nil {
			return nil, err
		}
	}

	// Is the server cool with us potentially reusing this connection?
	respKeepAlive := !heat.Closing(resp.Major, resp.Minor, resp.Fields)

	// Attach a reader for the response body (if there is one).
	if rsize == 0 {
		c.maybeClose(respKeepAlive)
	} else {
		r, _ := heat.OpenBody(c, rsize)

		resp.Body = &body{
			r:           r,
			c:           c,
			isKeepAlive: respKeepAlive && rsize != heat.Unbounded,
		}
	}

	return resp, nil
}

func (t *Transport) dial(scheme, addr string, deadline time.Time) (*conn, error) {
	var dial func(addr string, deadline time.Time) (net.Conn, error)

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
	raw, err := dial(addr, deadline)
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
