package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func roundTrip(t *testing.T, d *Dispatcher, frame string) *Message {
	t.Helper()
	m, errResp := Decode([]byte(frame))
	if errResp != nil {
		return errResp
	}
	return d.Serve(context.Background(), m)
}

func TestDispatch(t *testing.T) {
	d := NewDispatcher()
	d.Register("echo", func(_ context.Context, p json.RawMessage) (any, error) {
		var in struct{ X int }
		if err := Params(p, &in); err != nil {
			return nil, err
		}
		return map[string]int{"x": in.X}, nil
	})
	d.Register("fail", func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("boom") })
	d.Register("typed", func(context.Context, json.RawMessage) (any, error) { return nil, Errorf(CodeNotFound, "nope") })
	d.Register("nil", func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	d.Register("panic", func(context.Context, json.RawMessage) (any, error) { panic("oops") })

	cases := []struct{ in, want string }{
		{`{"jsonrpc":"2.0","id":1,"method":"echo","params":{"x":5}}`, `{"jsonrpc":"2.0","id":1,"result":{"x":5}}`},
		{`{"jsonrpc":"2.0","id":"a","method":"echo"}`, `{"jsonrpc":"2.0","id":"a","result":{"x":0}}`},
		{`{"jsonrpc":"2.0","id":2,"method":"fail"}`, `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"boom"}}`},
		{`{"jsonrpc":"2.0","id":3,"method":"typed"}`, `{"jsonrpc":"2.0","id":3,"error":{"code":-32002,"message":"nope"}}`},
		{`{"jsonrpc":"2.0","id":4,"method":"nil"}`, `{"jsonrpc":"2.0","id":4,"result":null}`},
		{`{"jsonrpc":"2.0","id":5,"method":"missing"}`, `{"jsonrpc":"2.0","id":5,"error":{"code":-32601,"message":"method \"missing\" not found"}}`},
		{`{"jsonrpc":"2.0","id":6,"method":"echo","params":"bad"}`, `{"jsonrpc":"2.0","id":6,"error":{"code":-32602,"message":"invalid params: json: cannot unmarshal string into Go value of type struct { X int }"}}`},
		{`{"jsonrpc":"2.0","id":7,"method":"panic"}`, `{"jsonrpc":"2.0","id":7,"error":{"code":-32603,"message":"internal error: oops"}}`},
		{`not json`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error: invalid character 'o' in literal null (expecting 'u')"}}`},
		{`{"id":8,"method":"echo"}`, `{"jsonrpc":"2.0","id":8,"error":{"code":-32600,"message":"not a JSON-RPC 2.0 message"}}`},
	}
	for _, c := range cases {
		resp := roundTrip(t, d, c.in)
		got, _ := json.Marshal(resp)
		if string(got) != c.want {
			t.Errorf("%s\n got %s\nwant %s", c.in, got, c.want)
		}
	}
	// Notifications never get a reply, known or not.
	if r := roundTrip(t, d, `{"jsonrpc":"2.0","method":"echo"}`); r != nil {
		t.Fatalf("reply to notification: %+v", r)
	}
	if r := roundTrip(t, d, `{"jsonrpc":"2.0","method":"missing"}`); r != nil {
		t.Fatalf("reply to unknown notification: %+v", r)
	}
}

func TestMessageKinds(t *testing.T) {
	m, _ := Decode([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	if !m.IsResponse() || m.IsRequest() || m.IsNotification() {
		t.Fatal("response misclassified")
	}
	b, _ := Notification("locate.ended", map[string]string{"reason": "timeout"})
	if string(b) != `{"jsonrpc":"2.0","method":"locate.ended","params":{"reason":"timeout"}}` {
		t.Fatal(string(b))
	}
}

func TestRequest(t *testing.T) {
	b, err := Request(7, "collector.hello", map[string]string{"instanceId": "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"jsonrpc":"2.0","id":7,"method":"collector.hello","params":{"instanceId":"abc"}}` {
		t.Fatal(string(b))
	}
	m, errResp := Decode(b)
	if errResp != nil || !m.IsRequest() {
		t.Fatalf("not a request: %+v %+v", m, errResp)
	}
}
