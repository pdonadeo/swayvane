package main

import (
	"log/slog"
	"runtime/debug"
)

// ── Handler interface ─────────────────────────────────────────────────────────

// WindowHandler reacts to Sway "window" events. Handlers are invoked in
// sequence from the event loop goroutine, one event at a time: their internal
// state needs no locking, but in exchange they must not block for long.
type WindowHandler interface {
	// Name identifies the handler in log records.
	Name() string
	// HandleWindow is called for every window event; filtering on
	// ctx.Info.Change is up to the handler.
	HandleWindow(ctx *WindowContext) error
}

// BindingHandler is implemented by handlers that are also driven by key
// bindings. Sway reports every binding it runs, `nop` ones included, so a
// binding like `bindsym F3 nop something` reaches the daemon without a command
// socket of its own. The daemon only subscribes to binding events when at
// least one handler implements this.
type BindingHandler interface {
	// HandleBinding is called for every binding Sway runs; the command is the
	// one written in the configuration, and recognizing it is up to the
	// handler. ctx.Info carries no window: Info.Change is WinUnknown.
	HandleBinding(ctx *WindowContext, command string) error
}

// ConnectHandler is implemented by handlers that have something to do as soon
// as the daemon is connected, before any event: typically finishing what a
// previous run left halfway.
type ConnectHandler interface {
	HandleConnect(ctx *WindowContext) error
}

// ── Per-event context ─────────────────────────────────────────────────────────

// WindowContext is everything a handler gets to see and do about a single
// event. Handlers never touch *Conn directly: going through the context keeps
// the tree fetch shared, the command log centralized, and the cached tree
// consistent with the commands already issued for this event.
type WindowContext struct {
	Info WindowEventInfo // the event itself: Change plus the container
	Log  *slog.Logger    // already tagged with handler=<name>

	conn    *Conn
	tree    *Node
	treeErr error
	treeOK  bool
}

// Tree returns the Sway layout tree, fetching it at most once per event and
// sharing it across handlers: with several handlers interested in the same
// event, a naive implementation would issue one get_tree round-trip each.
func (c *WindowContext) Tree() (*Node, error) {
	if !c.treeOK {
		tree, err := c.conn.GetTree()
		c.treeOK = true
		if err != nil {
			c.tree, c.treeErr = nil, err
		} else {
			c.tree, c.treeErr = &tree, nil
		}
	}
	return c.tree, c.treeErr
}

// Command sends a command to Sway and reports failures. Any command may
// change the layout, so the cached tree is dropped: a handler running later in
// the chain must not see the state as it was before the move.
func (c *WindowContext) Command(cmd string) error {
	c.Log.Debug("SENDING COMMAND", "cmd", cmd)
	outcomes, err := c.conn.Command(cmd)
	c.tree, c.treeErr, c.treeOK = nil, nil, false
	if err != nil {
		return err
	}
	for i, o := range outcomes {
		if o.Success {
			continue
		}
		errStr := ""
		if o.Error != nil {
			errStr = *o.Error
		}
		c.Log.Warn("command rejected by the compositor",
			"index", i+1, "cmd", cmd, "error", errStr)
	}
	return nil
}

// ── Dispatcher ────────────────────────────────────────────────────────────────

// dispatcher fans a single event out to every registered handler. Handlers run
// in registration order, which matters: one that issues commands should be
// registered before one that reads the resulting layout.
type dispatcher struct {
	handlers []WindowHandler
	log      *slog.Logger
}

func newDispatcher(log *slog.Logger, handlers ...WindowHandler) *dispatcher {
	return &dispatcher{handlers: handlers, log: log}
}

func (d *dispatcher) dispatch(conn *Conn, info WindowEventInfo) {
	ctx := &WindowContext{Info: info, conn: conn}
	for _, h := range d.handlers {
		d.runOne(h, ctx, h.HandleWindow)
	}
}

func (d *dispatcher) dispatchBinding(conn *Conn, command string) {
	ctx := &WindowContext{Info: WindowEventInfo{Change: WinUnknown}, conn: conn}
	for _, h := range d.handlers {
		if bh, ok := h.(BindingHandler); ok {
			d.runOne(h, ctx, func(ctx *WindowContext) error {
				return bh.HandleBinding(ctx, command)
			})
		}
	}
}

func (d *dispatcher) dispatchConnect(conn *Conn) {
	ctx := &WindowContext{Info: WindowEventInfo{Change: WinUnknown}, conn: conn}
	for _, h := range d.handlers {
		if ch, ok := h.(ConnectHandler); ok {
			d.runOne(h, ctx, ch.HandleConnect)
		}
	}
}

// subscriptions lists the Sway events the registered handlers need.
func (d *dispatcher) subscriptions() []string {
	events := []string{"window", "shutdown"}
	for _, h := range d.handlers {
		if _, ok := h.(BindingHandler); ok {
			return append(events, "binding")
		}
	}
	return events
}

// runOne keeps handlers in separate fault domains: a panic or an error in one
// of them is logged but neither stops the others nor brings the daemon down.
func (d *dispatcher) runOne(h WindowHandler, ctx *WindowContext, handle func(*WindowContext) error) {
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("handler panicked", "handler", h.Name(),
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	ctx.Log = d.log.With("handler", h.Name())
	if err := handle(ctx); err != nil {
		ctx.Log.Error("handler failed", "error", err)
	}
}
