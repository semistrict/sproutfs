package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

// client speaks the part of the Redis serialization protocol a load and a walk
// need: commands as arrays of bulk strings, and replies that are a status, an
// error, an integer, a bulk string or an array of bulk strings. It is not safe
// for concurrent use; one walk is one connection and one request at a time.
type client struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
}

func dial(network, address string) (*client, error) {
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	return &client{conn: conn, reader: bufio.NewReaderSize(conn, 1<<16), writer: bufio.NewWriterSize(conn, 1<<16)}, nil
}

func (c *client) Close() error { return c.conn.Close() }

// send queues one command. Nothing reaches the server until flush.
func (c *client) send(args ...[]byte) error {
	if _, err := fmt.Fprintf(c.writer, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(c.writer, "$%d\r\n", len(arg)); err != nil {
			return err
		}
		if _, err := c.writer.Write(arg); err != nil {
			return err
		}
		if _, err := c.writer.WriteString("\r\n"); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) flush() error { return c.writer.Flush() }

// errServer is a reply the server sent as an error.
var errServer = errors.New("the server answered with an error")

// reply is one reply: Status for a simple string, Integer for an integer, Bulk
// for a bulk string (Nil when it was the null one) and Array for an array of
// bulk strings.
type reply struct {
	Status  string
	Integer int64
	Bulk    []byte
	Nil     bool
	Array   [][]byte
}

// receive reads one reply. A bulk string's bytes are the caller's to keep.
func (c *client) receive() (reply, error) {
	line, err := c.line()
	if err != nil {
		return reply{}, err
	}
	if len(line) == 0 {
		return reply{}, fmt.Errorf("an empty reply line")
	}
	body := string(line[1:])
	switch line[0] {
	case '+':
		return reply{Status: body}, nil
	case '-':
		return reply{}, fmt.Errorf("%w: %s", errServer, body)
	case ':':
		value, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return reply{}, fmt.Errorf("an integer reply %q: %w", body, err)
		}
		return reply{Integer: value}, nil
	case '$':
		bulk, isNil, err := c.bulk(body)
		return reply{Bulk: bulk, Nil: isNil}, err
	case '*':
		count, err := strconv.Atoi(body)
		if err != nil {
			return reply{}, fmt.Errorf("an array reply %q: %w", body, err)
		}
		if count < 0 {
			return reply{Nil: true}, nil
		}
		array := make([][]byte, count)
		for at := range array {
			header, err := c.line()
			if err != nil {
				return reply{}, err
			}
			if len(header) == 0 || header[0] != '$' {
				return reply{}, fmt.Errorf("an array element %q, want a bulk string", header)
			}
			if array[at], _, err = c.bulk(string(header[1:])); err != nil {
				return reply{}, err
			}
		}
		return reply{Array: array}, nil
	}
	return reply{}, fmt.Errorf("a reply of unknown type %q", line)
}

func (c *client) bulk(length string) ([]byte, bool, error) {
	size, err := strconv.Atoi(length)
	if err != nil {
		return nil, false, fmt.Errorf("a bulk length %q: %w", length, err)
	}
	if size < 0 {
		return nil, true, nil
	}
	data := make([]byte, size+2)
	if _, err := io.ReadFull(c.reader, data); err != nil {
		return nil, false, err
	}
	if data[size] != '\r' || data[size+1] != '\n' {
		return nil, false, fmt.Errorf("a bulk string of %d bytes not ended by CRLF", size)
	}
	return data[:size], false, nil
}

// line reads one CRLF-terminated line without its terminator.
func (c *client) line() ([]byte, error) {
	line, err := c.reader.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, fmt.Errorf("a reply line %q not ended by CRLF", line)
	}
	return line[:len(line)-2], nil
}

// do sends one command and reads its reply.
func (c *client) do(args ...[]byte) (reply, error) {
	if err := c.send(args...); err != nil {
		return reply{}, err
	}
	if err := c.flush(); err != nil {
		return reply{}, err
	}
	return c.receive()
}

func words(args ...string) [][]byte {
	out := make([][]byte, len(args))
	for at, arg := range args {
		out[at] = []byte(arg)
	}
	return out
}
