// Package chromedp is a high level Chrome DevTools Protocol client that
// simplifies driving browsers for scraping, unit testing, or profiling web
// pages using the CDP.
//
// chromedp requires no third-party dependencies, implementing the async Chrome
// DevTools Protocol entirely in Go.
package chromedp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/target"
	"github.com/mailru/easyjson"
)

// Browser is the high-level Chrome DevTools Protocol browser manager, handling
// the browser process runner, WebSocket clients, associated targets, and
// network, page, and DOM events.
type Browser struct {
	UserDataDir string

	conn Transport

	// next is the next message id.
	next int64

	cmdQueue chan cmdJob

	// qres is the incoming command result queue.
	qres chan *cdproto.Message

	// logging funcs
	logf func(string, ...interface{})
	errf func(string, ...interface{})
}

type cmdJob struct {
	msg  *cdproto.Message
	resp chan *cdproto.Message
}

// NewBrowser creates a new browser.
func NewBrowser(ctx context.Context, urlstr string, opts ...BrowserOption) (*Browser, error) {
	conn, err := DialContext(ctx, ForceIP(urlstr))
	if err != nil {
		return nil, err
	}

	b := &Browser{
		conn: conn,
		logf: log.Printf,
	}

	// apply options
	for _, o := range opts {
		if err := o(b); err != nil {
			return nil, err
		}
	}

	// ensure errf is set
	if b.errf == nil {
		b.errf = func(s string, v ...interface{}) { b.logf("ERROR: "+s, v...) }
	}

	return b, nil
}

// Shutdown shuts down the browser.
func (b *Browser) Shutdown() error {
	if b.conn != nil {
		if err := b.send(cdproto.CommandBrowserClose, nil); err != nil {
			b.errf("could not close browser: %v", err)
		}
		return b.conn.Close()
	}
	return nil
}

// send writes the supplied message and params.
func (b *Browser) send(method cdproto.MethodType, params easyjson.RawMessage) error {
	msg := &cdproto.Message{
		ID:     atomic.AddInt64(&b.next, 1),
		Method: method,
		Params: params,
	}
	return b.conn.Write(msg)
}

type targetExecutor struct {
	browser   *Browser
	sessionID target.SessionID
}

func (t targetExecutor) Execute(ctx context.Context, method string, params json.Marshaler, res json.Unmarshaler) error {
	paramsMsg := emptyObj
	if params != nil {
		var err error
		if paramsMsg, err = json.Marshal(params); err != nil {
			return err
		}
	}
	innerID := atomic.AddInt64(&t.browser.next, 1)
	msg := &cdproto.Message{
		ID:     innerID,
		Method: cdproto.MethodType(method),
		Params: paramsMsg,
	}
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	sendParams := target.SendMessageToTarget(string(msgJSON)).
		WithSessionID(t.sessionID)
	sendParamsJSON, _ := json.Marshal(sendParams)

	// We want to grab the response from the inner message.
	ch := make(chan *cdproto.Message, 1)
	t.browser.cmdQueue <- cmdJob{
		msg:  &cdproto.Message{ID: innerID},
		resp: ch,
	}

	// The response from the outer message is uninteresting; pass a nil
	// resp channel.
	outerID := atomic.AddInt64(&t.browser.next, 1)
	t.browser.cmdQueue <- cmdJob{
		msg: &cdproto.Message{
			ID:     outerID,
			Method: target.CommandSendMessageToTarget,
			Params: sendParamsJSON,
		},
	}

	select {
	case msg := <-ch:
		switch {
		case msg == nil:
			return ErrChannelClosed
		case msg.Error != nil:
			return msg.Error
		case res != nil:
			return json.Unmarshal(msg.Result, res)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (b *Browser) executorForTarget(sessionID target.SessionID) targetExecutor {
	return targetExecutor{
		browser:   b,
		sessionID: sessionID,
	}
}

func (b *Browser) Execute(ctx context.Context, method string, params json.Marshaler, res json.Unmarshaler) error {
	paramsMsg := emptyObj
	if params != nil {
		var err error
		if paramsMsg, err = json.Marshal(params); err != nil {
			return err
		}
	}

	id := atomic.AddInt64(&b.next, 1)
	ch := make(chan *cdproto.Message, 1)
	b.cmdQueue <- cmdJob{
		msg: &cdproto.Message{
			ID:     id,
			Method: cdproto.MethodType(method),
			Params: paramsMsg,
		},
		resp: ch,
	}
	select {
	case msg := <-ch:
		switch {
		case msg == nil:
			return ErrChannelClosed
		case msg.Error != nil:
			return msg.Error
		case res != nil:
			return json.Unmarshal(msg.Result, res)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (b *Browser) Start(ctx context.Context) error {
	b.cmdQueue = make(chan cmdJob)
	b.qres = make(chan *cdproto.Message)

	go b.run(ctx)

	return nil
}

func (b *Browser) run(ctx context.Context) {
	defer b.conn.Close()

	// add cancel to context
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		defer cancel()

		for {
			select {
			default:
				msg, err := b.conn.Read()
				if err != nil {
					return
				}
				if msg.Method == cdproto.EventTargetReceivedMessageFromTarget {
					recv := new(target.EventReceivedMessageFromTarget)
					if err := json.Unmarshal(msg.Params, recv); err != nil {
						b.errf("%s", err)
						break
					}
					msg = new(cdproto.Message)
					if err := json.Unmarshal([]byte(recv.Message), msg); err != nil {
						b.errf("%s", err)
						break
					}
				}

				switch {
				case msg.Method != "":
					switch msg.Method {
					case cdproto.EventTargetAttachedToTarget:
					default:
						fmt.Printf("event: %+v\n", msg)
					}

				case msg.ID != 0:
					b.qres <- msg

				default:
					b.errf("ignoring malformed incoming message (missing id or method): %#v", msg)
				}

			case <-ctx.Done():
				return
			}
		}
	}()

	respByID := make(map[int64]chan *cdproto.Message)

	// process queues
	for {
		select {
		case res := <-b.qres:
			resp, ok := respByID[res.ID]
			if !ok {
				b.errf("id %d not present in response map", res.ID)
				break
			}
			if resp != nil {
				// resp could be nil, if we're not interested in
				// this response; for CommandSendMessageToTarget.
				resp <- res
				close(resp)
			}
			delete(respByID, res.ID)

		case q := <-b.cmdQueue:
			if _, ok := respByID[q.msg.ID]; ok {
				b.errf("id %d already present in response map", q.msg.ID)
				break
			}
			respByID[q.msg.ID] = q.resp

			if q.msg.Method == "" {
				// Only register the chananel in respByID;
				// useful for CommandSendMessageToTarget.
				break
			}
			if err := b.conn.Write(q.msg); err != nil {
				b.errf("%s", err)
				break
			}

		case <-ctx.Done():
			return
		}
	}
}

// BrowserOption is a browser option.
type BrowserOption func(*Browser) error

// WithLogf is a browser option to specify a func to receive general logging.
func WithLogf(f func(string, ...interface{})) BrowserOption {
	return func(b *Browser) error {
		b.logf = f
		return nil
	}
}

// WithErrorf is a browser option to specify a func to receive error logging.
func WithErrorf(f func(string, ...interface{})) BrowserOption {
	return func(b *Browser) error {
		b.errf = f
		return nil
	}
}

// WithConsolef is a browser option to specify a func to receive chrome log events.
//
// Note: NOT YET IMPLEMENTED.
func WithConsolef(f func(string, ...interface{})) BrowserOption {
	return func(b *Browser) error {
		return nil
	}
}
