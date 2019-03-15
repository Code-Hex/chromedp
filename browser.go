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
	"sync"
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

	// TODO: rethink this without a lock
	respByIDMu sync.RWMutex
	respByID   map[int64]chan *cdproto.Message

	// qcmd is the outgoing message queue.
	qcmd chan *cdproto.Message

	// qres is the incoming command result queue.
	qres chan *cdproto.Message

	// logging funcs
	logf func(string, ...interface{})
	errf func(string, ...interface{})
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
	b.respByIDMu.Lock()
	b.respByID[id] = ch
	b.respByIDMu.Unlock()

	b.qcmd <- &cdproto.Message{
		ID:     id,
		Method: cdproto.MethodType(methodType),
		Params: paramsMsg,
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

		b.respByIDMu.Lock()
		defer b.respByIDMu.Unlock()
		delete(b.respByID, id)
	}()

	return <-errch
}

func (b *Browser) Start(ctx context.Context) error {
	b.qcmd = make(chan *cdproto.Message)
	b.qres = make(chan *cdproto.Message)
	b.respByID = make(map[int64]chan *cdproto.Message)

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

	// process queues
	for {
		select {
		case res := <-b.qres:
			err := b.processResult(res)
			if err != nil {
				b.errf("could not process result for message %d: %v", res.ID, err)
			}

		case cmd := <-b.qcmd:
			err := b.processCommand(cmd)
			if err != nil {
				b.errf("could not process command message %d: %v", cmd.ID, err)
			}

		case <-ctx.Done():
			return
		}
	}
}

// processResult processes an incoming command result.
func (b *Browser) processResult(msg *cdproto.Message) error {
	b.respByIDMu.RLock()
	defer b.respByIDMu.RUnlock()

	ch, ok := b.respByID[msg.ID]
	if !ok {
		return fmt.Errorf("id %d not present in response map", msg.ID)
	}
	defer close(ch)

	ch <- msg
	return nil
}

// processCommand writes a command to the client connection.
func (b *Browser) processCommand(cmd *cdproto.Message) error {
	buf, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	return b.conn.Write(buf)
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
