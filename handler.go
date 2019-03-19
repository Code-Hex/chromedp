package chromedp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mailru/easyjson"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/inspector"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
)

// Target manages a Chrome DevTools Protocol target.
type Target struct {
	browser   *Browser
	sessionID target.SessionID

	// below are the old TargetHandler fields.

	// frames is the set of encountered frames.
	frames map[cdp.FrameID]*cdp.Frame

	// cur is the current top level frame.
	cur *cdp.Frame

	// qcmd is the outgoing message queue.
	qcmd chan *cdproto.Message

	// detached is closed when the detached event is received.
	detached chan *inspector.EventDetached

	// logging funcs
	logf, debugf, errf func(string, ...interface{})

	sync.RWMutex
}

func (t *Target) Execute(ctx context.Context, method string, params json.Marshaler, res json.Unmarshaler) error {
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

// below are the old TargetHandler methods.

// processEvent processes an incoming event.
func (t *Target) processEvent(ctxt context.Context, msg *cdproto.Message) error {
	if msg == nil {
		return ErrChannelClosed
	}

	// unmarshal
	ev, err := cdproto.UnmarshalMessage(msg)
	if err != nil {
		return err
	}

	switch e := ev.(type) {
	case *inspector.EventDetached:
		t.Lock()
		defer t.Unlock()
		t.detached <- e
		return nil

	case *dom.EventDocumentUpdated:
		go t.documentUpdated(ctxt)
		return nil
	}

	d := msg.Method.Domain()
	if d != "Page" && d != "DOM" {
		return nil
	}

	switch d {
	case "Page":
		t.pageEvent(ctxt, ev)

	case "DOM":
		t.domEvent(ctxt, ev)
	}

	return nil
}

// documentUpdated handles the document updated event, retrieving the document
// root for the root frame.
func (t *Target) documentUpdated(ctxt context.Context) {
	f, err := t.WaitFrame(ctxt, cdp.EmptyFrameID)
	if err != nil {
		t.errf("could not get current frame: %v", err)
		return
	}

	f.Lock()
	defer f.Unlock()

	// invalidate nodes
	if f.Root != nil {
		close(f.Root.Invalidated)
	}

	f.Nodes = make(map[cdp.NodeID]*cdp.Node)
	f.Root, err = dom.GetDocument().WithPierce(true).Do(ctxt, t)
	if err != nil {
		t.errf("could not retrieve document root for %s: %v", f.ID, err)
		return
	}
	f.Root.Invalidated = make(chan struct{})
	walk(f.Nodes, f.Root)
}

// emptyObj is an empty JSON object message.
var emptyObj = easyjson.RawMessage([]byte(`{}`))

// GetRoot returns the current top level frame's root document node.
func (t *Target) GetRoot(ctxt context.Context) (*cdp.Node, error) {
	var root *cdp.Node

	for {
		var cur *cdp.Frame
		select {
		default:
			t.RLock()
			cur = t.cur
			if cur != nil {
				cur.RLock()
				root = cur.Root
				cur.RUnlock()
			}
			t.RUnlock()

			if cur != nil && root != nil {
				return root, nil
			}

			time.Sleep(DefaultCheckDuration)

		case <-ctxt.Done():
			return nil, ctxt.Err()
		}
	}
}

// SetActive sets the currently active frame after a successful navigation.
func (t *Target) SetActive(ctxt context.Context, id cdp.FrameID) error {
	// get frame
	f, err := t.WaitFrame(ctxt, id)
	if err != nil {
		return err
	}

	t.Lock()
	defer t.Unlock()

	t.cur = f

	return nil
}

// WaitFrame waits for a frame to be loaded using the provided context.
func (t *Target) WaitFrame(ctxt context.Context, id cdp.FrameID) (*cdp.Frame, error) {
	// TODO: fix this
	timeout := time.After(time.Second)

	for {
		select {
		default:
			var f *cdp.Frame
			var ok bool

			t.RLock()
			if id == cdp.EmptyFrameID {
				f, ok = t.cur, t.cur != nil
			} else {
				f, ok = t.frames[id]
			}
			t.RUnlock()

			if ok {
				return f, nil
			}

			time.Sleep(DefaultCheckDuration)

		case <-ctxt.Done():
			return nil, ctxt.Err()

		case <-timeout:
			return nil, fmt.Errorf("timeout waiting for frame `%s`", id)
		}
	}
}

// WaitNode waits for a node to be loaded using the provided context.
func (t *Target) WaitNode(ctxt context.Context, f *cdp.Frame, id cdp.NodeID) (*cdp.Node, error) {
	// TODO: fix this
	timeout := time.After(time.Second)

	for {
		select {
		default:
			var n *cdp.Node
			var ok bool

			f.RLock()
			n, ok = f.Nodes[id]
			f.RUnlock()

			if n != nil && ok {
				return n, nil
			}

			time.Sleep(DefaultCheckDuration)

		case <-ctxt.Done():
			return nil, ctxt.Err()

		case <-timeout:
			return nil, fmt.Errorf("timeout waiting for node `%d`", id)
		}
	}
}

// pageEvent handles incoming page events.
func (t *Target) pageEvent(ctxt context.Context, ev interface{}) {
	var id cdp.FrameID
	var op frameOp

	switch e := ev.(type) {
	case *page.EventFrameNavigated:
		t.Lock()
		t.frames[e.Frame.ID] = e.Frame
		if t.cur != nil && t.cur.ID == e.Frame.ID {
			t.cur = e.Frame
		}
		t.Unlock()
		return

	case *page.EventFrameAttached:
		id, op = e.FrameID, frameAttached(e.ParentFrameID)

	case *page.EventFrameDetached:
		id, op = e.FrameID, frameDetached

	case *page.EventFrameStartedLoading:
		id, op = e.FrameID, frameStartedLoading

	case *page.EventFrameStoppedLoading:
		id, op = e.FrameID, frameStoppedLoading

	case *page.EventFrameScheduledNavigation:
		id, op = e.FrameID, frameScheduledNavigation

	case *page.EventFrameClearedScheduledNavigation:
		id, op = e.FrameID, frameClearedScheduledNavigation

		// ignored events
	case *page.EventDomContentEventFired:
		return
	case *page.EventLoadEventFired:
		return
	case *page.EventFrameResized:
		return
	case *page.EventLifecycleEvent:
		return

	default:
		t.errf("unhandled page event %s", reflect.TypeOf(ev))
		return
	}

	f, err := t.WaitFrame(ctxt, id)
	if err != nil {
		t.errf("could not get frame %s: %v", id, err)
		return
	}

	t.Lock()
	defer t.Unlock()

	f.Lock()
	defer f.Unlock()

	op(f)
}

// domEvent handles incoming DOM events.
func (t *Target) domEvent(ctxt context.Context, ev interface{}) {
	// wait current frame
	f, err := t.WaitFrame(ctxt, cdp.EmptyFrameID)
	if err != nil {
		t.errf("could not process DOM event %s: %v", reflect.TypeOf(ev), err)
		return
	}

	var id cdp.NodeID
	var op nodeOp

	switch e := ev.(type) {
	case *dom.EventSetChildNodes:
		id, op = e.ParentID, setChildNodes(f.Nodes, e.Nodes)

	case *dom.EventAttributeModified:
		id, op = e.NodeID, attributeModified(e.Name, e.Value)

	case *dom.EventAttributeRemoved:
		id, op = e.NodeID, attributeRemoved(e.Name)

	case *dom.EventInlineStyleInvalidated:
		if len(e.NodeIds) == 0 {
			return
		}

		id, op = e.NodeIds[0], inlineStyleInvalidated(e.NodeIds[1:])

	case *dom.EventCharacterDataModified:
		id, op = e.NodeID, characterDataModified(e.CharacterData)

	case *dom.EventChildNodeCountUpdated:
		id, op = e.NodeID, childNodeCountUpdated(e.ChildNodeCount)

	case *dom.EventChildNodeInserted:
		if e.PreviousNodeID != cdp.EmptyNodeID {
			_, err = t.WaitNode(ctxt, f, e.PreviousNodeID)
			if err != nil {
				return
			}
		}
		id, op = e.ParentNodeID, childNodeInserted(f.Nodes, e.PreviousNodeID, e.Node)

	case *dom.EventChildNodeRemoved:
		id, op = e.ParentNodeID, childNodeRemoved(f.Nodes, e.NodeID)

	case *dom.EventShadowRootPushed:
		id, op = e.HostID, shadowRootPushed(f.Nodes, e.Root)

	case *dom.EventShadowRootPopped:
		id, op = e.HostID, shadowRootPopped(f.Nodes, e.RootID)

	case *dom.EventPseudoElementAdded:
		id, op = e.ParentID, pseudoElementAdded(f.Nodes, e.PseudoElement)

	case *dom.EventPseudoElementRemoved:
		id, op = e.ParentID, pseudoElementRemoved(f.Nodes, e.PseudoElementID)

	case *dom.EventDistributedNodesUpdated:
		id, op = e.InsertionPointID, distributedNodesUpdated(e.DistributedNodes)

	default:
		t.errf("unhandled node event %s", reflect.TypeOf(ev))
		return
	}

	// retrieve node
	n, err := t.WaitNode(ctxt, f, id)
	if err != nil {
		s := strings.TrimSuffix(goruntime.FuncForPC(reflect.ValueOf(op).Pointer()).Name(), ".func1")
		i := strings.LastIndex(s, ".")
		if i != -1 {
			s = s[i+1:]
		}
		t.errf("could not perform (%s) operation on node %d (wait node): %v", s, id, err)
		return
	}

	t.Lock()
	defer t.Unlock()

	f.Lock()
	defer f.Unlock()

	op(n)
}

type TargetOption func(*Target)
