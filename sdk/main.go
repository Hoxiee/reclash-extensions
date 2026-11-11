package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Info identifies an extension in `version`.
type Info struct {
	ID      string
	Version string
}

// Node is what `describe` reports for one link.
type Node struct {
	Name   string
	Server string
	UDP    bool
}

// Extension is one protocol. Describe must not touch the network. Start brings the protocol up and returns the
// Proxy the front serves; it should not wait for the remote side, since Core times out a start that has no ready
// event. A Proxy that implements io.Closer is closed on stop.
type Extension interface {
	Info() Info
	Describe(link string) (Node, error)
	Start(ctx context.Context, rt *Runtime) (Proxy, error)
}

// Runtime is what a running extension gets from the SDK.
type Runtime struct {
	Link     string
	DataDir  string
	LogLevel string
	// Egress is the only way to reach the network: every protocol socket dials through it.
	Egress *Egress
	Log    *Logger

	events *eventWriter
	fatal  chan error
}

// Status reports the protocol's state to Core (StateOK or StateDegraded). Repeats of the last state are dropped;
// a status set before ready is sent right after it.
func (r *Runtime) Status(state, message string) {
	r.events.status(state, r.Log.Redact(message))
}

// Fatal stops the extension with err: Core gets a fatal event and the process exits non-zero.
func (r *Runtime) Fatal(err error) {
	select {
	case r.fatal <- err:
	default:
	}
}

// stopGrace bounds the shutdown after stdin closes; Core kills the process group after its own grace.
const stopGrace = 2500 * time.Millisecond

// Main runs the command in os.Args and exits.
func Main(ext Extension) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := Run(ctx, ext, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// Run executes one command (args[0]) and returns the exit code. Canceling ctx stops `run` like stdin EOF does.
func Run(ctx context.Context, ext Extension, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintf(stderr, "usage: %s run|describe|version\n", ext.Info().ID)
		return 2
	}
	switch args[0] {
	case CommandVersion:
		info := ext.Info()
		return writeJSON(stdout, stderr, VersionResponse{ID: info.ID, Version: info.Version, Protocol: []int{ProtocolVersion}})
	case CommandDescribe:
		return describe(ext, stdin, stdout, stderr)
	case CommandRun:
		return run(ctx, ext, stdin, stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "unknown command %q\n", args[0])
	return 2
}

func writeJSON(stdout, stderr io.Writer, v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := stdout.Write(append(b, '\n')); err != nil {
		return 1
	}
	return 0
}

func describe(ext Extension, stdin io.Reader, stdout, stderr io.Writer) int {
	var req DescribeRequest
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		_, _ = fmt.Fprintf(stderr, "describe: bad request: %v\n", err)
		return 1
	}
	resp := DescribeResponse{Nodes: make([]DescribedNode, len(req.Links))}
	for i, link := range req.Links {
		node, err := ext.Describe(link)
		if err != nil {
			log := NewLogger(io.Discard, LevelSilent)
			log.AddSecret(link, linkBody(link))
			resp.Nodes[i] = DescribedNode{Error: log.Redact(err.Error())}
			continue
		}
		resp.Nodes[i] = DescribedNode{Name: node.Name, Server: node.Server, UDP: node.UDP}
	}
	return writeJSON(stdout, stderr, resp)
}

// linkBody is the part of a link after its scheme, which on its own still carries the secrets.
func linkBody(link string) string {
	if _, body, ok := strings.Cut(link, "://"); ok {
		return body
	}
	return ""
}

func run(ctx context.Context, ext Extension, stdin io.Reader, stdout, stderr io.Writer) int {
	events := &eventWriter{w: stdout}
	in := bufio.NewReader(stdin)
	var req RunRequest
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		events.fatal("bad run request: " + err.Error())
		return 1
	}
	log := NewLogger(stderr, ParseLevel(req.LogLevel))
	log.AddSecret(req.Link, linkBody(req.Link), req.Egress.Username, req.Egress.Password)
	fail := func(err error) int {
		msg := log.Redact(err.Error())
		log.Errorf("%s", msg)
		events.fatal(msg)
		return 1
	}
	if req.Protocol != ProtocolVersion {
		return fail(fmt.Errorf("unsupported protocol %d", req.Protocol))
	}
	if req.Egress.SOCKS5 == "" {
		return fail(errors.New("run request has no egress"))
	}
	if req.DataDir != "" {
		if err := os.MkdirAll(req.DataDir, 0o700); err != nil {
			return fail(fmt.Errorf("data directory: %w", err))
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The decoder may have buffered past the request; whatever follows is ignored, EOF is the stop signal.
	stdinClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, in)
		close(stdinClosed)
	}()

	rt := &Runtime{
		Link:     req.Link,
		DataDir:  req.DataDir,
		LogLevel: req.LogLevel,
		Egress:   NewEgress(req.Egress),
		Log:      log,
		events:   events,
		fatal:    make(chan error, 1),
	}
	proxy, err := ext.Start(ctx, rt)
	if err != nil {
		return fail(err)
	}
	front, err := NewFront(req.Listen, proxy)
	if err != nil {
		closeWithin(proxy, stopGrace)
		return fail(err)
	}
	log.AddSecret(front.Password())
	events.ready(Event{Event: EventReady, SOCKS5: front.Addr(), Username: front.Username(), Password: front.Password(), UDP: front.UDP()})
	log.Infof("front listening on %s (udp %t)", front.Addr(), front.UDP())

	code := 0
	select {
	case <-stdinClosed:
		log.Infof("stdin closed, stopping")
	case <-ctx.Done():
		log.Infof("stopping")
	case err := <-rt.fatal:
		code = fail(err)
	}
	cancel()
	done := make(chan struct{})
	go func() {
		_ = front.Close()
		closeWithin(proxy, stopGrace)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopGrace):
		log.Warnf("shutdown did not finish in %s", stopGrace)
	}
	return code
}

// closeWithin closes p if it is an io.Closer, giving up after d.
func closeWithin(p Proxy, d time.Duration) {
	c, ok := p.(io.Closer)
	if !ok {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = c.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// eventWriter serializes stdout events and keeps ready first and fatal last.
type eventWriter struct {
	mu      sync.Mutex
	w       io.Writer
	isReady bool
	done    bool
	last    *Event
	pending *Event
}

func (e *eventWriter) writeLocked(ev Event) {
	b, _ := json.Marshal(ev)
	_, _ = e.w.Write(append(b, '\n'))
}

func (e *eventWriter) ready(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.isReady || e.done {
		return
	}
	e.isReady = true
	e.writeLocked(ev)
	if e.pending != nil {
		e.writeLocked(*e.pending)
		e.last, e.pending = e.pending, nil
	}
}

func (e *eventWriter) status(state, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ev := Event{Event: EventStatus, State: state, Message: message}
	if e.done {
		return
	}
	if !e.isReady {
		e.pending = &ev
		return
	}
	if e.last != nil && *e.last == ev {
		return
	}
	e.last = &ev
	e.writeLocked(ev)
}

func (e *eventWriter) fatal(message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return
	}
	e.done = true
	e.writeLocked(Event{Event: EventFatal, Message: message})
}
