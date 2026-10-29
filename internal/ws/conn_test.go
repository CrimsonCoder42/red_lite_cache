// Covers: internal/ws
package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// serve starts an httptest server whose every socket is a Conn running handle; conns receives each
// Conn before Run.
func serve(t *testing.T, opts ConnOptions, handle func(c *Conn, frame []byte)) (string, chan *Conn) {
	t.Helper()
	conns := make(chan *Conn, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsc, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c := NewConn(wsc, opts)
		conns <- c
		c.Run(r.Context(), func(f []byte) { handle(c, f) })
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), conns
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func read(t *testing.T, c *websocket.Conn) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	return b, err
}

func write(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	if err := c.Write(context.Background(), websocket.MessageText, []byte(s)); err != nil {
		t.Fatal(err)
	}
}

// TestConnPanicClosesOnlyThatSocket: a panic in the frame handler is recovered and closes that
// socket; another socket on the same server keeps working, and Done closes.
func TestConnPanicClosesOnlyThatSocket(t *testing.T) {
	url, conns := serve(t, ConnOptions{SendQueue: 4}, func(c *Conn, f []byte) {
		if string(f) == "boom" {
			panic("boom")
		}
		c.Send(f)
	})
	a, b := dial(t, url), dial(t, url)
	ca, cb := <-conns, <-conns
	if ca.ID() == cb.ID() {
		t.Fatal("conn ids not unique")
	}
	write(t, a, "boom")
	if _, err := read(t, a); err == nil {
		t.Fatal("socket survived a handler panic")
	}
	select {
	case <-ca.Done():
	case <-cb.Done():
		t.Fatal("the other conn ended")
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed after panic")
	}
	write(t, b, "echo")
	if got, err := read(t, b); err != nil || string(got) != "echo" {
		t.Fatalf("other socket: %q %v", got, err)
	}
}

// TestConnQueueBoundAndKick: Send is non-blocking and refuses once the queue is full or the conn
// is kicked; Kick is idempotent and closes with StatusGoingAway.
func TestConnQueueBoundAndKick(t *testing.T) {
	url, conns := serve(t, ConnOptions{SendQueue: 2}, func(c *Conn, f []byte) {})
	a := dial(t, url)
	c := <-conns
	// fill: the writer may drain some frames to the socket, so push until refused
	refused := false
	for range 100_000 {
		if !c.Send(make([]byte, 64<<10)) {
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("Send never refused with a non-reading peer and a 2-frame queue")
	}
	c.Kick("test kick")
	c.Kick("again")
	if c.Send([]byte("x")) {
		t.Fatal("Send accepted after Kick")
	}
	var err error
	for err == nil {
		_, err = read(t, a)
	}
	if st := websocket.CloseStatus(err); st != websocket.StatusGoingAway && st != -1 {
		t.Fatalf("close status %d, want %d (or a reset)", st, websocket.StatusGoingAway)
	}
	<-c.Done()
}

// TestConnSendAndClose: the frame is written before the close, and later Sends are refused.
func TestConnSendAndClose(t *testing.T) {
	url, _ := serve(t, ConnOptions{SendQueue: 4}, func(c *Conn, f []byte) {
		c.Send([]byte("first"))
		c.SendAndClose([]byte("last"), websocket.StatusPolicyViolation, "bye")
		if c.Send([]byte("after")) {
			panic("Send accepted after SendAndClose")
		}
	})
	a := dial(t, url)
	write(t, a, "go")
	for _, want := range []string{"first", "last"} {
		if got, err := read(t, a); err != nil || string(got) != want {
			t.Fatalf("got %q %v, want %q", got, err, want)
		}
	}
	_, err := read(t, a)
	if st := websocket.CloseStatus(err); st != websocket.StatusPolicyViolation {
		t.Fatalf("close status %d (%v), want %d", st, err, websocket.StatusPolicyViolation)
	}
}

// TestConnPingFailureCloses: a peer that never answers pings (its reads stop) is closed after
// PingInterval + WriteTimeout.
func TestConnPingFailureCloses(t *testing.T) {
	url, conns := serve(t, ConnOptions{SendQueue: 4, PingInterval: 50 * time.Millisecond, WriteTimeout: 100 * time.Millisecond}, func(c *Conn, f []byte) {})
	dial(t, url) // never read: the client's pong is only sent from inside its Read
	c := <-conns
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("conn not closed after unanswered pings")
	}
}
