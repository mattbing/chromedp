package chromedp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	jsonv2 "github.com/chromedp/cdproto/cdp/jsonv2"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
)

var (
	// DefaultUnmarshalOptions are default unmarshal options.
	DefaultUnmarshalOptions = jsonv2.JoinOptions(
		jsonv2.DefaultOptionsV2(),
		jsonv2.AllowInvalidUTF8(true),
	)
	// DefaultMarshalOptions are default marshal options.
	DefaultMarshalOptions = jsonv2.JoinOptions(
		jsonv2.DefaultOptionsV2(),
		jsonv2.AllowInvalidUTF8(true),
	)
)

// Transport is the common interface to send and receive the protocol messages
// of a browser. [PipeConn] implements it, and so does the websocket connection
// of the package github.com/chromedp/chromedp/remote. Browser reads and writes
// its messages through it, and NewBrowserTransport accepts any Transport.
//
// A Transport can also have the method SetDebugf(func(string, ...any)).
// NewBrowserTransport then calls it with the protocol logger of the browser.
type Transport interface {
	Read(context.Context, *cdproto.Message) error
	Write(context.Context, *cdproto.Message) error
	io.Closer
}

// Browser manages a browser through the Chrome DevTools Protocol. It handles
// the browser process runner, the connection to the browser, the targets, and
// the network, page, and DOM events. The connection is a pipe or a websocket.
type Browser struct {
	// next is the next message id.
	// NOTE: it must be 64-bit aligned on 32-bit targets too, so be careful when you move this field.
	// The compiler will do this when https://github.com/golang/go/issues/599 is fixed.
	next int64

	// LostConnection is closed when the connection to Chrome drops. The
	// connection is a pipe or a websocket.
	// Use it to make sure that the context of the Browser is canceled (and the
	// handler stopped) after the connection fails.
	LostConnection chan struct{}

	// closingGracefully is closed by Close before it shuts the browser down
	// gracefully. If the connection to the browser is lost and LostConnection
	// is closed, we then know not to kill the Chrome process at once. This is
	// important, because the browser must shut itself off and save its state
	// to disk.
	closingGracefully chan struct{}

	// lostReason is the error that ended the connection to the browser. It
	// is valid after LostConnection closes.
	lostReason error

	// diedUnexpectedly is true when the connection to the browser process
	// dropped while nobody had asked the browser to stop. The caller did not
	// cancel the context, and did not close the browser. See noteLost.
	diedUnexpectedly atomic.Bool

	// exitErr points to the error of the wait for the browser process. It is
	// nil for a browser that this program did not start. The error is valid
	// after exited closes.
	exitErr *error

	// pages tracks the attached targets by session ID. It is a field only so
	// that the tests can read the map after a browser closes.
	pages map[target.SessionID]*Target

	// autoAttachedSessions tracks the sessions that Target.setAutoAttach
	// creates (EventAttachedToTarget), as opposed to the sessions of an
	// explicit Target.attachToTarget. Those are not in pages, so without this
	// map their messages are dropped and commands on them fail.
	// Each entry owns the cancel func of the Target.run goroutine that it
	// started, so a detach can stop it.
	autoAttachedSessions sync.Map // target.SessionID -> *autoAttachedEntry

	// autoAttachQueue lets the goroutine that reads the connection ask run,
	// the only reader of pages, to register a session that the browser
	// auto-attached and reported on the browser channel.
	autoAttachQueue chan autoAttachReq

	// attaching counts, by target ID, the explicit attaches that
	// Context.attachTarget runs now. The browser also reports an explicit
	// attach on the browser channel, and run must not register that report
	// as an auto-attached session.
	attachingMu sync.Mutex
	attaching   map[target.ID]int

	listenersMu sync.Mutex
	listeners   []cancelableListener

	// events holds the subscriptions made with Subscribe.
	events subscribers

	conn Transport

	// newTabQueue is the queue of requests to create new target handlers,
	// after a new tab is created and attached. The new Target is sent back on
	// newTabResult.
	newTabQueue chan *Target

	// cmdQueue is the outgoing command queue.
	cmdQueue chan *cdproto.Message

	// logging funcs
	logf func(string, ...any)
	errf func(string, ...any)
	dbgf func(string, ...any)

	// The optional fields below help some tests.

	// process can be set by the allocators that start a process when they
	// allocate a browser.
	process *os.Process

	// userDataDir can be set by the allocators that set user data directories
	// directly.
	userDataDir string

	// exited is closed when the browser process exits. It is nil when the
	// allocator does not start the process. WaitClosed uses it.
	exited <-chan struct{}

	// reaped is closed when the wait for the browser process returns, and
	// exitErr is valid then. It can close before exited, because exited also
	// waits for the output of the browser. Child processes of the browser can
	// hold the output open for a while, especially on Windows.
	reaped <-chan struct{}

	// keptOpen is true when the allocator started the browser with KeepOpen.
	// wsURL is then the websocket address of the browser.
	keptOpen bool
	wsURL    string
}

// autoAttachedEntry is a value of Browser.autoAttachedSessions. It pairs the
// auto-attached Target with the cancel func of the goroutine that runs
// Target.run, so that a detach can stop that goroutine.
type autoAttachedEntry struct {
	target *Target
	cancel context.CancelFunc
}

// autoAttachReq asks Browser.run to register the session of ev. run closes
// done when it is finished.
type autoAttachReq struct {
	ev   *target.EventAttachedToTarget
	done chan struct{}
}

// NewBrowserTransport creates a new browser that uses tr, a connection to a
// browser that is already open, such as a [*PipeConn]. The browser closes tr
// when it stops. Typically you do not call it directly, because the Allocator
// interface does it.
func NewBrowserTransport(ctx context.Context, tr Transport, opts ...BrowserOption) (*Browser, error) {
	b := newBrowser(opts)
	if s, ok := tr.(interface{ SetDebugf(func(string, ...any)) }); ok {
		s.SetDebugf(b.dbgf)
	}
	b.conn = tr

	go b.run(ctx)
	return b, nil
}

// newBrowser returns a browser with the options applied, and without a
// connection.
func newBrowser(opts []BrowserOption) *Browser {
	b := &Browser{
		LostConnection:    make(chan struct{}),
		closingGracefully: make(chan struct{}),

		newTabQueue: make(chan *Target),

		// Fit some jobs without blocking, to reduce blocking in Execute.
		cmdQueue: make(chan *cdproto.Message, 32),

		autoAttachQueue: make(chan autoAttachReq),
		attaching:       make(map[target.ID]int),

		logf: log.Printf,
	}
	// apply options
	for _, o := range opts {
		o(b)
	}
	// make sure that errf is set
	if b.errf == nil {
		b.errf = func(s string, v ...any) { b.logf("ERROR: "+s, v...) }
	}
	return b
}

// Process returns the process object of the browser.
//
// It is nil when the browser was allocated by an allocator that attaches to a
// browser that runs already, such as the allocator of the remote package. A monitoring
// system can use it to collect process metrics of the browser process (see
// [prometheus.NewProcessCollector] for an example).
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

func (b *Browser) newTarget(ctx context.Context, targetID target.ID, sessionID target.SessionID) (*Target, error) {
	if targetID == "" {
		return nil, errors.New("empty target ID")
	}
	if sessionID == "" {
		return nil, errors.New("empty session ID")
	}
	// The caller runs the target with ctx. Set it before the send, so that
	// run can read it after it receives the target.
	t := b.makeTarget(targetID, sessionID)
	t.ctx = ctx

	// This send must block, so that the tab is in the map before any more
	// target events are routed.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case b.newTabQueue <- t:
	}
	return t, nil
}

// makeTarget returns a Target for the session, which does not run yet.
func (b *Browser) makeTarget(targetID target.ID, sessionID target.SessionID) *Target {
	return &Target{
		browser:   b,
		TargetID:  targetID,
		SessionID: sessionID,

		messageQueue:  make(chan *cdproto.Message, 1024),
		detached:      make(chan struct{}),
		frames:        make(map[cdp.FrameID]*Frame),
		execContexts:  make(map[cdp.FrameID]runtime.ExecutionContextID),
		execUniqueIDs: make(map[cdp.FrameID]string),
		cur:           cdp.FrameID(targetID),

		logf: b.logf,
		errf: b.errf,
	}
}

// registerAutoAttachedSession starts a Target for a session that the browser
// auto-attached (EventAttachedToTarget), rather than one of an explicit
// Target.attachToTarget. The Target is registered in autoAttachedSessions, so
// that its messages are routed and commands can be sent on it. Pass the
// context of the target that opened or holds the session as ctx, so that the
// session ends with that target.
// Only the first call for a session ID takes effect.
func (b *Browser) registerAutoAttachedSession(ctx context.Context, sessionID target.SessionID, targetID target.ID) {
	if sessionID == "" || targetID == "" {
		return
	}
	t := b.makeTarget(targetID, sessionID)
	childCtx, cancel := context.WithCancel(ctx)
	t.ctx = childCtx
	entry := &autoAttachedEntry{target: t, cancel: cancel}
	if _, loaded := b.autoAttachedSessions.LoadOrStore(sessionID, entry); loaded {
		cancel()
		return
	}
	go t.run(childCtx)
	go func() {
		// The session ends at its detach, at the end of its parent, or at
		// the end of the browser. In each case, fail the pending commands
		// with ErrTargetDetached, stop the target and remove the map entry.
		select {
		case <-t.detached:
		case <-childCtx.Done():
		}
		t.markDetached()
		cancel()
		b.autoAttachedSessions.CompareAndDelete(sessionID, entry)
	}()
}

// detachAutoAttachedSession routes a detach to an auto-attached session. The
// session stops after it handles the messages queued before the detach.
func (b *Browser) detachAutoAttachedSession(ctx context.Context, sessionID target.SessionID) bool {
	val, ok := b.autoAttachedSessions.LoadAndDelete(sessionID)
	if !ok {
		return false
	}
	entry := val.(*autoAttachedEntry)
	select {
	case <-ctx.Done():
	case <-entry.target.ctx.Done():
	case entry.target.messageQueue <- detachedMessage:
	}
	return true
}

// AutoAttachedSession returns the session of a target that the browser
// auto-attached with Target.setAutoAttach, such as an out-of-process iframe.
// Use it, for example, to call Runtime.runIfWaitingForDebugger on a target
// that waits for the debugger.
//
// The session is known before the funcs of [Listen] and [ListenBrowser] get
// its Target.attachedToTarget event, so such a func can look it up at once.
// It returns false for an unknown session, and again after the session
// detaches or its parent ends. Its commands then fail with ErrTargetDetached.
// The session has no domain enabled. Do not run actions with it. To run
// actions in the target, use [WithTargetID].
func (b *Browser) AutoAttachedSession(sessionID target.SessionID) (cdp.Session, bool) {
	if val, ok := b.autoAttachedSessions.Load(sessionID); ok {
		return val.(*autoAttachedEntry).target, true
	}
	return nil, false
}

// Call sends the command to the browser, waits for the response, and decodes
// the result into res. It satisfies [cdp.Session].
//
// Call returns a browser error as a [*cdproto.Error]. It returns an error when
// the connection to the browser is lost, also when ctx never ends. The error
// wraps the reason of the loss and [context.Canceled].
func (b *Browser) Call(ctx context.Context, method string, params, res any) error {
	// Certain methods are not available to the user directly.
	if method == browser.CommandClose {
		return fmt.Errorf("to close the browser gracefully, use chromedp.Cancel")
	}
	return b.execute(ctx, method, params, res)
}

// Subscribe starts to buffer the browser events with the method, and returns
// the channel that the raw event parameters arrive on. It satisfies
// [cdp.Session].
func (b *Browser) Subscribe(method string) (<-chan jsonv2.Value, func()) {
	return b.events.subscribe(method)
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
	case <-b.LostConnection:
		return b.lostError(ctx)
	case b.cmdQueue <- cmd:
	}

	// wait for result
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.LostConnection:
		// The reply can arrive just before the loss is known.
		select {
		case msg := <-ch:
			if msg != nil {
				return decodeReply(msg, res)
			}
		default:
		}
		return b.lostError(ctx)
	case msg := <-ch:
		return decodeReply(msg, res)
	}
}

// decodeReply returns the error of a reply to a command, or decodes its result
// into res. A nil msg means that the channel of the reply closed.
func decodeReply(msg *cdproto.Message, res any) error {
	switch {
	case msg == nil:
		return ErrChannelClosed
	case msg.Error != nil:
		return msg.Error
	case res != nil:
		return jsonv2.Unmarshal(msg.Result, res, DefaultUnmarshalOptions)
	}
	return nil
}

// lostError returns the error of a call that cannot finish, because the
// connection to the browser is lost. It wraps the reason that the connection
// gave. It wraps [context.Canceled] too, because the allocator cancels the
// context of the browser when the connection drops, and an earlier version
// returned the error of that context. Call it only after LostConnection is
// closed.
//
// A canceled ctx wins, so that a call with a context that the program canceled
// returns the error of that context, as before.
func (b *Browser) lostError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reason := b.lostReason
	if reason == nil {
		reason = io.EOF
	}
	return fmt.Errorf("lost the connection to the browser: %w: %w", reason, context.Canceled)
}

func (b *Browser) run(ctx context.Context) {
	defer b.conn.Close()
	defer b.events.close()

	// incomingQueue is the queue of the messages of the targets, to be routed
	// by their session ID. It also holds the detach of each session, in the
	// order in which the browser sent it. So the loop routes a detach after
	// every message of the session that the browser sent before it.
	incomingQueue := make(chan sessionMessage, 1)

	// This goroutine reads events from the websocket connection all the time.
	// It needs its own goroutine, because a websocket read blocks and cannot
	// be part of a select statement.
	go func() {
		// Signal to run and exit the browser cleanup goroutine.
		defer close(b.LostConnection)

		for {
			msg := new(cdproto.Message)
			if err := b.conn.Read(ctx, msg); err != nil {
				var syntaxErr *jsonv2.SyntacticError
				if errors.As(err, &syntaxErr) {
					b.errf("%s", err)
				}
				b.noteReadError(ctx, err)
				return
			}

			switch {
			case msg.SessionID != "" && (msg.Method != "" || msg.ID != 0):
				select {
				case <-ctx.Done():
					b.lostReason = ctx.Err()
					return
				case incomingQueue <- sessionMessage{msg: msg}:
				}

			case msg.Method != "":
				b.events.publish(string(msg.Method), msg.Params)
				ev, err := cdproto.UnmarshalMessage(msg, DefaultUnmarshalOptions)
				if err != nil {
					b.errf("%s", err)
					continue
				}

				// Register an auto-attached session before the listeners
				// run, so that a listener can send commands on it at once.
				if ev, ok := ev.(*target.EventAttachedToTarget); ok && ev.TargetInfo != nil {
					req := autoAttachReq{ev: ev, done: make(chan struct{})}
					select {
					case <-ctx.Done():
						b.lostReason = ctx.Err()
						return
					case b.autoAttachQueue <- req:
					}
					select {
					case <-ctx.Done():
						b.lostReason = ctx.Err()
						return
					case <-req.done:
					}
				}

				b.listenersMu.Lock()
				b.listeners = runListeners(b.listeners, ev)
				b.listenersMu.Unlock()

				if ev, ok := ev.(*target.EventDetachedFromTarget); ok {
					select {
					case <-ctx.Done():
						b.lostReason = ctx.Err()
						return
					case incomingQueue <- sessionMessage{detached: ev.SessionID}:
					}
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
	// Stop the auto-attached sessions that remain. Detaches and the end of
	// their parents normally stop them, but the connection can drop first,
	// and a session without a known opener has the browser ctx as its parent.
	defer b.autoAttachedSessions.Range(func(key, val any) bool {
		val.(*autoAttachedEntry).cancel()
		b.autoAttachedSessions.Delete(key)
		return true
	})
	for {
		select {
		case <-ctx.Done():
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
			// The browser also reports an explicit attach on the browser
			// channel, so the reader can have registered this session as an
			// auto-attached one. pages routes it now, so stop the duplicate.
			if val, ok := b.autoAttachedSessions.LoadAndDelete(t.SessionID); ok {
				val.(*autoAttachedEntry).cancel()
			}

		case req := <-b.autoAttachQueue:
			b.registerFromBrowserChannel(ctx, req.ev)
			close(req.done)

		case m := <-incomingQueue:
			if m.msg == nil {
				sessionID := m.detached
				if _, ok := b.pages[sessionID]; !ok {
					if !b.detachAutoAttachedSession(ctx, sessionID) {
						b.errf("executor for %q doesn't exist", sessionID)
					}
					continue
				}
				if page, ok := b.pages[sessionID]; ok {
					// Tell the target after its last routed message. The target
					// then ends the commands that wait for a reply that never comes.
					select {
					case <-ctx.Done():
						return
					case page.messageQueue <- detachedMessage:
					}
				}
				delete(b.pages, sessionID)
				continue
			}

			page, ok := b.pages[m.msg.SessionID]
			if !ok {
				val, ok := b.autoAttachedSessions.Load(m.msg.SessionID)
				if !ok {
					// A page that we closed recently still sends events.
					continue
				}
				page = val.(*autoAttachedEntry).target
				select {
				case <-ctx.Done():
					return
				case <-page.ctx.Done():
					// The session ended, and nobody reads its queue.
				case page.messageQueue <- m.msg:
				}
				continue
			}

			select {
			case <-ctx.Done():
				return
			case page.messageQueue <- m.msg:
			}

		case <-b.LostConnection:
			return // to avoid "write: broken pipe" errors
		}
	}
}

// registerFromBrowserChannel registers a session that the browser
// auto-attached and reported on the browser channel. The session ends with the
// page that opened it, or with the browser when no page did. Only run calls it,
// because it reads pages.
func (b *Browser) registerFromBrowserChannel(ctx context.Context, ev *target.EventAttachedToTarget) {
	if _, ok := b.pages[ev.SessionID]; ok || b.isAttaching(ev.TargetInfo.TargetID) {
		return // the report of an explicit attach
	}
	parent := ctx
	for _, t := range b.pages {
		if t.TargetID == ev.TargetInfo.OpenerID {
			parent = t.ctx
			break
		}
	}
	b.registerAutoAttachedSession(parent, ev.SessionID, ev.TargetInfo.TargetID)
}

// beginAttach and endAttach mark the explicit attach of a target. See
// Browser.attaching.
func (b *Browser) beginAttach(id target.ID) {
	b.attachingMu.Lock()
	b.attaching[id]++
	b.attachingMu.Unlock()
}

func (b *Browser) endAttach(id target.ID) {
	b.attachingMu.Lock()
	if b.attaching[id]--; b.attaching[id] <= 0 {
		delete(b.attaching, id)
	}
	b.attachingMu.Unlock()
}

func (b *Browser) isAttaching(id target.ID) bool {
	b.attachingMu.Lock()
	defer b.attachingMu.Unlock()
	return b.attaching[id] > 0
}

// sessionMessage is an item of the queue from the goroutine that reads the
// messages of the browser to the loop that routes them. It holds a message of a
// target, or the detach of the session detached when msg is nil.
type sessionMessage struct {
	msg      *cdproto.Message
	detached target.SessionID
}

// noteReadError records why the connection to the browser is lost. The reader
// goroutine calls it before it closes LostConnection, so that a call that sees
// the loss can read the reason and the flag diedUnexpectedly. See noteLost.
func (b *Browser) noteReadError(ctx context.Context, err error) {
	b.lostReason = err
	select {
	case <-b.closingGracefully:
	default:
		b.noteLost(ctx)
	}
}

// noteLost records that the connection to the browser process dropped. The
// allocator calls it when the connection drops without a graceful close. If
// the context ctx of the allocation is done, the program stopped the browser,
// so the loss is no surprise. Otherwise the process died, for example when the
// kernel killed it for lack of memory, or when it crashed.
//
// The allocator calls noteLost before it cancels the context. A call that
// fails because of the cancellation then sees the flag.
func (b *Browser) noteLost(ctx context.Context) {
	select {
	case <-ctx.Done():
	default:
		b.diedUnexpectedly.Store(true)
	}
}

// browserExitError is the error of a call that failed because the browser
// process died. It holds the error of the call and the exit error of the
// process.
type browserExitError struct {
	// exit is the error of the wait for the process, such as "signal: killed".
	exit error
	// err is the error that the call returned.
	err error
}

// Error satisfies the error interface.
func (e *browserExitError) Error() string {
	return fmt.Sprintf("browser process exited: %v: %v", e.exit, e.err)
}

// Unwrap returns the exit error and the error of the call, so that errors.Is
// and errors.As find both. For example, errors.As finds the *exec.ExitError,
// and errors.Is finds context.Canceled.
func (e *browserExitError) Unwrap() []error {
	return []error{e.exit, e.err}
}

// withExitError returns err with the exit error of the browser process, when
// the process died while nobody had asked the browser to stop. Otherwise it
// returns err. It waits a short time for the process to be reaped.
func (b *Browser) withExitError(err error) error {
	if err == nil || b == nil || !b.diedUnexpectedly.Load() || b.reaped == nil || b.exitErr == nil {
		return err
	}
	var exitErr *browserExitError
	if errors.As(err, &exitErr) {
		return err
	}
	select {
	case <-b.reaped:
	case <-time.After(time.Second):
		return err
	}
	exit := *b.exitErr
	if exit == nil {
		exit = errors.New("exit status 0")
	}
	return &browserExitError{exit: exit, err: err}
}

// BrowserOption is a browser option.
type BrowserOption = func(*Browser)

// WithBrowserLogf is a browser option that sets the func that receives general
// log messages.
func WithBrowserLogf(f func(string, ...any)) BrowserOption {
	return func(b *Browser) { b.logf = f }
}

// WithBrowserErrorf is a browser option that sets the func that receives error
// messages.
func WithBrowserErrorf(f func(string, ...any)) BrowserOption {
	return func(b *Browser) { b.errf = f }
}

// WithBrowserDebugf is a browser option that sets the func that receives the
// protocol messages.
func WithBrowserDebugf(f func(string, ...any)) BrowserOption {
	return func(b *Browser) { b.dbgf = f }
}

// WithConsolef is a browser option that sets the func that receives chrome log
// events.
//
// Note: it is not implemented yet.
func WithConsolef(f func(string, ...any)) BrowserOption {
	return func(b *Browser) {}
}
