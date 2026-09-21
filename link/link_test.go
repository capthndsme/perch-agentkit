package link

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/capthndsme/perch-agentkit/rpc"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeController accepts one endpoint and hands each session to onSession.
type fakeController struct {
	t         *testing.T
	status    int    // non-zero: refuse the upgrade with this status
	body      string // with status
	onSession func(ctx context.Context, c *websocket.Conn)

	mu      sync.Mutex
	headers []http.Header
}

func (f *fakeController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	if f.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		if f.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "7")
		}
		w.WriteHeader(f.status)
		io.WriteString(w, f.body)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{"perch-test.v1"},
		CompressionMode: websocket.CompressionNoContextTakeover,
	})
	if err != nil {
		f.t.Errorf("accept: %v", err)
		return
	}
	defer c.CloseNow()
	if f.onSession != nil {
		f.onSession(r.Context(), c)
	}
}

func wsURL(t *testing.T, srv *httptest.Server) string {
	u, err := WebSocketURL(srv.URL, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func readMsg(ctx context.Context, t *testing.T, c *websocket.Conn) rpc.Message {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m rpc.Message
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("frame %s: %v", data, err)
	}
	return m
}

func TestSessionRequestsNotificationsAndCalls(t *testing.T) {
	notes := make(chan string, 4)
	serverDone := make(chan struct{})
	fc := &fakeController{t: t}
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		defer close(serverDone)
		if c.Subprotocol() != "perch-test.v1" {
			t.Errorf("subprotocol %q", c.Subprotocol())
		}
		// The agent's OnOpen calls hello first.
		hello := readMsg(ctx, t, c)
		if hello.Method != "agent.hello" || !hello.IsRequest() || string(hello.Params) != `{"name":"x"}` {
			t.Errorf("hello %+v", hello)
		}
		c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":`+string(hello.ID)+`,"result":{"welcome":true}}`))
		// A notification, then a request of ours.
		c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","method":"agent.configure","params":{"metricsIntervalSeconds":5}}`))
		c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":"r1","method":"ping"}`))
		resp := readMsg(ctx, t, c)
		if string(resp.ID) != `"r1"` || string(resp.Result) != `{"pong":true}` {
			t.Errorf("ping response %+v", resp)
		}
		// NotifyRaw from OnOpen after the welcome.
		raw := readMsg(ctx, t, c)
		if raw.Method != "agent.push" || string(raw.Params) != `{"seq":1}` {
			t.Errorf("raw notification %+v", raw)
		}
		c.Close(CloseReplaced, "replaced by a newer session")
	}
	srv := httptest.NewServer(fc)
	defer srv.Close()

	disp := rpc.NewDispatcher()
	disp.Register("ping", func(context.Context, json.RawMessage) (any, error) {
		return map[string]bool{"pong": true}, nil
	})
	err := Run(context.Background(), Options{
		URL:         wsURL(t, srv),
		Subprotocol: "perch-test.v1",
		Header:      http.Header{"Authorization": {"Bearer k"}},
		Compression: true,
		Log:         quiet,
		Dispatcher:  disp,
		OnNotification: func(_ context.Context, _ *Session, m *rpc.Message) {
			notes <- m.Method + " " + string(m.Params)
		},
		OnOpen: func(ctx context.Context, s *Session) {
			var res struct{ Welcome bool }
			if err := s.Call(ctx, "agent.hello", map[string]string{"name": "x"}, &res); err != nil || !res.Welcome {
				t.Errorf("call: %v %+v", err, res)
				return
			}
			// Wait until the ping round trip is done, then push raw params.
			time.Sleep(50 * time.Millisecond)
			if err := s.NotifyRaw("agent.push", json.RawMessage(`{"seq":1}`)); err != nil {
				t.Errorf("notify raw: %v", err)
			}
		},
	})
	<-serverDone
	if websocket.CloseStatus(err) != CloseReplaced {
		t.Fatalf("run ended with %v", err)
	}
	if got := <-notes; got != `agent.configure {"metricsIntervalSeconds":5}` {
		t.Fatalf("notification %q", got)
	}
	h := fc.headers[0]
	if h.Get("Authorization") != "Bearer k" || !strings.Contains(h.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
		t.Fatalf("upgrade headers %v", h)
	}
}

func TestCallErrorsAndClosedSession(t *testing.T) {
	fc := &fakeController{t: t}
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		m := readMsg(ctx, t, c)
		c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":`+string(m.ID)+`,"error":{"code":-32000,"message":"no","data":{"error":"announce_key_mismatch"}}}`))
		c.Close(CloseRevoked, "revoked")
	}
	srv := httptest.NewServer(fc)
	defer srv.Close()
	var callErr, lateErr error
	var s *Session
	done := make(chan struct{})
	err := Run(context.Background(), Options{
		URL: wsURL(t, srv), Log: quiet,
		OnOpen: func(ctx context.Context, sess *Session) {
			defer close(done)
			s = sess
			callErr = sess.Call(ctx, "collector.hello", nil, nil)
		},
	})
	<-done
	var rerr *rpc.Error
	if !errors.As(callErr, &rerr) || rerr.Code != rpc.CodeCommandFailed || rerr.Message != "no" {
		t.Fatalf("call error %v", callErr)
	}
	if websocket.CloseStatus(err) != CloseRevoked {
		t.Fatalf("run %v", err)
	}
	lateErr = s.Call(context.Background(), "x", nil, nil)
	if !errors.Is(lateErr, ErrClosed) {
		t.Fatalf("call after the session ended: %v", lateErr)
	}
}

func TestRefusedUpgradeIsAStatusError(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		code   string
		retry  time.Duration
	}{
		{401, `{"error":"invalid_collector_key","message":"Unknown key."}`, "invalid_collector_key", 0},
		{429, `{"error":"rate_limited","retryAfterSeconds":30}`, "rate_limited", 7 * time.Second},
		{404, `not found`, "", 0},
	} {
		srv := httptest.NewServer(&fakeController{t: t, status: c.status, body: c.body})
		err := Run(context.Background(), Options{URL: wsURL(t, srv), Log: quiet})
		srv.Close()
		var se *StatusError
		if !errors.As(err, &se) || se.Status != c.status || se.Code != c.code || se.RetryAfter != c.retry {
			t.Errorf("%d: %#v", c.status, err)
		}
	}
}

func TestCancelSaysGoingAway(t *testing.T) {
	closed := make(chan websocket.StatusCode, 1)
	fc := &fakeController{t: t}
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		_, _, err := c.Read(ctx)
		closed <- websocket.CloseStatus(err)
	}
	srv := httptest.NewServer(fc)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	opened := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- Run(ctx, Options{URL: wsURL(t, srv), Log: quiet, OnOpen: func(context.Context, *Session) { close(opened) }})
	}()
	<-opened
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("run after cancel: %v", err)
	}
	select {
	case code := <-closed:
		if code != websocket.StatusGoingAway {
			t.Fatalf("close code %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("controller never saw the close")
	}
}

func TestMissedPongEndsTheSession(t *testing.T) {
	fc := &fakeController{t: t}
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		<-ctx.Done() // never reads, so pings go unanswered
	}
	srv := httptest.NewServer(fc)
	defer srv.Close()
	start := time.Now()
	err := Run(context.Background(), Options{URL: wsURL(t, srv), Log: quiet, PingInterval: 50 * time.Millisecond, PingTimeout: 50 * time.Millisecond})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("run %v after %v", err, time.Since(start))
	}
}

func TestStatusErrorParsing(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.WriteHeader(422)
	rec.WriteString(`{"errors":[{"field":"hostname","message":"is required"}]}`)
	se := ReadStatusError(rec.Result())
	if se.Status != 422 || se.Message != "hostname is required" {
		t.Fatalf("%+v", se)
	}
	rec = httptest.NewRecorder()
	rec.Header().Set("Retry-After", "30")
	rec.WriteHeader(429)
	se = ReadStatusError(rec.Result())
	if se.RetryAfter != 30*time.Second || se.Error() != "HTTP 429: Too Many Requests" {
		t.Fatalf("%+v %s", se, se.Error())
	}
}

func TestBackoff(t *testing.T) {
	b := Backoff{Min: time.Second, Max: time.Minute}
	prev := time.Duration(0)
	for i := 0; i < 10; i++ {
		d := b.Next()
		if d < prev/2 || d > 72*time.Second {
			t.Fatalf("step %d: %v", i, d)
		}
		prev = d
	}
	if b.cur != time.Minute {
		t.Fatalf("cap %v", b.cur)
	}
	b.Reset()
	if d := b.Next(); d > 1200*time.Millisecond {
		t.Fatalf("after reset %v", d)
	}
}

func TestURLs(t *testing.T) {
	for in, want := range map[string]string{
		"https://perch.example.com/":  "wss://perch.example.com/api/v1/collector-agent/ws",
		"http://192.168.1.10:8080":    "ws://192.168.1.10:8080/api/v1/collector-agent/ws",
		"https://example.com/prefix/": "wss://example.com/prefix/api/v1/collector-agent/ws",
	} {
		got, err := WebSocketURL(in, "/api/v1/collector-agent/ws")
		if err != nil || got != want {
			t.Errorf("%s -> %s %v", in, got, err)
		}
	}
	if _, err := WebSocketURL("ftp://x", "/ws"); err == nil {
		t.Error("ftp accepted")
	}
}

func TestRunWaitsForOnOpen(t *testing.T) {
	fc := &fakeController{t: t}
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		time.Sleep(20 * time.Millisecond)
		c.Close(CloseReplaced, "replaced")
	}
	srv := httptest.NewServer(fc)
	defer srv.Close()
	var mu sync.Mutex
	finished := false
	Run(context.Background(), Options{URL: wsURL(t, srv), Log: quiet, OnOpen: func(ctx context.Context, _ *Session) {
		<-ctx.Done()
		time.Sleep(30 * time.Millisecond) // cleanup after the session ended
		mu.Lock()
		finished = true
		mu.Unlock()
	}})
	mu.Lock()
	defer mu.Unlock()
	if !finished {
		t.Fatal("Run returned before OnOpen did")
	}
}

// An answer that arrives together with the close is still the answer.
func TestCallAnswerBeatsClose(t *testing.T) {
	for i := 0; i < 30; i++ {
		fc := &fakeController{t: t}
		fc.onSession = func(ctx context.Context, c *websocket.Conn) {
			m := readMsg(ctx, t, c)
			c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"lifecycle":"dismissed"}}`))
			c.Close(CloseDismissed, "dismissed")
		}
		srv := httptest.NewServer(fc)
		var got struct{ Lifecycle string }
		var callErr error
		Run(context.Background(), Options{URL: wsURL(t, srv), Log: quiet, OnOpen: func(ctx context.Context, s *Session) {
			callErr = s.Call(ctx, "collector.hello", nil, &got)
		}})
		srv.Close()
		if callErr != nil || got.Lifecycle != "dismissed" {
			t.Fatalf("run %d: %v %+v", i, callErr, got)
		}
	}
}
