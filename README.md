**wire** is a small, extendable HTTP/1.X client library for Go.

By itself it does very little, probably no more than the very least you'd
expect an HTTP client to do: it sends requests and returns their responses,
managing keep-alive connections in between. However, a simple middleware system
allows the core transport to be extended with all kinds of additional
functionality.

Middleware functions which change the functionality of an underlying transport
by modifying requests as they go out, and responses as they come in. Pieces of
middleware can be used to set a default User-Agent header, or enforce response
timeouts, or limit the number of concurrent requests per domain, or
transparently follow 3XX redirects, or attach persistent cookie jars, or...
well, you get the idea.

See [godoc.org](http://godoc.org/github.com/erkl/wire) for the specifics.


#### Benchmark

Let me preface this by saying that realistically, issuing HTTP requests isn't
ever going to be the bottleneck in your application (and even if it is, it'll
be due to network latency or bandwidth constraints). In any case, here are some
numbers comparing `wire.Transport` and `http.Transport` for small HTTP
round-trips over synthetic keep-alive connections.

```
net/http       15091 ns/op     2241 B/op     38 allocs/op     (100% ops/s)
erkl/wire       4246 ns/op      524 B/op     14 allocs/op     (355% ops/s)
erkl/wire ✝     6753 ns/op      836 B/op     24 allocs/op     (223% ops/s)
```

The benchmark marked ✝ includes a non-nil cancel channel in the `RoundTrip`
call. This incurs a performance penalty because all I/O has to be carried out
in separate goroutines.


#### License

```
Copyright (c) 2015, Erik Lundin.

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY
AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM
LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE
OR OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR
PERFORMANCE OF THIS SOFTWARE.
```
