package chromedp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
)

var (
	// DefaultUnmarshalOptions are default unmarshal options.
	DefaultUnmarshalOptions = jsonv2.JoinOptions(
		jsonv2.DefaultOptionsV2(),
		jsontext.AllowInvalidUTF8(true),
	)
	// DefaultMarshalOptions are default marshal options.
	DefaultMarshalOptions = jsonv2.JoinOptions(
		jsonv2.DefaultOptionsV2(),
		jsontext.AllowInvalidUTF8(true),
	)
)

// Browser is the high-level Chrome DevTools Protocol browser manager, handling
// the browser process runner, WebSocket clients, associated targets, and
// network, page, and DOM events.
type Browser struct {
	// next is the next message id.
	// NOTE: needs to be 64-bit aligned for 32-bit targets too, so be careful when moving this field.
	// This will be eventually done by the compiler once https://github.com/golang/go/issues/599 is fixed.
	next int64

	// LostConnection is closed when the websocket connection to Chrome is
	// dropped. This can be useful to make sure that Browser's context is
	// cancelled (and the handler stopped) once the connection has failed.
	LostConnection chan struct{}

	// closingGracefully is closed by Close before gracefully shutting down
	// the browser. This way, when the connection to the browser is lost and
	// LostConnection is closed, we will know not to immediately kill the
	// Chrome process. This is important to let the browser shut itself off,
	// saving its state to disk.
	closingGracefully chan struct{}

	dialTimeout time.Duration

	// pages keeps track of the attached targets, indexed by each's session
	// ID. The only reason this is a field is so that the tests can check the
	// map once a browser is closed.
	pages map[target.SessionID]*Target

	// autoAttachedSessions tracks sessions created by SetAutoAttach
	// (EventAttachedToTarget) that were NOT created via AttachToTarget.
	// These sessions are not in pages, so without this map their incoming
	// messages would be silently dropped and commands on them would fail.
	// Each entry owns the cancel func for the Target.run goroutine it
	// spawned; the detach handler invokes cancel so those goroutines exit
	// when Chrome reports the session is gone.
	// Keyed by SessionID; concurrent access is safe via sync.Map.
	autoAttachedSessions sync.Map // target.SessionID -> *autoAttachedEntry

	listenersMu sync.Mutex
	listeners   []cancelableListener

	conn Transport

	// newTabQueue is the queue used to create new target handlers, once a new
	// tab is created and attached to. The newly created Target is sent back
	// via newTabResult.
	newTabQueue chan *Target

	// cmdQueue is the outgoing command queue.
	cmdQueue chan *cdproto.Message

	// logging funcs
	logf func(string, ...any)
	errf func(string, ...any)
	dbgf func(string, ...any)

	// The optional fields below are helpful for some tests.

	// process can be initialized by the allocators which start a process
	// when allocating a browser.
	process *os.Process

	// userDataDir can be initialized by the allocators which set up user
	// data dirs directly.
	userDataDir string
}

// autoAttachedEntry is a value stored in Browser.autoAttachedSessions. It
// pairs the auto-attached Target with the cancel func for the goroutine
// running Target.run, so the detach handler can stop that goroutine.
type autoAttachedEntry struct {
	target *Target
	cancel context.CancelFunc
}

// NewBrowser creates a new browser. Typically, this function wouldn't be called
// directly, as the Allocator interface takes care of it.
func NewBrowser(ctx context.Context, urlstr string, opts ...BrowserOption) (*Browser, error) {
	b := &Browser{
		LostConnection:    make(chan struct{}),
		closingGracefully: make(chan struct{}),

		dialTimeout: 10 * time.Second,

		newTabQueue: make(chan *Target),

		// Fit some jobs without blocking, to reduce blocking in Execute.
		cmdQueue: make(chan *cdproto.Message, 32),

		logf: log.Printf,
	}
	// apply options
	for _, o := range opts {
		o(b)
	}
	// ensure errf is set
	if b.errf == nil {
		b.errf = func(s string, v ...any) { b.logf("ERROR: "+s, v...) }
	}

	dialCtx := ctx
	if b.dialTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, b.dialTimeout)
		defer cancel()
	}

	var err error
	b.conn, err = DialContext(dialCtx, urlstr, WithConnDebugf(b.dbgf))
	if err != nil {
		return nil, fmt.Errorf("could not dial %q: %w", urlstr, err)
	}

	go b.run(ctx)
	return b, nil
}

// Process returns the process object of the browser.
//
// It could be nil when the browser is allocated with RemoteAllocator.
// It could be useful for a monitoring system to collect process metrics of the browser process.
// (See [prometheus.NewProcessCollector] for an example).
//
// Example:
//
//	if process := chromedp.FromContext(ctx).Browser.Process(); process != nil {
//		fmt.Printf("Browser PID: %v", process.Pid)
//	}
//
// [prometheus.NewProcessCollector]: https://pkg.go.dev/github.com/prometheus/client_golang/prometheus#NewProcessCollector
func (b *Browser) Process() *os.Process {
	return b.process
}

func (b *Browser) newExecutorForTarget(ctx context.Context, targetID target.ID, sessionID target.SessionID) (*Target, error) {
	if targetID == "" {
		return nil, errors.New("empty target ID")
	}
	if sessionID == "" {
		return nil, errors.New("empty session ID")
	}
	t := &Target{
		browser:   b,
		TargetID:  targetID,
		SessionID: sessionID,

		messageQueue: make(chan *cdproto.Message, 1024),
		frames:       make(map[cdp.FrameID]*cdp.Frame),
		execContexts: make(map[cdp.FrameID]runtime.ExecutionContextID),
		cur:          cdp.FrameID(targetID),

		logf: b.logf,
		errf: b.errf,
	}

	// This send should be blocking, to ensure the tab is inserted into the
	// map before any more target events are routed.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case b.newTabQueue <- t:
	}
	return t, nil
}

// registerAutoAttachedSession creates a Target for a session that was
// auto-attached via SetAutoAttach (EventAttachedToTarget) rather than via an
// explicit AttachToTarget call. The Target is registered in autoAttachedSessions
// so that its incoming messages are routed and commands can be sent on it.
// ctx should be the context of the parent target so the session is cancelled
// when the parent is cancelled. Concurrent calls for the same sessionID are
// safe; only the first call takes effect.
func (b *Browser) registerAutoAttachedSession(ctx context.Context, sessionID target.SessionID, targetID target.ID) {
	t := &Target{
		browser:      b,
		TargetID:     targetID,
		SessionID:    sessionID,
		messageQueue: make(chan *cdproto.Message, 1024),
		frames:       make(map[cdp.FrameID]*cdp.Frame),
		execContexts: make(map[cdp.FrameID]runtime.ExecutionContextID),
		cur:          cdp.FrameID(targetID),
		logf:         b.logf,
		errf:         b.errf,
	}
	childCtx, cancel := context.WithCancel(ctx)
	entry := &autoAttachedEntry{target: t, cancel: cancel}
	if _, loaded := b.autoAttachedSessions.LoadOrStore(sessionID, entry); loaded {
		cancel() // discard the duplicate child ctx
		return
	}
	go t.run(childCtx)
}

// SessionExecutor returns a CDP executor for the given auto-attached session ID.
// This is the primary API for interacting with sessions created by
// Target.SetAutoAttach with waitForDebugger (e.g. to call
// Runtime.runIfWaitingForDebugger on a paused OOPIF).
//
// Returns (executor, true) if the session is known, or (nil, false) if not.
func (b *Browser) SessionExecutor(sessionID target.SessionID) (cdp.Executor, bool) {
	if val, ok := b.autoAttachedSessions.Load(sessionID); ok {
		return val.(*autoAttachedEntry).target, true
	}
	return nil, false
}

func (b *Browser) Execute(ctx context.Context, method string, params, res any) error {
	// Certain methods aren't available to the user directly.
	if method == browser.CommandClose {
		return fmt.Errorf("to close the browser gracefully, use chromedp.Cancel")
	}
	return b.execute(ctx, method, params, res)
}

func (b *Browser) execute(ctx context.Context, method string, params, res any) error {
	id := atomic.AddInt64(&b.next, 1)
	lctx, cancel := context.WithCancel(ctx)
	ch := make(chan *cdproto.Message, 1)
	fn := func(ev any) {
		if msg, ok := ev.(*cdproto.Message); ok && msg.ID == id {
			select {
			case <-ctx.Done():
			case ch <- msg:
			}
			cancel()
		}
	}
	b.listenersMu.Lock()
	b.listeners = append(b.listeners, cancelableListener{lctx, fn})
	b.listenersMu.Unlock()

	// send command
	var buf []byte
	if params != nil {
		var err error
		if buf, err = jsonv2.Marshal(params, DefaultMarshalOptions); err != nil {
			return err
		}
	}
	cmd := &cdproto.Message{
		ID:     id,
		Method: cdproto.MethodType(method),
		Params: buf,
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case b.cmdQueue <- cmd:
	}

	// wait for result
	select {
	case <-ctx.Done():
		return ctx.Err()
	case msg := <-ch:
		switch {
		case msg == nil:
			return ErrChannelClosed
		case msg.Error != nil:
			return msg.Error
		case res != nil:
			return jsonv2.Unmarshal(msg.Result, res, DefaultUnmarshalOptions)
		}
	}
	return nil
}

func (b *Browser) run(ctx context.Context) {
	defer b.conn.Close()

	// incomingQueue is the queue of incoming target events, to be routed by
	// their session ID.
	incomingQueue := make(chan *cdproto.Message, 1)

	delTabQueue := make(chan target.SessionID, 1)

	// This goroutine continuously reads events from the websocket
	// connection. The separate goroutine is needed since a websocket read
	// is blocking, so it cannot be used in a select statement.
	go func() {
		// Signal to run and exit the browser cleanup goroutine.
		defer close(b.LostConnection)

		for {
			msg := new(cdproto.Message)
			if err := b.conn.Read(ctx, msg); err != nil {
				var syntacticError *jsontext.SyntacticError
				if errors.As(err, &syntacticError) {
					b.errf("%s", err)
				}
				return
			}

			switch {
			case msg.SessionID != "" && (msg.Method != "" || msg.ID != 0):
				select {
				case <-ctx.Done():
					return
				case incomingQueue <- msg:
				}

			case msg.Method != "":
				ev, err := cdproto.UnmarshalMessage(msg, DefaultUnmarshalOptions)
				if err != nil {
					b.errf("%s", err)
					continue
				}

				// Register auto-attached sessions before notifying listeners so
				// that a listener can immediately send commands on the new session.
				if ev, ok := ev.(*target.EventAttachedToTarget); ok {
					b.registerAutoAttachedSession(ctx, ev.SessionID, ev.TargetInfo.TargetID)
				}

				b.listenersMu.Lock()
				b.listeners = runListeners(b.listeners, ev)
				b.listenersMu.Unlock()

				if ev, ok := ev.(*target.EventDetachedFromTarget); ok {
					delTabQueue <- ev.SessionID
				}

			case msg.ID != 0:
				b.listenersMu.Lock()
				b.listeners = runListeners(b.listeners, msg)
				b.listenersMu.Unlock()

			default:
				b.errf("ignoring malformed incoming message (missing id or method): %#v", msg)
			}
		}
	}()

	b.pages = make(map[target.SessionID]*Target, 32)
	for {
		select {
		case <-ctx.Done():
			// Safety net: cancel any auto-attached session goroutines still
			// alive. Detach events normally clean these up via delTabQueue,
			// but the WS may have dropped or Chrome may have crashed before
			// emitting them.
			b.autoAttachedSessions.Range(func(key, val any) bool {
				val.(*autoAttachedEntry).cancel()
				b.autoAttachedSessions.Delete(key)
				return true
			})
			return

		case msg := <-b.cmdQueue:
			if err := b.conn.Write(ctx, msg); err != nil {
				b.errf("%s", err)
				continue
			}

		case t := <-b.newTabQueue:
			if _, ok := b.pages[t.SessionID]; ok {
				b.errf("executor for %q already exists", t.SessionID)
			}
			b.pages[t.SessionID] = t

		case sessionID := <-delTabQueue:
			if _, ok := b.pages[sessionID]; ok {
				delete(b.pages, sessionID)
			} else if val, ok := b.autoAttachedSessions.LoadAndDelete(sessionID); ok {
				val.(*autoAttachedEntry).cancel()
			} else {
				b.errf("executor for %q doesn't exist", sessionID)
			}

		case m := <-incomingQueue:
			page, ok := b.pages[m.SessionID]
			if !ok {
				// Route to auto-attached session if registered; otherwise the
				// session was recently closed and the event can be dropped.
				if val, ok := b.autoAttachedSessions.Load(m.SessionID); ok {
					t := val.(*autoAttachedEntry).target
					select {
					case <-ctx.Done():
						return
					case t.messageQueue <- m:
					}
				}
				continue
			}

			select {
			case <-ctx.Done():
				return
			case page.messageQueue <- m:
			}

		case <-b.LostConnection:
			return // to avoid "write: broken pipe" errors
		}
	}
}

// BrowserOption is a browser option.
type BrowserOption = func(*Browser)

// WithBrowserLogf is a browser option to specify a func to receive general logging.
func WithBrowserLogf(f func(string, ...any)) BrowserOption {
	return func(b *Browser) { b.logf = f }
}

// WithBrowserErrorf is a browser option to specify a func to receive error logging.
func WithBrowserErrorf(f func(string, ...any)) BrowserOption {
	return func(b *Browser) { b.errf = f }
}

// WithBrowserDebugf is a browser option to specify a func to log actual
// websocket messages.
func WithBrowserDebugf(f func(string, ...any)) BrowserOption {
	return func(b *Browser) { b.dbgf = f }
}

// WithConsolef is a browser option to specify a func to receive chrome log events.
//
// Note: NOT YET IMPLEMENTED.
func WithConsolef(f func(string, ...any)) BrowserOption {
	return func(b *Browser) {}
}

// WithDialTimeout is a browser option to specify the timeout when dialing a
// browser's websocket address. The default is ten seconds; use a zero duration
// to not use a timeout.
func WithDialTimeout(d time.Duration) BrowserOption {
	return func(b *Browser) { b.dialTimeout = d }
}
