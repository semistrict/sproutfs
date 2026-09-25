package vmmigrate_test

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/platform"
	migratev1 "github.com/semistrict/sproutfs/internal/vmmigrate/internal/gen/sproutfs/migrate/v1"
	"github.com/semistrict/sproutfs/internal/vmmigrate/internal/wire"
)

type inspectingPageConn struct {
	platform.Conn
	inspect func(*migratev1.PageResponse, []byte)
}

func (c *inspectingPageConn) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	f, err := c.Conn.Receive(ctx)
	if err != nil {
		return f, err
	}
	in, err := wire.Decode(f)
	if err != nil {
		return platform.ReceivedFrame{}, err
	}
	defer in.Payload.Close()
	message := new(migratev1.PageResponse)
	if err := in.UnmarshalTo(message); err != nil {
		return platform.ReceivedFrame{}, err
	}
	data, err := io.ReadAll(in.Payload)
	if err != nil {
		return platform.ReceivedFrame{}, err
	}
	c.inspect(message, data)
	// Recompute the transport checksum: corrupt-blob tests must reach the
	// decompressor rather than only testing the existing wire CRC.
	encoded, err := wire.Encode(wire.Outgoing{RequestID: in.RequestID, InReplyTo: in.InReplyTo,
		Message: message, Payload: wire.Payload{Body: bytes.NewReader(data), Size: int64(len(data)),
			Algorithm: wire.ChecksumCRC32C, Checksum: wire.EncodeCRC32C(crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)))}})
	if err != nil {
		return platform.ReceivedFrame{}, err
	}
	return platform.ReceivedFrame{Header: encoded.Header, PayloadSize: int64(len(data)), Payload: io.NopCloser(bytes.NewReader(data))}, nil
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
					return &inspectingPageConn{Conn: conn, inspect: func(response *migratev1.PageResponse, data []byte) {
						seen = true
						if len(data) >= pageSize/2 {
							t.Errorf("repetitive page transferred %d bytes", len(data))
						}
						switch mode {
						case "checksum":
							data[16] ^= 1
						case "version":
							response.SetPayloadFormat(0)
						case "bitmap":
							response.SetPresent([]byte{0x80})
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
				if !errors.Is(err, wire.ErrMalformedFrame) {
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
