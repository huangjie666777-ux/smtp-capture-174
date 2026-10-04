package main

import (
	"net"
	"sync"
	"time"
)

type ListenerLimiter struct {
	sem chan struct{}
}

func NewListenerLimiter(limit int) *ListenerLimiter {
	return &ListenerLimiter{sem: make(chan struct{}, limit)}
}

func (l *ListenerLimiter) Acquire() bool {
	select {
	case l.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *ListenerLimiter) Release() {
	<-l.sem
}

type limitedListener struct {
	inner   net.Listener
	limiter *ListenerLimiter
	reject  []byte
}

func newLimitedListener(inner net.Listener, limiter *ListenerLimiter, reject []byte) net.Listener {
	return &limitedListener{inner: inner, limiter: limiter, reject: reject}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.inner.Accept()
		if err != nil {
			return nil, err
		}
		if l.limiter.Acquire() {
			return &limitedConn{Conn: conn, limiter: l.limiter}, nil
		}
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		conn.Write(l.reject)
		conn.Close()
	}
}

func (l *limitedListener) Close() error {
	return l.inner.Close()
}

func (l *limitedListener) Addr() net.Addr {
	return l.inner.Addr()
}

type limitedConn struct {
	net.Conn
	limiter *ListenerLimiter
	once    sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.limiter.Release)
	return err
}
