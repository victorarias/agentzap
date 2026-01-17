package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testClient struct {
	conn   net.Conn
	reader *bufio.Reader
}

func startTestServer(t *testing.T, historyDir string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	state := newServerState(historyDir, 200, 0)
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(done)
				return
			}
			go handleConn(state, conn)
		}
	}()
	cleanup := func() {
		_ = ln.Close()
		<-done
	}
	return ln.Addr().String(), cleanup
}

func dialAndHello(t *testing.T, addr, id, session string) *testClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tc := &testClient{conn: conn, reader: bufio.NewReader(conn)}
	if err := writeJSON(conn, envelope{Type: "hello", AgentID: id, Session: session}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	_ = readEnv(t, tc, 2*time.Second)
	return tc
}

func readEnv(t *testing.T, c *testClient, timeout time.Duration) envelope {
	t.Helper()
	if timeout > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	}
	line, err := c.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var env envelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return env
}

func sendEnv(t *testing.T, c *testClient, env envelope) {
	t.Helper()
	if err := writeJSON(c.conn, env); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestPresenceBySession(t *testing.T) {
	addr, cleanup := startTestServer(t, "")
	defer cleanup()

	a := dialAndHello(t, addr, "agent_a", "alpha")
	defer func() { _ = a.conn.Close() }()
	b := dialAndHello(t, addr, "agent_b", "beta")
	defer func() { _ = b.conn.Close() }()

	sendEnv(t, a, envelope{Type: "presence", Session: "alpha"})
	resp := readEnv(t, a, 2*time.Second)
	if resp.Type != "presence" {
		t.Fatalf("expected presence, got %s", resp.Type)
	}

	var rows []map[string]string
	if err := json.Unmarshal([]byte(resp.Text), &rows); err != nil {
		t.Fatalf("presence json: %v", err)
	}
	if len(rows) != 1 || rows[0]["agent_id"] != "agent_a" {
		t.Fatalf("expected only agent_a, got %#v", rows)
	}
	_ = b
}

func TestMessageRoutingAndBroadcast(t *testing.T) {
	addr, cleanup := startTestServer(t, "")
	defer cleanup()

	a := dialAndHello(t, addr, "agent_a", "alpha")
	defer func() { _ = a.conn.Close() }()
	b := dialAndHello(t, addr, "agent_b", "alpha")
	defer func() { _ = b.conn.Close() }()
	c := dialAndHello(t, addr, "agent_c", "alpha")
	defer func() { _ = c.conn.Close() }()

	sendEnv(t, a, envelope{Type: "msg", To: "agent_b", Text: "hi"})
	ack := readEnv(t, a, 2*time.Second)
	if ack.Type != "ack" || ack.Status != "delivered" || ack.MsgID == "" {
		t.Fatalf("unexpected ack: %#v", ack)
	}
	got := readEnv(t, b, 2*time.Second)
	if got.Type != "msg" || got.AgentID != "agent_a" || got.Text != "hi" {
		t.Fatalf("unexpected msg: %#v", got)
	}

	sendEnv(t, a, envelope{Type: "msg", To: "all", Text: "broadcast"})
	_ = readEnv(t, a, 2*time.Second) // ack
	gotB := readEnv(t, b, 2*time.Second)
	gotC := readEnv(t, c, 2*time.Second)
	if gotB.Text != "broadcast" || gotC.Text != "broadcast" {
		t.Fatalf("broadcast mismatch: b=%#v c=%#v", gotB, gotC)
	}
}

func TestOfflineQueueDelivery(t *testing.T) {
	addr, cleanup := startTestServer(t, "")
	defer cleanup()

	a := dialAndHello(t, addr, "agent_a", "alpha")
	defer func() { _ = a.conn.Close() }()

	sendEnv(t, a, envelope{Type: "msg", To: "agent_b", Text: "queued"})
	ack := readEnv(t, a, 2*time.Second)
	if ack.Type != "ack" || ack.Status != "queued" {
		t.Fatalf("expected queued ack, got %#v", ack)
	}

	b := dialAndHello(t, addr, "agent_b", "alpha")
	defer func() { _ = b.conn.Close() }()
	got := readEnv(t, b, 2*time.Second)
	if got.Type != "msg" || got.Text != "queued" || got.AgentID != "agent_a" {
		t.Fatalf("unexpected queued delivery: %#v", got)
	}
}

func TestNoQueueOffline(t *testing.T) {
	addr, cleanup := startTestServer(t, "")
	defer cleanup()

	a := dialAndHello(t, addr, "agent_a", "alpha")
	defer func() { _ = a.conn.Close() }()
	sendEnv(t, a, envelope{Type: "msg", To: "agent_b", Text: "drop", Meta: map[string]string{"no_queue": "true"}})
	ack := readEnv(t, a, 2*time.Second)
	if ack.Type != "ack" || ack.Status != "offline" {
		t.Fatalf("expected offline ack, got %#v", ack)
	}
}

func TestPinSetGet(t *testing.T) {
	dir := t.TempDir()
	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	a := dialAndHello(t, addr, "agent_a", "alpha")
	defer func() { _ = a.conn.Close() }()
	sendEnv(t, a, envelope{Type: "pin_set", Session: "alpha", Meta: map[string]string{"key": "topic", "value": "hello"}})
	ack := readEnv(t, a, 2*time.Second)
	if ack.Type != "pin_ack" {
		t.Fatalf("unexpected pin_ack: %#v", ack)
	}
	sendEnv(t, a, envelope{Type: "pin_get", Session: "alpha", Meta: map[string]string{"key": "topic"}})
	got := readEnv(t, a, 2*time.Second)
	if got.Type != "pin" || !strings.Contains(got.Text, "hello") {
		t.Fatalf("unexpected pin: %#v", got)
	}
}

func TestHistoryWrites(t *testing.T) {
	dir := t.TempDir()
	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	a := dialAndHello(t, addr, "agent_a", "alpha")
	defer func() { _ = a.conn.Close() }()
	b := dialAndHello(t, addr, "agent_b", "alpha")
	defer func() { _ = b.conn.Close() }()

	sendEnv(t, a, envelope{Type: "msg", To: "agent_b", Text: "logged", Thread: "alpha-thread"})
	_ = readEnv(t, a, 2*time.Second) // ack
	_ = readEnv(t, b, 2*time.Second) // message

	path := filepath.Join(dir, "alpha__alpha-thread.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if !strings.Contains(string(data), `"text":"logged"`) {
		t.Fatalf("history missing message: %s", string(data))
	}
}

func TestHistorySessionIsolation(t *testing.T) {
	addr, cleanup := startTestServer(t, "")
	defer cleanup()

	a := dialAndHello(t, addr, "agent_a", "alpha")
	defer func() { _ = a.conn.Close() }()
	b := dialAndHello(t, addr, "agent_b", "beta")
	defer func() { _ = b.conn.Close() }()

	sendEnv(t, a, envelope{Type: "msg", To: "agent_a", Text: "alpha msg", Thread: "shared"})
	_ = readEnv(t, a, 2*time.Second) // ack
	_ = readEnv(t, a, 2*time.Second) // delivered msg

	sendEnv(t, b, envelope{Type: "msg", To: "agent_b", Text: "beta msg", Thread: "shared"})
	_ = readEnv(t, b, 2*time.Second) // ack
	_ = readEnv(t, b, 2*time.Second) // delivered msg

	sendEnv(t, a, envelope{Type: "history", Session: "alpha", Thread: "shared"})
	histAlpha := readEnv(t, a, 2*time.Second)
	if histAlpha.Type != "history" || strings.Contains(histAlpha.Text, "beta msg") {
		t.Fatalf("history leak: %s", histAlpha.Text)
	}
}
