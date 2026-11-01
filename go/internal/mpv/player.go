package mpv

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Observed are the properties pushed to OnProperty while mpv runs.
var Observed = []string{"path", "media-title", "pause", "playlist-pos", "playlist-count", "time-pos", "duration"}

// Player keeps one mpv process running and reconnects after it restarts.
type Player struct {
	Bin        string
	Socket     string
	Args       []string                                // extra arguments (config lives in ~/.config/mpv/mpv.conf)
	OnConnect  func(ctx context.Context)               // called after each (re)start, e.g. to load the playlist
	OnProperty func(name string, data json.RawMessage) // observed property changes

	mu   sync.Mutex
	conn *conn
	proc *os.Process
}

// Run starts mpv and restarts it whenever it exits, until ctx is cancelled.
func (p *Player) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := p.runOnce(ctx); err != nil {
			slog.Error("mpv", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (p *Player) runOnce(ctx context.Context) error {
	os.Remove(p.Socket)
	args := append([]string{"--idle=yes", "--input-ipc-server=" + p.Socket}, p.Args...)
	cmd := exec.Command(p.Bin, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	slog.Info("mpv started", "pid", cmd.Process.Pid)
	p.mu.Lock()
	p.proc = cmd.Process
	p.mu.Unlock()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var cn *conn
	for i := 0; i < 40 && cn == nil; i++ {
		select {
		case err := <-exited:
			return err
		case <-ctx.Done():
			cmd.Process.Kill()
			<-exited
			return nil
		case <-time.After(250 * time.Millisecond):
		}
		cn, _ = dial(p.Socket, p.OnProperty)
	}
	if cn == nil {
		cmd.Process.Kill()
		<-exited
		return ErrNotConnected
	}
	for i, name := range Observed {
		cn.command(ctx, "observe_property", i+1, name)
	}
	p.mu.Lock()
	p.conn = cn
	p.mu.Unlock()
	if p.OnConnect != nil {
		p.OnConnect(ctx)
	}

	var err error
	select {
	case err = <-exited:
	case <-cn.closed:
		cmd.Process.Kill()
		err = <-exited
	case <-ctx.Done():
		cn.command(context.Background(), "quit")
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
	}
	p.mu.Lock()
	p.conn, p.proc = nil, nil
	p.mu.Unlock()
	cn.close()
	slog.Info("mpv exited", "err", err)
	return err
}

// Restart kills mpv; Run starts a fresh one which reloads the playlist.
func (p *Player) Restart() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.proc != nil {
		p.proc.Kill()
	}
}

// Command sends one IPC command and returns its data.
func (p *Player) Command(ctx context.Context, args ...any) (json.RawMessage, error) {
	p.mu.Lock()
	cn := p.conn
	p.mu.Unlock()
	if cn == nil {
		return nil, ErrNotConnected
	}
	return cn.command(ctx, args...)
}

// CommandNamed sends a command with named arguments, e.g.
// {"name": "osd-overlay", "id": 1, "format": "ass-events", "data": "..."}.
func (p *Player) CommandNamed(ctx context.Context, args map[string]any) (json.RawMessage, error) {
	p.mu.Lock()
	cn := p.conn
	p.mu.Unlock()
	if cn == nil {
		return nil, ErrNotConnected
	}
	return cn.send(ctx, args, args["name"])
}

// PlaylistEntry is one item of mpv's "playlist" property.
type PlaylistEntry struct {
	Filename string `json:"filename"`
	Current  bool   `json:"current"`
}

func (p *Player) Playlist(ctx context.Context) ([]PlaylistEntry, error) {
	data, err := p.Command(ctx, "get_property", "playlist")
	if err != nil {
		return nil, err
	}
	var pl []PlaylistEntry
	err = json.Unmarshal(data, &pl)
	return pl, err
}
