package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"deploybot/internal/auth"
	"deploybot/internal/docker"
	"deploybot/internal/executor"
	"deploybot/internal/poller"
	"deploybot/internal/store"
	terminalsession "deploybot/internal/terminal"
)

type fakeTerminal struct {
	availableErr error
	connection   terminalsession.Connection
	attached     chan struct{}
}

func (f *fakeTerminal) Available() error { return f.availableErr }

func (f *fakeTerminal) Attach(context.Context, uint16, uint16) (terminalsession.Connection, error) {
	if f.connection == nil {
		return nil, errors.New("no fake terminal connection")
	}
	if f.attached != nil {
		close(f.attached)
	}
	return f.connection, nil
}

type fakeTerminalConnection struct {
	net.Conn
	resized chan [2]uint16
}

func (c *fakeTerminalConnection) Resize(rows, cols uint16) error {
	c.resized <- [2]uint16{rows, cols}
	return nil
}

func newTerminalTestServer(t *testing.T, terminal terminalsession.Attacher) (*Server, []*http.Cookie) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "terminal.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	latest := func(context.Context, string) (string, error) { return "", nil }
	ex := executor.New(st, &executor.OSRunner{}, latest, 0)
	pl := poller.New(st, latest, ex, 0)
	hash, err := auth.HashPassword("test")
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(hash, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(st, &docker.Fake{}, ex, pl, a, terminal)

	login := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password=test"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.ServeHTTP(login, req)
	return srv, login.Result().Cookies()
}

func TestTerminalPageShowsUnavailableState(t *testing.T) {
	srv, cookies := newTerminalTestServer(t, &fakeTerminal{availableErr: errors.New("tmux is not installed")})
	req := httptest.NewRequest(http.MethodGet, "/terminal", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	for _, want := range []string{"Terminal unavailable", "tmux is not installed", "Persistent workspace"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestTerminalWebSocketRelaysInputOutputAndResize(t *testing.T) {
	serverSide, terminalSide := net.Pipe()
	t.Cleanup(func() { terminalSide.Close() })
	fakeConnection := &fakeTerminalConnection{Conn: serverSide, resized: make(chan [2]uint16, 1)}
	terminal := &fakeTerminal{connection: fakeConnection, attached: make(chan struct{})}
	srv, cookies := newTerminalTestServer(t, terminal)
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)

	ws, response, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/terminal/ws",
		terminalRequestHeaders(httpServer.URL, cookies),
	)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial websocket: %v (status %d)", err, status)
	}
	t.Cleanup(func() { ws.Close() })

	select {
	case <-terminal.attached:
	case <-time.After(time.Second):
		t.Fatal("terminal was not attached")
	}

	if err := ws.WriteJSON(terminalClientMessage{Type: "resize", Rows: 1000, Cols: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-fakeConnection.resized:
		if got != [2]uint16{500, 2} {
			t.Fatalf("resize = %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("resize was not relayed")
	}

	if err := ws.WriteJSON(terminalClientMessage{Type: "input", Data: "echo hello\r"}); err != nil {
		t.Fatal(err)
	}
	_ = terminalSide.SetReadDeadline(time.Now().Add(time.Second))
	input := make([]byte, len("echo hello\r"))
	if _, err := terminalSide.Read(input); err != nil {
		t.Fatalf("read terminal input: %v", err)
	}
	if string(input) != "echo hello\r" {
		t.Fatalf("input = %q", input)
	}

	outputErr := make(chan error, 1)
	go func() {
		_, err := terminalSide.Write([]byte("hello\r\n"))
		outputErr <- err
	}()
	messageType, output, err := ws.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.BinaryMessage || string(output) != "hello\r\n" {
		t.Fatalf("output type=%d body=%q", messageType, output)
	}
	if err := <-outputErr; err != nil {
		t.Fatal(err)
	}
}

func TestTerminalWebSocketRejectsCrossOrigin(t *testing.T) {
	srv, cookies := newTerminalTestServer(t, &fakeTerminal{connection: &fakeTerminalConnection{}})
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)
	headers := terminalRequestHeaders("https://attacker.example", cookies)
	_, response, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/terminal/ws",
		headers,
	)
	if err == nil {
		t.Fatal("expected cross-origin websocket to be rejected")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("status = %d, want %d (%v)", status, http.StatusForbidden, err)
	}
}

func terminalRequestHeaders(origin string, cookies []*http.Cookie) http.Header {
	headers := http.Header{"Origin": []string{origin}}
	values := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		values = append(values, fmt.Sprintf("%s=%s", cookie.Name, url.QueryEscape(cookie.Value)))
	}
	headers.Set("Cookie", strings.Join(values, "; "))
	return headers
}
