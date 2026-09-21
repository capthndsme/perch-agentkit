// Package rpc is JSON-RPC 2.0 over whole WebSocket messages: one object per
// frame, no batches. Both Perch daemons (perch-apd, perch-collector) and the
// controller speak it the same way.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeCommandFailed  = -32000
	CodeUnsupported    = -32001
	CodeNotFound       = -32002
)

// Message is any JSON-RPC 2.0 object: request, notification or response.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// IsRequest: has a method and an id.
func (m *Message) IsRequest() bool { return m.Method != "" && m.ID != nil }

// IsNotification: has a method, no id.
func (m *Message) IsNotification() bool { return m.Method != "" && m.ID == nil }

// IsResponse: no method, an id.
func (m *Message) IsResponse() bool { return m.Method == "" && m.ID != nil }

// Error is a JSON-RPC error object; it is also a Go error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Errorf builds an *Error.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Handler serves one method. Returning an *Error sends it as is; any other
// error becomes -32000 "command failed".
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Dispatcher routes requests to handlers.
type Dispatcher struct {
	handlers map[string]Handler
}

// NewDispatcher returns an empty dispatcher.
func NewDispatcher() *Dispatcher { return &Dispatcher{handlers: map[string]Handler{}} }

// Register adds a method.
func (d *Dispatcher) Register(method string, h Handler) { d.handlers[method] = h }

// Methods lists registered method names.
func (d *Dispatcher) Methods() []string {
	out := make([]string, 0, len(d.handlers))
	for m := range d.handlers {
		out = append(out, m)
	}
	return out
}

// Decode parses one frame. A parse failure returns a ready-to-send error
// response (id null).
func Decode(frame []byte) (*Message, *Message) {
	var m Message
	dec := json.NewDecoder(bytes.NewReader(frame))
	if err := dec.Decode(&m); err != nil {
		return nil, errorResponse(json.RawMessage("null"), Errorf(CodeParseError, "parse error: %v", err))
	}
	if m.JSONRPC != "2.0" || (m.Method == "" && m.ID == nil) {
		id := m.ID
		if id == nil {
			id = json.RawMessage("null")
		}
		return nil, errorResponse(id, Errorf(CodeInvalidRequest, "not a JSON-RPC 2.0 message"))
	}
	return &m, nil
}

// Serve runs a request and returns its response (nil for notifications).
func (d *Dispatcher) Serve(ctx context.Context, m *Message) *Message {
	h, ok := d.handlers[m.Method]
	if !ok {
		if m.IsNotification() {
			return nil
		}
		return errorResponse(m.ID, Errorf(CodeMethodNotFound, "method %q not found", m.Method))
	}
	result, err := safeCall(ctx, h, m.Params)
	if m.IsNotification() {
		return nil
	}
	if err != nil {
		var rpcErr *Error
		if !errors.As(err, &rpcErr) {
			rpcErr = &Error{Code: CodeCommandFailed, Message: err.Error()}
		}
		return errorResponse(m.ID, rpcErr)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return errorResponse(m.ID, Errorf(CodeInternal, "encoding result: %v", err))
	}
	return &Message{JSONRPC: "2.0", ID: m.ID, Result: raw}
}

func safeCall(ctx context.Context, h Handler, params json.RawMessage) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = Errorf(CodeInternal, "internal error: %v", r)
		}
	}()
	return h(ctx, params)
}

func errorResponse(id json.RawMessage, e *Error) *Message {
	return &Message{JSONRPC: "2.0", ID: id, Error: e}
}

// Notification builds a notification message.
func Notification(method string, params any) ([]byte, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Message{JSONRPC: "2.0", Method: method, Params: raw})
}

// Params decodes params into v; empty or null params leave v unchanged.
func Params(raw json.RawMessage, v any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(trimmed, v); err != nil {
		return Errorf(CodeInvalidParams, "invalid params: %v", err)
	}
	return nil
}

// Request builds a request message with a numeric id.
func Request(id int64, method string, params any) ([]byte, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	idRaw, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Message{JSONRPC: "2.0", ID: idRaw, Method: method, Params: raw})
}
