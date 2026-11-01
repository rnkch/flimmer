// Package mpv runs the mpv player and talks to it over its JSON IPC socket.
package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// ErrNotConnected is returned when mpv is not running (yet).
var ErrNotConnected = errors.New("mpv not connected")

type reply struct {
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

type message struct {
	RequestID int64           `json:"request_id"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
	Event     string          `json:"event"`
	Name      string          `json:"name"`
}

// conn is one IPC connection. Each request carries a request_id so replies
// can be matched while events arrive on the same stream.
type conn struct {
	c       net.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply
	closed  chan struct{}
}

func dial(path string, onProperty func(name string, data json.RawMessage)) (*conn, error) {
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, err
	}
	cn := &conn{c: c, pending: map[int64]chan reply{}, closed: make(chan struct{})}
	go cn.readLoop(onProperty)
	return cn, nil
}

func (cn *conn) readLoop(onProperty func(string, json.RawMessage)) {
	defer close(cn.closed)
	sc := bufio.NewScanner(cn.c)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var m message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch {
		case m.Event == "property-change":
			if onProperty != nil {
				onProperty(m.Name, m.Data)
			}
		case m.Event == "" && m.RequestID != 0:
			cn.mu.Lock()
			ch := cn.pending[m.RequestID]
			delete(cn.pending, m.RequestID)
			cn.mu.Unlock()
			if ch != nil {
				ch <- reply{Data: m.Data, Error: m.Error}
			}
		}
	}
	cn.mu.Lock()
	for id, ch := range cn.pending {
		close(ch)
		delete(cn.pending, id)
	}
	cn.mu.Unlock()
}

func (cn *conn) command(ctx context.Context, args ...any) (json.RawMessage, error) {
	return cn.send(ctx, args, args[0])
}

// send writes one command: a positional []any or a map of named arguments
// (which must contain "name").
func (cn *conn) send(ctx context.Context, cmd any, name any) (json.RawMessage, error) {
	cn.mu.Lock()
	cn.nextID++
	id := cn.nextID
	ch := make(chan reply, 1)
	cn.pending[id] = ch
	cn.mu.Unlock()

	b, err := json.Marshal(map[string]any{"command": cmd, "request_id": id})
	if err != nil {
		return nil, err
	}
	cn.writeMu.Lock()
	cn.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = cn.c.Write(append(b, '\n'))
	cn.writeMu.Unlock()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, ErrNotConnected
		}
		if r.Error != "success" {
			return nil, fmt.Errorf("mpv %v: %s", name, r.Error)
		}
		return r.Data, nil
	case <-ctx.Done():
		cn.mu.Lock()
		delete(cn.pending, id)
		cn.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (cn *conn) close() { cn.c.Close() }
