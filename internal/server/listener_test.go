package server

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnectionAdmissionClosesExcessBeforeHTTPAndRecovers(t *testing.T) {
	underlying := &testListener{accepted: make(chan net.Conn, 3), closed: make(chan struct{})}
	listener := &connectionListener{Listener: underlying, slots: make(chan struct{}, 1)}
	first := &testConnection{closed: make(chan struct{})}
	underlying.accepted <- first
	admitted, err := listener.Accept()
	if err != nil || len(listener.slots) != 1 {
		t.Fatalf("first admission failed: %v", err)
	}
	excess := &testConnection{closed: make(chan struct{})}
	underlying.accepted <- excess
	result := make(chan net.Conn, 1)
	failures := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			failures <- err
		} else {
			result <- connection
		}
	}()
	select {
	case <-excess.closed:
	case <-time.After(time.Second):
		t.Fatal("excess connection was queued instead of closed")
	}
	if excess.closes.Load() != 1 || excess.writes.Load() != 0 || len(listener.slots) != 1 {
		t.Fatal("excess connection received a fake HTTP response or altered admission")
	}
	if err := admitted.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := &testConnection{closed: make(chan struct{})}
	underlying.accepted <- replacement
	select {
	case connection := <-result:
		if len(listener.slots) != 1 || connection == replacement {
			t.Fatal("replacement was not counted")
		}
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("closing a connection did not restore admission")
	}
	if len(listener.slots) != 0 || replacement.closes.Load() != 1 {
		t.Fatal("replacement close leaked a slot")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener did not preserve close cancellation: %v", err)
	}
}

func TestCountedConnectionClosesAndReleasesExactlyOnceEvenOnError(t *testing.T) {
	cause := errors.New("close failed")
	underlying := &testConnection{closed: make(chan struct{}), err: cause}
	var released atomic.Int32
	connection := &countedConnection{Conn: underlying, release: func() { released.Add(1) }}
	var wait sync.WaitGroup
	results := make(chan error, 32)
	for range 32 {
		wait.Go(func() { results <- connection.Close() })
	}
	wait.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, cause) {
			t.Fatalf("close did not preserve its first result: %v", err)
		}
	}
	if underlying.closes.Load() != 1 || released.Load() != 1 {
		t.Fatalf("closed %d times, released %d times", underlying.closes.Load(), released.Load())
	}
}

type testListener struct {
	accepted chan net.Conn
	closed   chan struct{}
	once     sync.Once
}

func (l *testListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.accepted:
		return connection, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *testListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }
func (*testListener) Addr() net.Addr { return &net.TCPAddr{} }

type testConnection struct {
	net.Conn
	closes atomic.Int32
	writes atomic.Int32
	closed chan struct{}
	err    error
}

func (c *testConnection) Close() error {
	if c.closes.Add(1) == 1 {
		close(c.closed)
	}
	return c.err
}

func (c *testConnection) Write(data []byte) (int, error) {
	c.writes.Add(1)
	return len(data), nil
}
