package wire

import (
	"errors"
	"io"
)

var ErrReadAfterClose = errors.New("read after close on response body")

type body struct {
	// Actual body io.Reader.
	r io.Reader

	// Associated Transport and conn instances.
	t *Transport
	c *conn

	// Persisted error.
	err error

	// True if the server has indicated that the connection
	// can be reused.
	isKeepAlive bool
}

func (b *body) Read(buf []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}

	n, err := b.r.Read(buf)
	if err != nil && b.err == nil {
		b.err = err
	}

	return n, err
}

func (b *body) Close() error {
	if b.err == nil {
		b.err = ErrReadAfterClose
	}

	// Signal that we're done with the response body.
	b.c.maybeClose(b.isKeepAlive && b.err == io.EOF)

	return nil
}
