package server

import (
	"net"
	"sync"
)

type connectionListener struct {
	net.Listener
	slots chan struct{}
}

func (l *connectionListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &countedConnection{Conn: connection, release: func() { <-l.slots }}, nil
		default:
			_ = connection.Close()
		}
	}
}

type countedConnection struct {
	net.Conn
	once    sync.Once
	release func()
	err     error
}

func (c *countedConnection) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.release()
	})
	return c.err
}
