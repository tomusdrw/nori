package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/creack/pty"
)

const (
	defaultRows = 24
	defaultCols = 80
)

// Connection is one browser attachment to a persistent terminal session.
// Closing it detaches the client without stopping the tmux session.
type Connection interface {
	io.ReadWriteCloser
	Resize(rows, cols uint16) error
}

// Attacher opens client connections to a persistent terminal session.
type Attacher interface {
	Available() error
	Attach(ctx context.Context, rows, cols uint16) (Connection, error)
}

// Manager maintains a named tmux session. tmux owns the shell lifecycle, so
// commands continue running when every browser client has disconnected.
type Manager struct {
	name       string
	workingDir string
}

func New(name, workingDir string) *Manager {
	if workingDir == "" {
		workingDir = "."
	}
	if absolute, err := filepath.Abs(workingDir); err == nil {
		workingDir = absolute
	}
	return &Manager{name: name, workingDir: workingDir}
}

func (m *Manager) Available() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return errors.New("tmux is not installed")
	}
	info, err := os.Stat(m.workingDir)
	if err != nil {
		return fmt.Errorf("terminal working directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("terminal working directory %q is not a directory", m.workingDir)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		return errors.New("bash is not installed")
	}
	return nil
}

func (m *Manager) Attach(ctx context.Context, rows, cols uint16) (Connection, error) {
	if err := m.Available(); err != nil {
		return nil, err
	}
	if rows == 0 {
		rows = defaultRows
	}
	if cols == 0 {
		cols = defaultCols
	}

	cmd := exec.CommandContext(ctx, "tmux", "new-session", "-A", "-s", m.name, "-c", m.workingDir, "bash")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, fmt.Errorf("attach tmux session: %w", err)
	}

	client := &ptyConnection{file: ptmx, cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(client.done)
	}()
	return client, nil
}

type ptyConnection struct {
	file  *os.File
	cmd   *exec.Cmd
	done  chan struct{}
	close sync.Once
}

func (c *ptyConnection) Read(p []byte) (int, error)  { return c.file.Read(p) }
func (c *ptyConnection) Write(p []byte) (int, error) { return c.file.Write(p) }

func (c *ptyConnection) Resize(rows, cols uint16) error {
	return pty.Setsize(c.file, &pty.Winsize{Rows: rows, Cols: cols})
}

func (c *ptyConnection) Close() error {
	var closeErr error
	c.close.Do(func() {
		// Closing the PTY detaches this tmux client. It does not send input to the
		// shell and therefore leaves the named tmux session running.
		closeErr = c.file.Close()
		select {
		case <-c.done:
		case <-time.After(500 * time.Millisecond):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-c.done
		}
	})
	return closeErr
}
