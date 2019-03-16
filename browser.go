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
	cmd  *cdproto.Message
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
	buf, err := msg.MarshalJSON()
	if err != nil {
		return err
	}
	return b.conn.Write(buf)
}

func (b *Browser) Execute(ctx context.Context, methodType string, params json.Marshaler, res json.Unmarshaler) error {
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
		cmd: &cdproto.Message{
			ID:     id,
			Method: cdproto.MethodType(methodType),
			Params: paramsMsg,
		},
		resp: ch,
	}
	errch := make(chan error, 1)
	go func() {
		defer close(errch)

		select {
		case msg := <-ch:
			switch {
			case msg == nil:
				errch <- ErrChannelClosed

			case msg.Error != nil:
				errch <- msg.Error

			case res != nil:
				errch <- json.Unmarshal(msg.Result, res)
			}

		case <-ctx.Done():
			errch <- ctx.Err()
		}
	}()

	return <-errch
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
				msg, err := b.read()
				if err != nil {
					return
				}

				switch {
				case msg.Method != "":
					fmt.Printf("method: %+v\n", msg)

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
			resp <- res
			close(resp)
			delete(respByID, res.ID)

		case q := <-b.cmdQueue:
			respByID[q.cmd.ID] = q.resp

			buf, err := json.Marshal(q.cmd)
			if err != nil {
				b.errf("%s", err)
				break
			}
			if err := b.conn.Write(buf); err != nil {
				b.errf("%s", err)
				break
			}

		case <-ctx.Done():
			return
		}
	}
}

func (b *Browser) read() (*cdproto.Message, error) {
	buf, err := b.conn.Read()
	if err != nil {
		return nil, err
	}
	msg := new(cdproto.Message)
	if err := json.Unmarshal(buf, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// sendToTarget writes the supplied message to the target.
func (b *Browser) sendToTarget(targetID string, method cdproto.MethodType, params easyjson.RawMessage) error {
	return nil
}

// CreateContext creates a new browser context.
func (b *Browser) CreateContext() (context.Context, error) {
	return nil, nil
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
