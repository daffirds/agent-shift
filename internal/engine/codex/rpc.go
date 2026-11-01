package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/daffirds/agent-shift/internal/engine"
)

// rpc is a minimal JSON-RPC 2.0 peer over newline-delimited stdio (codex app-server).
type rpc struct {
	w          io.Writer
	transcript *engine.TranscriptLog

	writeMu sync.Mutex
	seq     atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan rpcResult
	closed  bool

	onNotify  func(method string, params json.RawMessage)
	onRequest func(method string, params json.RawMessage) (any, error)
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error returned by codex.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("codex rpc error %d: %s", e.Code, e.Message) }

func (r *rpc) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.transcript.Write("out", b)
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_, err = r.w.Write(append(b, '\n'))
	return err
}

// call sends a request and waits for its response.
func (r *rpc) call(ctx context.Context, method string, params any, out any) error {
	id := r.seq.Add(1)
	ch := make(chan rpcResult, 1)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return engine.ErrClosed
	}
	r.pending[id] = ch
	r.mu.Unlock()
	if err := r.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		r.drop(id)
		return err
	}
	select {
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		if out != nil && len(res.result) > 0 {
			return json.Unmarshal(res.result, out)
		}
		return nil
	case <-ctx.Done():
		r.drop(id)
		return ctx.Err()
	}
}

func (r *rpc) drop(id int64) {
	r.mu.Lock()
	delete(r.pending, id)
	r.mu.Unlock()
}

func (r *rpc) notify(method string, params any) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// readLoop dispatches responses, notifications and server→client requests until EOF.
func (r *rpc) readLoop(rd io.Reader) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		r.transcript.Write("in", line)
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		switch {
		case m.Method != "" && m.ID != nil:
			go r.serve(*m.ID, m.Method, m.Params)
		case m.Method != "":
			if r.onNotify != nil {
				r.onNotify(m.Method, m.Params)
			}
		case m.ID != nil:
			var id int64
			if json.Unmarshal(*m.ID, &id) != nil {
				continue
			}
			r.mu.Lock()
			ch := r.pending[id]
			delete(r.pending, id)
			r.mu.Unlock()
			if ch != nil {
				if m.Error != nil {
					ch <- rpcResult{err: m.Error}
				} else {
					ch <- rpcResult{result: m.Result}
				}
			}
		}
	}
	r.mu.Lock()
	r.closed = true
	for id, ch := range r.pending {
		ch <- rpcResult{err: engine.ErrClosed}
		delete(r.pending, id)
	}
	r.mu.Unlock()
}

func (r *rpc) serve(id json.RawMessage, method string, params json.RawMessage) {
	var (
		result any
		err    error = errors.New("method not supported")
	)
	if r.onRequest != nil {
		result, err = r.onRequest(method, params)
	}
	if err != nil {
		_ = r.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": err.Error()}})
		return
	}
	_ = r.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
