package vmmigrate_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/peer/peertest"
	"github.com/semistrict/sproutfs/platform"
)

type inspectingPageConn struct {
	platform.Conn
	inspect func(*peertest.PageReply)
}

func (c *inspectingPageConn) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	f, err := c.Conn.Receive(ctx)
	if err != nil {
		return f, err
	}
	frame, err := peertest.Read(f)
	if err != nil {
		return platform.ReceivedFrame{}, err
	}
	reply, ok := frame.PageReply()
	if !ok {
		return frame.Pass(), nil
	}
	c.inspect(reply)
	// The rewrite recomputes the transport checksum: corrupt-blob tests must
	// reach the decompressor rather than only testing the wire CRC.
	return frame.Rewrite(reply)
}

func TestPageRepliesCompressAndRejectInvalidDecodedPages(t *testing.T) {
	for _, mode := range []string{"compressed", "checksum", "version", "bitmap"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newServed(t, nil, 1)
				dial := s.migration.cluster.dialer("compression-dest")
				seen := false
				backing := s.dialing(t, nil, "ram0", func(ctx context.Context, address platform.Address) (platform.Conn, error) {
					conn, err := dial(ctx, address)
					if err != nil {
						return nil, err
					}
					return &inspectingPageConn{Conn: conn, inspect: func(reply *peertest.PageReply) {
						seen = true
						if len(reply.Payload) >= pageSize/2 {
							t.Errorf("repetitive page transferred %d bytes", len(reply.Payload))
						}
						switch mode {
						case "checksum":
							reply.Payload[16] ^= 1
						case "version":
							reply.PayloadFormat = 0
						case "bitmap":
							reply.Present = []byte{0x80}
						}
					}}, nil
				})
				got := make([]byte, pageSize)
				err := backing.Load(t.Context(), 0, got)
				if !seen {
					t.Fatal("page transport was not exercised")
				}
				if mode == "compressed" {
					if err != nil {
						t.Fatal(err)
					}
					want := make([]byte, pageSize)
					if held, _, err := s.machine.MemoryRegions()["ram0"].ReadResident(t.Context(), 0, want); err != nil || !held {
						t.Fatalf("resident source: %v", err)
					}
					if !bytes.Equal(got, want) || backing.Stats().PeerPages != 1 {
						t.Fatal("compressed page did not arrive intact")
					}
					return
				}
				// A reply this host cannot read is neither a source that is gone
				// nor one that stumbled: it is a source this destination cannot
				// use at all, so the fault fails with that cause and the received
				// VM is torn, rather than reading a volume whose bytes may predate
				// the guest's own write.
				if !errors.Is(err, peer.ErrMalformed) {
					t.Fatalf("a %s reply = %v, want a malformed frame", mode, err)
				}
				if !bytes.Equal(got, make([]byte, pageSize)) || backing.Stats().PeerPages != 0 {
					t.Fatal("an invalid page reached the caller")
				}
				if stats := backing.Stats(); stats.FellBack || stats.VolumePages != 0 {
					t.Fatalf("an unreadable reply was answered from the volume: %+v", stats)
				}
			})
		})
	}
}
