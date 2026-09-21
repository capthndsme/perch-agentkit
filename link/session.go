// Package link keeps an agent's WebSocket session to the Perch controller:
// dial with the agent's credentials, keep the connection alive with pings,
// serve the controller's JSON-RPC requests, route its notifications, make
// calls of the agent's own and push on the schedule the controller sets.
//
// One Run is one session. Reconnecting, and what each way of ending means
// (revoked, replaced, dismissed, server restarting), is the daemon's policy:
// the AP daemon rejoins with a token, the collector waits for an admin.
package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/capthndsme/perch-agentkit/rpc"
)

// Close codes shared by the Perch protocols.
const (
	// CloseRevoked: the credentials this session presented are no longer
	// valid (agent forgotten, collector deleted or re-keyed).
	CloseRevoked websocket.StatusCode = 4001
	// CloseReplaced: a newer session with the same identity took over.
	CloseReplaced websocket.StatusCode = 4002
	// CloseDismissed: an admin dismissed this collector.
	CloseDismissed websocket.StatusCode = 4003
)

// Defaults for the zero values of Options.
const (
	DefaultDialTimeout  = 20 * time.Second
	DefaultPingInterval = 30 * time.Second
	DefaultPingTimeout  = 10 * time.Second
	DefaultWriteTimeout = 10 * time.Second
	DefaultReadLimit    = 4 << 20
	DefaultMaxInFlight  = 4
)

// ErrClosed is returned by calls on a session that has ended.
var ErrClosed = errors.New("link: session closed")

// Options configure one session.
type Options struct {
	// URL is the ws:// or wss:// endpoint (see WebSocketURL).
	URL string
	// Subprotocol is offered and required ("perch-ap.v1", …); empty offers none.
	Subprotocol string
	// Header is sent with the upgrade request (credentials, User-Agent).
	Header http.Header
	// HTTPClient performs the handshake (see NewHTTPClient); nil = default.
	HTTPClient *http.Client
	// Compression offers permessage-deflate without context takeover.
	Compression bool
	// ReadLimit caps one message from the controller (default 4 MiB).
	ReadLimit int64
	// Log gets connection-level lines; nil = slog.Default().
	Log *slog.Logger
	// Dispatcher serves the controller's requests; nil answers -32601.
	Dispatcher *rpc.Dispatcher
	// OnNotification gets the controller's notifications in frame order, on
	// the read goroutine: keep it quick (hand work off to a channel).
	OnNotification func(ctx context.Context, s *Session, m *rpc.Message)
	// OnOpen runs in its own goroutine once the session is reading, so it may
	// Call the controller. Its ctx ends with the session, and Run waits for
	// it to return: it must not outlive ctx.
	OnOpen func(ctx context.Context, s *Session)

	DialTimeout  time.Duration // default 20 s
	PingInterval time.Duration // default 30 s
	PingTimeout  time.Duration // default 10 s
	WriteTimeout time.Duration // default 10 s, per message
	MaxInFlight  int           // concurrent controller requests, default 4
}

func (o Options) withDefaults() Options {
	if o.DialTimeout <= 0 {
		o.DialTimeout = DefaultDialTimeout
	}
	if o.PingInterval <= 0 {
		o.PingInterval = DefaultPingInterval
	}
	if o.PingTimeout <= 0 {
		o.PingTimeout = DefaultPingTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = DefaultWriteTimeout
	}
	if o.ReadLimit <= 0 {
		o.ReadLimit = DefaultReadLimit
	}
	if o.MaxInFlight <= 0 {
		o.MaxInFlight = DefaultMaxInFlight
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Dispatcher == nil {
		o.Dispatcher = rpc.NewDispatcher()
	}
	return o
}

// Session is one open connection to the controller.
type Session struct {
	conn         *websocket.Conn
	log          *slog.Logger
	writeTimeout time.Duration
	ctx          context.Context

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *rpc.Message
	closed  bool
}

// Subprotocol is what the controller selected.
func (s *Session) Subprotocol() string { return s.conn.Subprotocol() }

// Context ends when the session does.
func (s *Session) Context() context.Context { return s.ctx }

// Notify sends a notification.
func (s *Session) Notify(method string, params any) error {
	b, err := rpc.Notification(method, params)
	if err != nil {
		return err
	}
	return s.write(b)
}

// NotifyRaw sends a notification whose params are already encoded JSON,
// without decoding and re-encoding them (large pushes).
func (s *Session) NotifyRaw(method string, params json.RawMessage) error {
	name, err := json.Marshal(method)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	b.Grow(len(params) + len(name) + 40)
	b.WriteString(`{"jsonrpc":"2.0","method":`)
	b.Write(name)
	b.WriteString(`,"params":`)
	b.Write(params)
	b.WriteByte('}')
	return s.write(b.Bytes())
}

// Call sends a request and waits for the controller's answer. An error
// response comes back as *rpc.Error; result may be nil to ignore the result.
func (s *Session) Call(ctx context.Context, method string, params, result any) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.nextID++
	id := s.nextID
	ch := make(chan *rpc.Message, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	frame, err := rpc.Request(id, method, params)
	if err != nil {
		return err
	}
	if err := s.write(frame); err != nil {
		return err
	}
	select {
	case m := <-ch:
		return decodeResult(m, result)
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		// The read loop delivers before the session ends, so an answer that
		// came in with the close is already waiting: prefer it.
		select {
		case m := <-ch:
			return decodeResult(m, result)
		default:
			return ErrClosed
		}
	}
}

func decodeResult(m *rpc.Message, result any) error {
	if m.Error != nil {
		return m.Error
	}
	if result != nil && len(m.Result) > 0 {
		return json.Unmarshal(m.Result, result)
	}
	return nil
}

// Close ends the session with a close frame.
func (s *Session) Close(code websocket.StatusCode, reason string) {
	_ = s.conn.Close(code, reason)
}

func (s *Session) write(b []byte) error {
	ctx, cancel := context.WithTimeout(s.ctx, s.writeTimeout)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, b)
}

func (s *Session) deliver(m *rpc.Message) {
	id, err := strconv.ParseInt(string(bytes.TrimSpace(m.ID)), 10, 64)
	if err != nil {
		s.log.Debug("response with a non-numeric id", "id", string(m.ID))
		return
	}
	s.mu.Lock()
	ch := s.pending[id]
	s.mu.Unlock()
	if ch == nil {
		s.log.Debug("response to no pending call", "id", id)
		return
	}
	select {
	case ch <- m:
	default:
	}
}

func (s *Session) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// Run dials once and serves the session until it ends. It returns nil when
// ctx is done (after a 1001 close), a *StatusError when the controller
// refused the upgrade, and otherwise the error that ended the session:
// websocket.CloseStatus(err) is the controller's close code, -1 for a
// dropped connection or a missed pong.
func Run(ctx context.Context, o Options) error {
	o = o.withDefaults()
	dopts := &websocket.DialOptions{HTTPClient: o.HTTPClient, HTTPHeader: o.Header}
	if o.Subprotocol != "" {
		dopts.Subprotocols = []string{o.Subprotocol}
	}
	if o.Compression {
		dopts.CompressionMode = websocket.CompressionNoContextTakeover
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, o.DialTimeout)
	conn, resp, err := websocket.Dial(dialCtx, o.URL, dopts)
	cancelDial()
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			se := ReadStatusError(resp)
			if resp.Body != nil {
				resp.Body.Close()
			}
			return se
		}
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(o.ReadLimit)

	// Reads use their own context: cancelling a Read's context tears the
	// connection down without a close frame, and on shutdown we want to say
	// goodbye (1001) instead.
	readCtx, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()
	sess := &Session{conn: conn, log: o.Log, writeTimeout: o.WriteTimeout, ctx: readCtx, pending: map[int64]chan *rpc.Message{}}
	defer sess.markClosed()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusGoingAway, "agent stopping")
			cancelRead()
		case <-done:
		}
	}()
	go pinger(readCtx, o, conn, cancelRead)
	// Run returns only once OnOpen has: nothing the session started (a
	// hello answered at the last moment, a push in flight) can report after
	// the daemon has moved on to the next session.
	var opened sync.WaitGroup
	if o.OnOpen != nil {
		opened.Add(1)
		go func() {
			defer opened.Done()
			o.OnOpen(readCtx, sess)
		}()
	}
	defer func() {
		cancelRead()
		opened.Wait()
	}()

	sem := make(chan struct{}, o.MaxInFlight)
	for {
		typ, data, err := conn.Read(readCtx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		msg, errResp := rpc.Decode(data)
		if errResp != nil {
			if b, err := json.Marshal(errResp); err == nil {
				_ = sess.write(b)
			}
			continue
		}
		switch {
		case msg.IsRequest():
			go func(m *rpc.Message) {
				sem <- struct{}{}
				defer func() { <-sem }()
				start := time.Now()
				resp := o.Dispatcher.Serve(readCtx, m)
				if resp == nil {
					return
				}
				if resp.Error != nil {
					o.Log.Info("request failed", "method", m.Method, "code", resp.Error.Code, "err", resp.Error.Message)
				} else {
					o.Log.Debug("request served", "method", m.Method, "ms", time.Since(start).Milliseconds())
				}
				b, err := json.Marshal(resp)
				if err != nil {
					o.Log.Error("encoding response", "method", m.Method, "err", err)
					return
				}
				if err := sess.write(b); err != nil {
					o.Log.Debug("response not sent", "method", m.Method, "err", err)
				}
			}(msg)
		case msg.IsResponse():
			sess.deliver(msg)
		case msg.IsNotification():
			if o.OnNotification != nil {
				o.OnNotification(readCtx, sess, msg)
			} else {
				o.Log.Debug("notification from the controller", "method", msg.Method)
			}
		}
	}
}

func pinger(ctx context.Context, o Options, conn *websocket.Conn, kill context.CancelFunc) {
	t := time.NewTicker(o.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, o.PingTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				o.Log.Warn("controller did not answer a ping; reconnecting", "err", err)
				kill()
				_ = conn.CloseNow()
				return
			}
		}
	}
}
