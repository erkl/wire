package wire

import (
	"errors"
	"net"

	"github.com/erkl/heat"
)

var ErrUnsupportedScheme = errors.New("unsupported scheme in request")

type Transport struct {
	// Dial specifies the function used to establish plain TCP connections
	// with remote hosts.
	Dial func(addr string) (net.Conn, error)

	// DialTLS specifies the function used to establish TLS connections with
	// remote hosts.
	DialTLS func(addr string) (net.Conn, error)
}

func (t *Transport) RoundTrip(req *heat.Request) (*heat.Response, error) {
	if req.Body != nil {
		defer req.Body.Close()
	}

	// Validate the request body size.
	wsize, err := heat.RequestBodySize(req)
	if err != nil {
		return nil, err
	}

	// Establish a connection.
	conn, err := t.dial(req.Scheme, req.Remote)
	if err != nil {
		return nil, err
	}

	// Write the request header.
	err = heat.WriteRequestHeader(conn, req)
	if err != nil {
		conn.Close()
		return nil, err
	}

	err = conn.Flush()
	if err != nil {
		conn.Close()
		return nil, err
	}

	// Did the user explicitly disable keep-alive for this request?
	reqKeepAlive := !heat.Closing(req.Major, req.Minor, req.Fields)

	// Transmit the request body.
	if wsize == 0 {
		conn.maybeClose(reqKeepAlive)
	} else {
		go func() {
			isKeepAlive := heat.WriteBody(conn, req.Body, wsize) == nil &&
				conn.Flush() == nil && reqKeepAlive
			conn.maybeClose(isKeepAlive)
		}()
	}

	// Read the response.
	resp, err := heat.ReadResponseHeader(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}

	rsize, err := heat.ResponseBodySize(resp, req.Method)
	if err != nil {
		conn.Close()
		return nil, err
	}

	// Is the server cool with us potentially reusing this connection?
	respKeepAlive := !heat.Closing(resp.Major, resp.Minor, resp.Fields)

	// Attach a reader for the response body (if there is one).
	if rsize == 0 {
		conn.maybeClose(respKeepAlive)
	} else {
		r, _ := heat.OpenBody(conn, rsize)

		resp.Body = &body{
			r:           r,
			t:           t,
			c:           conn,
			isKeepAlive: respKeepAlive && rsize != heat.Unbounded,
		}
	}

	return resp, nil
}

func (t *Transport) dial(scheme, addr string) (*conn, error) {
	var dial func(addr string) (net.Conn, error)

	// Scheme-specific rules.
	switch scheme {
	case "http":
		addr = defaultPort(addr, "80")
		dial = t.Dial

	case "https":
		addr = defaultPort(addr, "443")
		dial = t.DialTLS

	default:
		return nil, ErrUnsupportedScheme
	}

	// Invoke the real dial function.
	raw, err := dial(addr)
	if err != nil {
		return nil, err
	}

	return newConn(raw), nil
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

	for i, conn := range addr {
		if conn == ':' {
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
