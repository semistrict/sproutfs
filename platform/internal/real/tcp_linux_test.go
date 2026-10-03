package real

import (
	"context"
	"testing"

	"github.com/semistrict/sproutfs/platform"
)

// Every socket a host dials or accepts drops a peer whose acknowledgements stop
// for ten seconds, whatever the peer server's own pings have noticed.
func TestHostSocketsCarryAUserTimeout(t *testing.T) {
	listener, err := TCP{}.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan int, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			accepted <- -1
			return
		}
		defer conn.Close()
		timeout, err := userTimeoutOf(conn)
		if err != nil {
			timeout = -1
		}
		accepted <- timeout
	}()
	conn, err := TCP{}.Dial(context.Background(), platform.Address(listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	timeout, err := userTimeoutOf(conn)
	if err != nil || timeout != 10000 {
		t.Fatalf("a dialed socket's TCP_USER_TIMEOUT is %d ms (%v), want 10000", timeout, err)
	}
	if got := <-accepted; got != 10000 {
		t.Fatalf("an accepted socket's TCP_USER_TIMEOUT is %d ms, want 10000", got)
	}
}
