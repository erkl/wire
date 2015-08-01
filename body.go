package wire

import (
	"errors"
	"io"
	"net"
	"time"
)

var ErrReadAfterClose = errors.New("read after close on response body")
var ErrBodyTimeout = errors.New("response body timed out")

// The BodyReader interface extends io.ReadClosers with the ability to set a deadline
// for read operations.
//
// All non-nil response bodies returned by Transport.RoundTrip implement this
// interface.
type BodyReader interface {
	io.ReadCloser

	// SetReadDeadline sets the deadline for future Read calls. A zero value
	// for t clears any previous deadline.
	SetReadDeadline(t time.Time) error
}

// Compile-time type check.
var _ BodyReader = new(body)

type body struct {
	// Actual body io.Reader.
	r io.Reader

	// Underlying conn instance.
	c *conn

	// Persisted error.
	err error

	// Has the user closed the body?
	closed bool

	// True if the server has indicated that the connection
	// can be reused.
	reuse bool
}

func (b *body) Read(buf []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}

	n, err := b.r.Read(buf)
	if err != nil {
		// Don't persist timeout errors.
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
			err = ErrBodyTimeout
		} else {
			b.err = err
		}
	}

	return n, err
}

func (b *body) SetReadDeadline(t time.Time) error {
	// Don't bother setting a timeout unless Read actually has a chance to
	// succeed. This also prevents the user from setting a deadline on a
	// connection after it has been repurposed for a new request.
	if b.err != nil {
		return nil
	}

	return b.c.raw.SetReadDeadline(t)
}

func (b *body) Close() error {
	if b.err == nil {
		b.err = ErrReadAfterClose
	}

	// Signal that we're done reading from the connection.
	if !b.closed {
		b.c.maybeClose(b.reuse && b.err == io.EOF)
	}

	b.closed = true
	return nil
}
