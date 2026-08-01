package main

import (
	"io"
	"net"
	"os"
	"time"
)

// engineConn adapts the engine's file-transport wire pump to net.Conn
// so pgx can dial the in-process engine. Single connection, single
// goroutine. The engine is synchronous: replies only ever appear as a
// result of a tick, so Read drains what the last tick produced and
// otherwise nudges the engine.
type engineConn struct {
	e      *engine
	recv   []byte
	closed bool
}

func newEngineConn(e *engine) *engineConn { return &engineConn{e: e} }

func (c *engineConn) Write(p []byte) (int, error) {
	if c.closed || c.e.dead {
		return 0, io.ErrClosedPipe
	}
	data, err := c.e.sendWire(p)
	if err != nil {
		return 0, err
	}
	c.recv = append(c.recv, data...)
	return len(p), nil
}

func (c *engineConn) Read(p []byte) (int, error) {
	if c.closed {
		return 0, io.EOF
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(c.recv) == 0 {
		if c.e.dead {
			return 0, io.ErrUnexpectedEOF
		}
		// A reply can only be produced by a tick; give the engine a
		// few extra ticks in case output lagged the consuming tick.
		data, err := c.e.tickAndDrain()
		if err != nil {
			return 0, err
		}
		c.recv = append(c.recv, data...)
		if len(c.recv) > 0 {
			break
		}
		if time.Now().After(deadline) {
			return 0, os.ErrDeadlineExceeded
		}
		time.Sleep(5 * time.Millisecond)
	}
	n := copy(p, c.recv)
	c.recv = c.recv[n:]
	return n, nil
}

func (c *engineConn) Close() error {
	c.closed = true
	return nil
}

func (c *engineConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (c *engineConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (c *engineConn) SetDeadline(t time.Time) error      { return nil }
func (c *engineConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *engineConn) SetWriteDeadline(t time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "pglite" }
func (dummyAddr) String() string  { return "pglite" }
