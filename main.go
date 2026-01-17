package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	yamlv3 "gopkg.in/yaml.v3"
)

const (
	defaultAddr       = "0.0.0.0:9800"
	defaultHistory    = "./data/history"
	defaultMaxHistory = 200
)

type config struct {
	Addr    string `yaml:"addr"`
	Session string `yaml:"session"`
	ID      string `yaml:"id"`
}

type envelope struct {
	Type      string            `json:"type"`
	AgentID   string            `json:"agent_id,omitempty"`
	Session   string            `json:"session,omitempty"`
	To        string            `json:"to,omitempty"`
	Text      string            `json:"text,omitempty"`
	Thread    string            `json:"thread,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
	MsgID     string            `json:"msg_id,omitempty"`
	Status    string            `json:"status,omitempty"`
	ExpiresAt int64             `json:"exp,omitempty"`
	Timestamp int64             `json:"ts,omitempty"`
}

type msgRecord struct {
	From      string            `json:"from"`
	To        string            `json:"to"`
	Text      string            `json:"text"`
	Thread    string            `json:"thread,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
	MsgID     string            `json:"msg_id,omitempty"`
	Timestamp int64             `json:"ts"`
}

type agentConn struct {
	id         string
	session    string
	conn       net.Conn
	lastSeen   time.Time
	lastFrom   string
	lastTo     string
	lastMsgAt  time.Time
	lastMsgTxt string
}

type sessionState struct {
	agents map[string]*agentConn
}

type broadcastState struct {
	messages []envelope
	index    map[string]int
}

type serverState struct {
	mu           sync.Mutex
	sessions     map[string]*sessionState
	history      map[string][]msgRecord
	offline      map[string]map[string][]envelope
	broadcast    map[string]*broadcastState
	pins         map[string]map[string]string
	pinsLoaded   map[string]bool
	historyDir   string
	pinsDir      string
	maxPerThread int
	queueTTL     time.Duration
}

func newServerState(historyDir string, maxPerThread int, queueTTL time.Duration) *serverState {
	pinsDir := ""
	if historyDir != "" {
		pinsDir = filepath.Join(filepath.Dir(historyDir), "pins")
	}
	return &serverState{
		sessions:     make(map[string]*sessionState),
		history:      make(map[string][]msgRecord),
		offline:      make(map[string]map[string][]envelope),
		broadcast:    make(map[string]*broadcastState),
		pins:         make(map[string]map[string]string),
		pinsLoaded:   make(map[string]bool),
		historyDir:   historyDir,
		pinsDir:      pinsDir,
		maxPerThread: maxPerThread,
		queueTTL:     queueTTL,
	}
}

func (s *serverState) registerAgent(session, id string, c net.Conn) []envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		session = "default"
	}
	ss, ok := s.sessions[session]
	if !ok {
		ss = &sessionState{agents: make(map[string]*agentConn)}
		s.sessions[session] = ss
	}
	ss.agents[id] = &agentConn{
		id:       id,
		session:  session,
		conn:     c,
		lastSeen: time.Now(),
	}
	var queued []envelope
	if perAgent, ok := s.offline[session]; ok {
		if q, ok := perAgent[id]; ok {
			queued = append(queued, q...)
			delete(perAgent, id)
		}
	}
	if b, ok := s.broadcast[session]; ok {
		if b.index == nil {
			b.index = make(map[string]int)
		}
		start := b.index[id]
		if start < len(b.messages) {
			queued = append(queued, b.messages[start:]...)
			b.index[id] = len(b.messages)
		}
	}
	return queued
}

func (s *serverState) unregisterAgent(session, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		session = "default"
	}
	if ss, ok := s.sessions[session]; ok {
		delete(ss.agents, id)
		if len(ss.agents) == 0 {
			delete(s.sessions, session)
		}
	}
}

func (s *serverState) updatePresence(session, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		session = "default"
	}
	if ss, ok := s.sessions[session]; ok {
		if a, ok := ss.agents[id]; ok {
			a.lastSeen = time.Now()
		}
	}
}

func (s *serverState) noteLastMessage(session, from, to, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		session = "default"
	}
	if ss, ok := s.sessions[session]; ok {
		if a, ok := ss.agents[from]; ok {
			a.lastTo = to
			a.lastMsgAt = time.Now()
			a.lastMsgTxt = text
		}
		if a, ok := ss.agents[to]; ok {
			a.lastFrom = from
		}
	}
}

func (s *serverState) queueOffline(session, to string, env envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		session = "default"
	}
	if _, ok := s.offline[session]; !ok {
		s.offline[session] = make(map[string][]envelope)
	}
	s.offline[session][to] = append(s.offline[session][to], env)
}

func (s *serverState) addBroadcast(session string, env envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		session = "default"
	}
	b, ok := s.broadcast[session]
	if !ok {
		b = &broadcastState{index: make(map[string]int)}
		s.broadcast[session] = b
	}
	b.messages = append(b.messages, env)
}

func (s *serverState) loadPins(session string) {
	if s.pinsDir == "" {
		return
	}
	if s.pinsLoaded[session] {
		return
	}
	path := filepath.Join(s.pinsDir, safeFileName(session)+".yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		s.pinsLoaded[session] = true
		return
	}
	var m map[string]string
	if err := yamlv3.Unmarshal(b, &m); err != nil {
		s.pinsLoaded[session] = true
		return
	}
	s.pins[session] = m
	s.pinsLoaded[session] = true
}

func (s *serverState) savePins(session string) {
	if s.pinsDir == "" {
		return
	}
	if err := os.MkdirAll(s.pinsDir, 0o755); err != nil {
		return
	}
	path := filepath.Join(s.pinsDir, safeFileName(session)+".yaml")
	b, err := yamlv3.Marshal(s.pins[session])
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}

func (s *serverState) setPin(session, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		session = "default"
	}
	s.loadPins(session)
	if s.pins[session] == nil {
		s.pins[session] = make(map[string]string)
	}
	s.pins[session][key] = value
	s.savePins(session)
}

func (s *serverState) getPins(session string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		session = "default"
	}
	s.loadPins(session)
	out := make(map[string]string)
	for k, v := range s.pins[session] {
		out[k] = v
	}
	return out
}

func (s *serverState) deletePin(session, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		session = "default"
	}
	s.loadPins(session)
	if s.pins[session] == nil {
		return
	}
	delete(s.pins[session], key)
	s.savePins(session)
}

func (s *serverState) getPresenceSnapshot(session string) []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		session = "default"
	}
	ss, ok := s.sessions[session]
	if !ok {
		return nil
	}
	out := make([]map[string]string, 0, len(ss.agents))
	for _, a := range ss.agents {
		out = append(out, map[string]string{
			"agent_id":  a.id,
			"session":   a.session,
			"last_seen": a.lastSeen.Format(time.RFC3339),
			"last_from": a.lastFrom,
			"last_to":   a.lastTo,
			"last_msg":  a.lastMsgTxt,
			"last_msg_at": func() string {
				if a.lastMsgAt.IsZero() {
					return ""
				}
				return a.lastMsgAt.Format(time.RFC3339)
			}(),
		})
	}
	return out
}

func (s *serverState) getHistory(session, thread string, last int, since int64) []msgRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if thread != "" {
		h := s.history[thread]
		return filterHistory(h, last, since)
	}
	var out []msgRecord
	for _, h := range s.history {
		out = append(out, h...)
	}
	return filterHistory(out, last, since)
}

func filterHistory(h []msgRecord, last int, since int64) []msgRecord {
	out := make([]msgRecord, 0, len(h))
	for _, r := range h {
		if since > 0 && r.Timestamp < since {
			continue
		}
		out = append(out, r)
	}
	if last > 0 && len(out) > last {
		out = out[len(out)-last:]
	}
	return out
}

func (s *serverState) appendHistory(thread string, rec msgRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.history[thread]
	h = append(h, rec)
	if len(h) > s.maxPerThread {
		h = h[len(h)-s.maxPerThread:]
	}
	s.history[thread] = h
}

func (s *serverState) saveHistory(rec msgRecord) {
	if s.historyDir == "" {
		return
	}
	if err := os.MkdirAll(s.historyDir, 0o755); err != nil {
		log.Printf("history mkdir: %v", err)
		return
	}
	name := fmt.Sprintf("%s.jsonl", safeFileName(rec.Thread))
	if rec.Thread == "" {
		name = "default.jsonl"
	}
	path := filepath.Join(s.historyDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("history open: %v", err)
		return
	}
	defer func() { _ = f.Close() }()
	b, _ := json.Marshal(rec)
	if _, err := f.Write(append(b, '\n')); err != nil {
		log.Printf("history write: %v", err)
	}
}

func safeFileName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "default"
	}
	s = strings.ReplaceAll(s, "..", "")
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		if r >= '0' && r <= '9' {
			return r
		}
		if r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
	return s
}

func readHistoryFile(historyDir, thread string, last int, since int64) ([]msgRecord, error) {
	name := fmt.Sprintf("%s.jsonl", safeFileName(thread))
	if thread == "" {
		name = "default.jsonl"
	}
	path := filepath.Join(historyDir, name)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []msgRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		var rec msgRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if since > 0 && rec.Timestamp < since {
			continue
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if last > 0 && len(out) > last {
		out = out[len(out)-last:]
	}
	return out, nil
}

func writeJSON(conn net.Conn, env envelope) error {
	env.Timestamp = time.Now().Unix()
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = conn.Write(append(b, '\n'))
	return err
}

func newMsgID() string {
	return fmt.Sprintf("%d-%06d", time.Now().UnixNano(), rand.Intn(1000000))
}

func parseInt(s string) int {
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func filterExpired(envs []envelope) []envelope {
	now := time.Now().Unix()
	out := make([]envelope, 0, len(envs))
	for _, e := range envs {
		if e.ExpiresAt > 0 && e.ExpiresAt < now {
			continue
		}
		out = append(out, e)
	}
	return out
}

func handleConn(s *serverState, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	var agentID string
	var sessionID string

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("read: %v", err)
			}
			if agentID != "" {
				s.unregisterAgent(sessionID, agentID)
			}
			return
		}
		line = bytesTrim(line)
		if len(line) == 0 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			_ = writeJSON(conn, envelope{Type: "error", Text: "invalid json"})
			continue
		}
		switch env.Type {
		case "hello":
			if env.AgentID == "" {
				_ = writeJSON(conn, envelope{Type: "error", Text: "missing agent_id"})
				continue
			}
			agentID = env.AgentID
			sessionID = env.Session
			queued := s.registerAgent(sessionID, agentID, conn)
			_ = writeJSON(conn, envelope{Type: "hello_ack", AgentID: agentID, Session: sessionID})
			for _, q := range filterExpired(queued) {
				if q.AgentID == agentID {
					continue
				}
				_ = writeJSON(conn, q)
			}
		case "ping":
			if agentID != "" {
				s.updatePresence(sessionID, agentID)
			}
			_ = writeJSON(conn, envelope{Type: "pong"})
		case "presence":
			sess := env.Session
			if sess == "" {
				sess = sessionID
			}
			snapshot := s.getPresenceSnapshot(sess)
			payload, _ := json.Marshal(snapshot)
			_ = writeJSON(conn, envelope{Type: "presence", Text: string(payload)})
		case "pin_set":
			sess := env.Session
			if sess == "" {
				sess = sessionID
			}
			key := ""
			val := ""
			if env.Meta != nil {
				key = env.Meta["key"]
				val = env.Meta["value"]
			}
			if key == "" {
				_ = writeJSON(conn, envelope{Type: "error", Text: "missing key"})
				continue
			}
			s.setPin(sess, key, val)
			_ = writeJSON(conn, envelope{Type: "pin_ack", Status: "ok"})
		case "pin_get":
			sess := env.Session
			if sess == "" {
				sess = sessionID
			}
			key := ""
			if env.Meta != nil {
				key = env.Meta["key"]
			}
			pins := s.getPins(sess)
			if key != "" {
				val := pins[key]
				payload, _ := json.Marshal(map[string]string{key: val})
				_ = writeJSON(conn, envelope{Type: "pin", Text: string(payload)})
				continue
			}
			payload, _ := json.Marshal(pins)
			_ = writeJSON(conn, envelope{Type: "pin", Text: string(payload)})
		case "pin_del":
			sess := env.Session
			if sess == "" {
				sess = sessionID
			}
			key := ""
			if env.Meta != nil {
				key = env.Meta["key"]
			}
			if key == "" {
				_ = writeJSON(conn, envelope{Type: "error", Text: "missing key"})
				continue
			}
			s.deletePin(sess, key)
			_ = writeJSON(conn, envelope{Type: "pin_ack", Status: "ok"})
		case "history":
			sess := env.Session
			if sess == "" {
				sess = sessionID
			}
			thread := env.Thread
			last := 0
			since := int64(0)
			if env.Meta != nil {
				last = parseInt(env.Meta["last"])
				since = parseInt64(env.Meta["since"])
			}
			hist := s.getHistory(sess, thread, last, since)
			if len(hist) == 0 && thread != "" && s.historyDir != "" {
				if h2, err := readHistoryFile(s.historyDir, thread, last, since); err == nil {
					hist = h2
				}
			}
			payload, _ := json.Marshal(hist)
			_ = writeJSON(conn, envelope{Type: "history", Text: string(payload)})
		case "msg":
			if agentID == "" {
				_ = writeJSON(conn, envelope{Type: "error", Text: "not registered"})
				continue
			}
			if env.To == "" {
				_ = writeJSON(conn, envelope{Type: "error", Text: "missing to"})
				continue
			}
			thread := env.Thread
			if thread == "" {
				thread = fmt.Sprintf("%s__%s__%s", sessionID, agentID, env.To)
			}
			msgID := env.MsgID
			if msgID == "" {
				msgID = newMsgID()
			}
			rec := msgRecord{
				From:      agentID,
				To:        env.To,
				Text:      env.Text,
				Thread:    thread,
				Meta:      env.Meta,
				MsgID:     msgID,
				Timestamp: time.Now().Unix(),
			}
			s.appendHistory(thread, rec)
			s.saveHistory(rec)
			s.noteLastMessage(sessionID, agentID, env.To, env.Text)
			msgEnv := envelope{
				Type:    "msg",
				AgentID: agentID,
				Session: sessionID,
				Text:    env.Text,
				Thread:  thread,
				Meta:    env.Meta,
				MsgID:   msgID,
			}
			if s.queueTTL > 0 {
				msgEnv.ExpiresAt = time.Now().Add(s.queueTTL).Unix()
			}
			s.mu.Lock()
			ss := s.sessions[sessionID]
			targets := make([]*agentConn, 0)
			if ss != nil {
				if env.To == "*" || env.To == "all" {
					for id, a := range ss.agents {
						if id == agentID {
							continue
						}
						targets = append(targets, a)
					}
				} else {
					if t := ss.agents[env.To]; t != nil {
						targets = append(targets, t)
					}
				}
			}
			s.mu.Unlock()
			if env.To == "*" || env.To == "all" {
				s.addBroadcast(sessionID, msgEnv)
				for _, t := range targets {
					_ = writeJSON(t.conn, msgEnv)
				}
				_ = writeJSON(conn, envelope{Type: "ack", Status: "broadcast", MsgID: msgID})
				continue
			}
			if len(targets) == 0 {
				if env.Meta != nil && env.Meta["no_queue"] == "true" {
					_ = writeJSON(conn, envelope{Type: "ack", Status: "offline", MsgID: msgID})
					continue
				}
				s.queueOffline(sessionID, env.To, msgEnv)
				_ = writeJSON(conn, envelope{Type: "ack", Status: "queued", MsgID: msgID})
				continue
			}
			for _, t := range targets {
				_ = writeJSON(t.conn, msgEnv)
			}
			_ = writeJSON(conn, envelope{Type: "ack", Status: "delivered", MsgID: msgID})
		default:
			_ = writeJSON(conn, envelope{Type: "error", Text: "unknown type"})
		}
	}
}

func bytesTrim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func runServer(addr, historyDir string, maxHistory int, queueTTL time.Duration) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	log.Printf("relay listening on %s", addr)
	state := newServerState(historyDir, maxHistory, queueTTL)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handleConn(state, conn)
	}
}

func runClient(addr, agentID, session string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if session == "" {
		session = "default"
	}
	if err := writeJSON(conn, envelope{Type: "hello", AgentID: agentID, Session: session}); err != nil {
		return err
	}
	go func() {
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			fmt.Println(sc.Text())
		}
	}()
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		line := input.Text()
		if strings.HasPrefix(line, "/ping") {
			_ = writeJSON(conn, envelope{Type: "ping"})
			continue
		}
		if strings.HasPrefix(line, "/presence") {
			_ = writeJSON(conn, envelope{Type: "presence", Session: session})
			continue
		}
		if strings.HasPrefix(line, "/msg ") {
			parts := strings.SplitN(line, " ", 3)
			if len(parts) < 3 {
				fmt.Fprintln(os.Stderr, "usage: /msg <agent_id> <text> [thread]")
				continue
			}
			to := parts[1]
			rest := parts[2]
			text, thread := splitTextThread(rest)
			_ = writeJSON(conn, envelope{Type: "msg", To: to, Text: text, Thread: thread})
			continue
		}
		fmt.Fprintln(os.Stderr, "commands: /msg /presence /ping")
	}
	return input.Err()
}

func runSend(addr, fromID, session, to, text, thread string, timeout time.Duration, noQueue bool, mode outputMode, quiet bool) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if session == "" {
		session = "default"
	}
	if err := writeJSON(conn, envelope{Type: "hello", AgentID: fromID, Session: session}); err != nil {
		return err
	}
	meta := map[string]string{}
	if noQueue {
		meta["no_queue"] = "true"
	}
	if err := writeJSON(conn, envelope{Type: "msg", To: to, Text: text, Thread: thread, Meta: meta}); err != nil {
		return err
	}
	if timeout <= 0 {
		return nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var env envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			continue
		}
		if env.Type == "ack" || env.Type == "error" {
			if !quiet {
				if env.Type == "error" {
					fmt.Println(env.Text)
				} else {
					fmt.Println(formatAck(env, mode))
				}
			}
			return nil
		}
	}
	return sc.Err()
}

func runWait(addr, agentID, session, thread string, timeout time.Duration, mode outputMode, quiet bool) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if session == "" {
		session = "default"
	}
	if err := writeJSON(conn, envelope{Type: "hello", AgentID: agentID, Session: session}); err != nil {
		return err
	}
	if timeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
	}
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var env envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			continue
		}
		if env.Type != "msg" {
			continue
		}
		if thread != "" && env.Thread != thread {
			continue
		}
		if !quiet {
			fmt.Println(formatMsg(env, mode))
		}
		return nil
	}
	return sc.Err()
}

func runPresence(addr, agentID, session string, mode outputMode, quiet bool) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if agentID == "" {
		agentID = fmt.Sprintf("cli-%s", newMsgID())
	}
	if session == "" {
		session = "default"
	}
	if err := writeJSON(conn, envelope{Type: "hello", AgentID: agentID, Session: session}); err != nil {
		return err
	}
	if err := writeJSON(conn, envelope{Type: "presence", Session: session}); err != nil {
		return err
	}
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var env envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			continue
		}
		if env.Type != "presence" {
			continue
		}
		if quiet {
			return nil
		}
		if mode == outputJSON {
			fmt.Println(env.Text)
			return nil
		}
		var rows []map[string]string
		if err := json.Unmarshal([]byte(env.Text), &rows); err != nil {
			fmt.Println(env.Text)
			return nil
		}
		for _, r := range rows {
			if mode == outputVerbose {
				fmt.Printf("%s last_seen=%s last_from=%s last_to=%s last_msg=%s\n",
					r["agent_id"], r["last_seen"], r["last_from"], r["last_to"], r["last_msg"])
			} else {
				fmt.Println(r["agent_id"])
			}
		}
		return nil
	}
	return sc.Err()
}

func runHistory(addr, agentID, session, thread string, last int, since int64, mode outputMode, quiet bool) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if agentID == "" {
		agentID = fmt.Sprintf("cli-%s", newMsgID())
	}
	if session == "" {
		session = "default"
	}
	meta := map[string]string{}
	if last > 0 {
		meta["last"] = strconv.Itoa(last)
	}
	if since > 0 {
		meta["since"] = strconv.FormatInt(since, 10)
	}
	if err := writeJSON(conn, envelope{Type: "hello", AgentID: agentID, Session: session}); err != nil {
		return err
	}
	if err := writeJSON(conn, envelope{Type: "history", Session: session, Thread: thread, Meta: meta}); err != nil {
		return err
	}
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var env envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			continue
		}
		if env.Type != "history" {
			continue
		}
		if quiet {
			return nil
		}
		if mode == outputJSON {
			fmt.Println(env.Text)
			return nil
		}
		var rows []msgRecord
		if err := json.Unmarshal([]byte(env.Text), &rows); err != nil {
			fmt.Println(env.Text)
			return nil
		}
		for _, r := range rows {
			if mode == outputVerbose {
				fmt.Printf("[%s] %s -> %s: %s (msg_id=%s)\n", r.Thread, r.From, r.To, r.Text, r.MsgID)
			} else {
				fmt.Printf("[%s] %s: %s\n", r.Thread, r.From, r.Text)
			}
		}
		return nil
	}
	return sc.Err()
}

func runPin(addr, agentID, session, action, key, value string, mode outputMode, quiet bool) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if agentID == "" {
		agentID = fmt.Sprintf("cli-%s", newMsgID())
	}
	if session == "" {
		session = "default"
	}
	if err := writeJSON(conn, envelope{Type: "hello", AgentID: agentID, Session: session}); err != nil {
		return err
	}
	meta := map[string]string{}
	if key != "" {
		meta["key"] = key
	}
	if value != "" {
		meta["value"] = value
	}
	switch action {
	case "set":
		if key == "" {
			return fmt.Errorf("missing key")
		}
		if err := writeJSON(conn, envelope{Type: "pin_set", Session: session, Meta: meta}); err != nil {
			return err
		}
	case "get":
		if err := writeJSON(conn, envelope{Type: "pin_get", Session: session, Meta: meta}); err != nil {
			return err
		}
	case "list":
		if err := writeJSON(conn, envelope{Type: "pin_get", Session: session}); err != nil {
			return err
		}
	case "del":
		if key == "" {
			return fmt.Errorf("missing key")
		}
		if err := writeJSON(conn, envelope{Type: "pin_del", Session: session, Meta: meta}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown pin action: %s", action)
	}
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var env envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			continue
		}
		if env.Type == "pin_ack" {
			if !quiet {
				fmt.Println(formatAck(envelope{Status: "ok"}, mode))
			}
			return nil
		}
		if env.Type == "pin" {
			if quiet {
				return nil
			}
			if mode == outputJSON {
				fmt.Println(env.Text)
				return nil
			}
			var pins map[string]string
			if err := json.Unmarshal([]byte(env.Text), &pins); err != nil {
				fmt.Println(env.Text)
				return nil
			}
			for k, v := range pins {
				if mode == outputVerbose {
					fmt.Printf("%s=%s\n", k, v)
				} else {
					fmt.Printf("%s: %s\n", k, v)
				}
			}
			return nil
		}
	}
	return sc.Err()
}

func runDoctor(addr, agentID, session string, mode outputMode, quiet bool) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if agentID == "" {
		agentID = fmt.Sprintf("cli-%s", newMsgID())
	}
	if session == "" {
		session = "default"
	}
	if err := writeJSON(conn, envelope{Type: "hello", AgentID: agentID, Session: session}); err != nil {
		return err
	}
	if err := writeJSON(conn, envelope{Type: "ping"}); err != nil {
		return err
	}
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var env envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			continue
		}
		if env.Type != "pong" {
			continue
		}
		if quiet {
			return nil
		}
		if mode == outputJSON {
			fmt.Println(sc.Text())
			return nil
		}
		fmt.Println("ok")
		return nil
	}
	return sc.Err()
}

func splitTextThread(rest string) (string, string) {
	parts := strings.SplitN(rest, " :: ", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return rest, ""
}

type outputMode int

const (
	outputDefault outputMode = iota
	outputVerbose
	outputJSON
)

func formatMsg(env envelope, mode outputMode) string {
	switch mode {
	case outputJSON:
		b, _ := json.Marshal(env)
		return string(b)
	case outputVerbose:
		return fmt.Sprintf("[%s/%s] %s: %s (msg_id=%s)",
			env.Session, env.Thread, env.AgentID, env.Text, env.MsgID)
	default:
		return fmt.Sprintf("[%s] %s: %s", env.Thread, env.AgentID, env.Text)
	}
}

func formatAck(env envelope, mode outputMode) string {
	switch mode {
	case outputJSON:
		b, _ := json.Marshal(env)
		return string(b)
	case outputVerbose:
		return fmt.Sprintf("ack status=%s msg_id=%s", env.Status, env.MsgID)
	default:
		if env.Status != "" {
			return fmt.Sprintf("ack: %s", env.Status)
		}
		return "ack"
	}
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agentzap", "config.yaml"), nil
}

func loadConfig() (config, error) {
	path, err := configPath()
	if err != nil {
		return config{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}
	var cfg config
	if err := yamlv3.Unmarshal(b, &cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func saveConfig(cfg config, force bool) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config already exists: %s", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := yamlv3.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func main() {
	if len(os.Args) == 1 {
		usage()
		return
	}
	serverCmd := flag.NewFlagSet("server", flag.ExitOnError)
	serverAddr := serverCmd.String("addr", defaultAddr, "listen address")
	serverHistory := serverCmd.String("history", defaultHistory, "history directory (jsonl)")
	serverMax := serverCmd.Int("max-history", defaultMaxHistory, "max messages per thread in memory")
	serverQueueTTL := serverCmd.Duration("queue-ttl", 0, "expire queued messages after duration (0=disable)")

	clientCmd := flag.NewFlagSet("client", flag.ExitOnError)
	clientAddr := clientCmd.String("addr", defaultAddr, "relay address")
	clientID := clientCmd.String("id", "", "agent id")
	clientSession := clientCmd.String("session", "default", "session id")

	sendCmd := flag.NewFlagSet("send", flag.ExitOnError)
	sendAddr := sendCmd.String("addr", defaultAddr, "relay address")
	sendFrom := sendCmd.String("from", "", "sender agent id")
	sendTo := sendCmd.String("to", "", "target agent id (or all)")
	sendSession := sendCmd.String("session", "default", "session id")
	sendText := sendCmd.String("text", "", "message text")
	sendThread := sendCmd.String("thread", "", "thread id")
	sendTimeout := sendCmd.Duration("timeout", 5*time.Second, "wait for ack (0 to skip)")
	sendNoQueue := sendCmd.Bool("no-queue", false, "fail if target offline (no queue)")
	sendJSON := sendCmd.Bool("json", false, "json output")
	sendVerbose := sendCmd.Bool("verbose", false, "verbose output")
	sendQuiet := sendCmd.Bool("quiet", false, "suppress output")

	waitCmd := flag.NewFlagSet("wait", flag.ExitOnError)
	waitAddr := waitCmd.String("addr", defaultAddr, "relay address")
	waitID := waitCmd.String("id", "", "agent id")
	waitSession := waitCmd.String("session", "default", "session id")
	waitThread := waitCmd.String("thread", "", "thread id to filter")
	waitTimeout := waitCmd.Duration("timeout", 0, "timeout to wait for a message (0 = no timeout)")
	waitJSON := waitCmd.Bool("json", false, "json output")
	waitVerbose := waitCmd.Bool("verbose", false, "verbose output")
	waitQuiet := waitCmd.Bool("quiet", false, "suppress output")

	initCmd := flag.NewFlagSet("init", flag.ExitOnError)
	initAddr := initCmd.String("addr", defaultAddr, "relay address")
	initSession := initCmd.String("session", "default", "session id")
	initID := initCmd.String("id", "", "agent id")
	initForce := initCmd.Bool("force", false, "overwrite existing config")

	presenceCmd := flag.NewFlagSet("presence", flag.ExitOnError)
	presenceAddr := presenceCmd.String("addr", defaultAddr, "relay address")
	presenceID := presenceCmd.String("id", "", "agent id")
	presenceSession := presenceCmd.String("session", "default", "session id")
	presenceJSON := presenceCmd.Bool("json", false, "json output")
	presenceVerbose := presenceCmd.Bool("verbose", false, "verbose output")
	presenceQuiet := presenceCmd.Bool("quiet", false, "suppress output")

	historyCmd := flag.NewFlagSet("history", flag.ExitOnError)
	historyAddr := historyCmd.String("addr", defaultAddr, "relay address")
	historyID := historyCmd.String("id", "", "agent id")
	historySession := historyCmd.String("session", "default", "session id")
	historyThread := historyCmd.String("thread", "", "thread id")
	historyLast := historyCmd.Int("last", 0, "last N messages")
	historySince := historyCmd.Int64("since", 0, "since unix timestamp")
	historyJSON := historyCmd.Bool("json", false, "json output")
	historyVerbose := historyCmd.Bool("verbose", false, "verbose output")
	historyQuiet := historyCmd.Bool("quiet", false, "suppress output")

	pinCmd := flag.NewFlagSet("pin", flag.ExitOnError)
	pinAddr := pinCmd.String("addr", defaultAddr, "relay address")
	pinID := pinCmd.String("id", "", "agent id")
	pinSession := pinCmd.String("session", "default", "session id")
	pinJSON := pinCmd.Bool("json", false, "json output")
	pinVerbose := pinCmd.Bool("verbose", false, "verbose output")
	pinQuiet := pinCmd.Bool("quiet", false, "suppress output")

	doctorCmd := flag.NewFlagSet("doctor", flag.ExitOnError)
	doctorAddr := doctorCmd.String("addr", defaultAddr, "relay address")
	doctorID := doctorCmd.String("id", "", "agent id")
	doctorSession := doctorCmd.String("session", "default", "session id")
	doctorJSON := doctorCmd.Bool("json", false, "json output")
	doctorVerbose := doctorCmd.Bool("verbose", false, "verbose output")
	doctorQuiet := doctorCmd.Bool("quiet", false, "suppress output")

	switch os.Args[1] {
	case "server":
		_ = serverCmd.Parse(os.Args[2:])
		if err := runServer(*serverAddr, *serverHistory, *serverMax, *serverQueueTTL); err != nil {
			log.Fatal(err)
		}
	case "client":
		_ = clientCmd.Parse(os.Args[2:])
		cfg, _ := loadConfig()
		if *clientAddr == defaultAddr && cfg.Addr != "" {
			*clientAddr = cfg.Addr
		}
		if *clientSession == "default" && cfg.Session != "" {
			*clientSession = cfg.Session
		}
		if *clientID == "" && cfg.ID != "" {
			*clientID = cfg.ID
		}
		if *clientID == "" {
			fmt.Fprintln(os.Stderr, "missing --id")
			os.Exit(2)
		}
		if err := runClient(*clientAddr, *clientID, *clientSession); err != nil {
			log.Fatal(err)
		}
	case "send":
		_ = sendCmd.Parse(os.Args[2:])
		cfg, _ := loadConfig()
		if *sendAddr == defaultAddr && cfg.Addr != "" {
			*sendAddr = cfg.Addr
		}
		if *sendSession == "default" && cfg.Session != "" {
			*sendSession = cfg.Session
		}
		if *sendFrom == "" && cfg.ID != "" {
			*sendFrom = cfg.ID
		}
		if *sendFrom == "" || *sendTo == "" {
			fmt.Fprintln(os.Stderr, "missing --from or --to")
			os.Exit(2)
		}
		mode := outputDefault
		if *sendVerbose {
			mode = outputVerbose
		}
		if *sendJSON {
			mode = outputJSON
		}
		if err := runSend(*sendAddr, *sendFrom, *sendSession, *sendTo, *sendText, *sendThread, *sendTimeout, *sendNoQueue, mode, *sendQuiet); err != nil {
			log.Fatal(err)
		}
	case "wait":
		_ = waitCmd.Parse(os.Args[2:])
		cfg, _ := loadConfig()
		if *waitAddr == defaultAddr && cfg.Addr != "" {
			*waitAddr = cfg.Addr
		}
		if *waitSession == "default" && cfg.Session != "" {
			*waitSession = cfg.Session
		}
		if *waitID == "" && cfg.ID != "" {
			*waitID = cfg.ID
		}
		if *waitID == "" {
			fmt.Fprintln(os.Stderr, "missing --id")
			os.Exit(2)
		}
		mode := outputDefault
		if *waitVerbose {
			mode = outputVerbose
		}
		if *waitJSON {
			mode = outputJSON
		}
		if err := runWait(*waitAddr, *waitID, *waitSession, *waitThread, *waitTimeout, mode, *waitQuiet); err != nil {
			log.Fatal(err)
		}
	case "init":
		_ = initCmd.Parse(os.Args[2:])
		cfg := config{
			Addr:    *initAddr,
			Session: *initSession,
			ID:      *initID,
		}
		if err := saveConfig(cfg, *initForce); err != nil {
			log.Fatal(err)
		}
	case "presence", "who":
		_ = presenceCmd.Parse(os.Args[2:])
		cfg, _ := loadConfig()
		if *presenceAddr == defaultAddr && cfg.Addr != "" {
			*presenceAddr = cfg.Addr
		}
		if *presenceSession == "default" && cfg.Session != "" {
			*presenceSession = cfg.Session
		}
		if *presenceID == "" && cfg.ID != "" {
			*presenceID = cfg.ID
		}
		mode := outputDefault
		if *presenceVerbose {
			mode = outputVerbose
		}
		if *presenceJSON {
			mode = outputJSON
		}
		if err := runPresence(*presenceAddr, *presenceID, *presenceSession, mode, *presenceQuiet); err != nil {
			log.Fatal(err)
		}
	case "history":
		_ = historyCmd.Parse(os.Args[2:])
		cfg, _ := loadConfig()
		if *historyAddr == defaultAddr && cfg.Addr != "" {
			*historyAddr = cfg.Addr
		}
		if *historySession == "default" && cfg.Session != "" {
			*historySession = cfg.Session
		}
		if *historyID == "" && cfg.ID != "" {
			*historyID = cfg.ID
		}
		mode := outputDefault
		if *historyVerbose {
			mode = outputVerbose
		}
		if *historyJSON {
			mode = outputJSON
		}
		if err := runHistory(*historyAddr, *historyID, *historySession, *historyThread, *historyLast, *historySince, mode, *historyQuiet); err != nil {
			log.Fatal(err)
		}
	case "pin", "note":
		_ = pinCmd.Parse(os.Args[2:])
		action := ""
		key := ""
		value := ""
		if len(pinCmd.Args()) > 0 {
			action = pinCmd.Args()[0]
		}
		if len(pinCmd.Args()) > 1 {
			key = pinCmd.Args()[1]
		}
		if len(pinCmd.Args()) > 2 {
			value = pinCmd.Args()[2]
		}
		if action == "" {
			fmt.Fprintln(os.Stderr, "pin action required: set|get|list|del")
			os.Exit(2)
		}
		cfg, _ := loadConfig()
		if *pinAddr == defaultAddr && cfg.Addr != "" {
			*pinAddr = cfg.Addr
		}
		if *pinSession == "default" && cfg.Session != "" {
			*pinSession = cfg.Session
		}
		if *pinID == "" && cfg.ID != "" {
			*pinID = cfg.ID
		}
		mode := outputDefault
		if *pinVerbose {
			mode = outputVerbose
		}
		if *pinJSON {
			mode = outputJSON
		}
		if err := runPin(*pinAddr, *pinID, *pinSession, action, key, value, mode, *pinQuiet); err != nil {
			log.Fatal(err)
		}
	case "doctor", "status":
		_ = doctorCmd.Parse(os.Args[2:])
		cfg, _ := loadConfig()
		if *doctorAddr == defaultAddr && cfg.Addr != "" {
			*doctorAddr = cfg.Addr
		}
		if *doctorSession == "default" && cfg.Session != "" {
			*doctorSession = cfg.Session
		}
		if *doctorID == "" && cfg.ID != "" {
			*doctorID = cfg.ID
		}
		mode := outputDefault
		if *doctorVerbose {
			mode = outputVerbose
		}
		if *doctorJSON {
			mode = outputJSON
		}
		if err := runDoctor(*doctorAddr, *doctorID, *doctorSession, mode, *doctorQuiet); err != nil {
			log.Fatal(err)
		}
	case "config":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "config action required: get|set|show")
			os.Exit(2)
		}
		action := os.Args[2]
		switch action {
		case "show":
			cfg, err := loadConfig()
			if err != nil {
				log.Fatal(err)
			}
			b, _ := yamlv3.Marshal(cfg)
			fmt.Print(string(b))
		case "get":
			if len(os.Args) < 4 {
				fmt.Fprintln(os.Stderr, "config get <key>")
				os.Exit(2)
			}
			key := os.Args[3]
			cfg, err := loadConfig()
			if err != nil {
				log.Fatal(err)
			}
			switch key {
			case "addr":
				fmt.Println(cfg.Addr)
			case "session":
				fmt.Println(cfg.Session)
			case "id":
				fmt.Println(cfg.ID)
			default:
				fmt.Fprintln(os.Stderr, "unknown key")
				os.Exit(2)
			}
		case "set":
			if len(os.Args) < 5 {
				fmt.Fprintln(os.Stderr, "config set <key> <value>")
				os.Exit(2)
			}
			key := os.Args[3]
			value := os.Args[4]
			cfg, _ := loadConfig()
			switch key {
			case "addr":
				cfg.Addr = value
			case "session":
				cfg.Session = value
			case "id":
				cfg.ID = value
			default:
				fmt.Fprintln(os.Stderr, "unknown key")
				os.Exit(2)
			}
			if err := saveConfig(cfg, true); err != nil {
				log.Fatal(err)
			}
		default:
			fmt.Fprintln(os.Stderr, "config action required: get|set|show")
			os.Exit(2)
		}
	case "help", "-h", "--help":
		usage()
	default:
		usage()
	}
}

func usage() {
	fmt.Println(`agentzap relay

USAGE:
  agentzap server --addr 0.0.0.0:9800 --history ./data/history
  agentzap init --addr 100.x.y.z:9800 --session alpha --id agent_x
  agentzap client --id agent_x --session alpha
  agentzap send --from agent_x --to agent_y --session alpha --text "hi"
  agentzap wait --id agent_y --session alpha --timeout 30s
  agentzap presence --session alpha
  agentzap history --session alpha --thread topic-1 --last 10
  agentzap pin set --session alpha topic "agent glossary"
  agentzap config show
  agentzap doctor

PROTOCOL (JSONL):
  hello:    {"type":"hello","agent_id":"agent_x","session":"alpha"}
  msg:      {"type":"msg","to":"agent_y","text":"hi","thread":"topic-1"}
  msg(all): {"type":"msg","to":"all","text":"hi","thread":"topic-1"}
  ping:     {"type":"ping"}
  presence: {"type":"presence","session":"alpha"}
  pin_set:  {"type":"pin_set","session":"alpha","meta":{"key":"k","value":"v"}}
  history:  {"type":"history","session":"alpha","thread":"topic-1","meta":{"last":"10"}}

CLIENT COMMANDS:
  /msg <agent_id|all|*> <text> [thread]
  /presence
  /ping

OUTPUT:
  --json   structured output
  --verbose detailed output
  --quiet  suppress output

AGENT BEHAVIOR:
  - Use "send" for one-off messages without a persistent client.
  - Use "wait" to block until a message arrives, then reply using the same "thread".
  - Use a shared "session" to allow multi-agent group chats.
  - See BASE_PROMPT.md for the recommended agent prompt.`)
}
