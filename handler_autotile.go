package main

import (
	"context"
	"fmt"
	"log/slog"
)

// autotileHandler decides how a newly opened window is fitted into the layout,
// and which way the next one will go.
//
// Sway tiles manually — the split direction is whatever you last set with
// `split h`/`split v` — so without help, opening windows one after another
// keeps slicing the same axis and leaves you with ever-narrower columns. This
// handler picks the direction from the shape of the focused container instead:
// wider than tall means the next window goes beside it, taller than wide means
// it goes below. That much is the behaviour of nwg-piotr/autotiling.
//
// On top of that it knows when not to split at all: past a certain point a
// column is too narrow for anything to be usable in it, and what you really
// want is a tabbed container holding both windows at full size. See
// placeNewWindow.
type autotileHandler struct {
	// splitRatio is the share of the container a newly opened window gets,
	// as a percentage. Sway splits evenly on its own, so 50 means "send no
	// resize command at all".
	splitRatio int
	// tabMinWidth and stackMinHeight are the smallest a new window may end up
	// on the axis of the split before the container is tabbed (or stacked)
	// instead, in pixels. Zero disables that behaviour on the axis.
	tabMinWidth    int
	stackMinHeight int
	// autoTabbed holds the containers this handler turned into tabbed or
	// stacked ones, mapped to the layout they had before, so the change can be
	// undone once they are back to a single window. Containers tabbed by hand
	// are deliberately absent: those are the user's, and stay as they are.
	autoTabbed map[NodeID]string
	// paused, when set and true, makes the handler ignore every event: see
	// exposeHandler.
	paused func() bool
}

func newAutotileHandler(splitRatio, tabMinWidth, stackMinHeight int) *autotileHandler {
	return &autotileHandler{
		splitRatio:     splitRatio,
		tabMinWidth:    tabMinWidth,
		stackMinHeight: stackMinHeight,
		autoTabbed:     make(map[NodeID]string),
	}
}

func (h *autotileHandler) Name() string { return "autotiling" }

func (h *autotileHandler) HandleWindow(ctx *WindowContext) error {
	if h.paused != nil && h.paused() {
		return nil
	}
	switch ctx.Info.Change {
	case WinClose:
		return h.flattenAutoTabbed(ctx)
	case WinNew, WinFocus, WinMove, WinFloating:
	default:
		return nil
	}

	isNew := ctx.Info.Change == WinNew
	if isNew {
		// Must come first: it changes the window's shape, and the split
		// direction below has to be decided on the shape it ends up with, not
		// on the even split Sway just gave it.
		if err := h.placeNewWindow(ctx); err != nil {
			return err
		}
	}

	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	node, parent := findFocusedLeaf(tree)
	if node == nil || parent == nil {
		return nil
	}

	// Floating and fullscreen windows have no meaningful split direction, and
	// in a tabbed or stacked container the children are drawn on top of each
	// other: setting a split there would silently reshape the layout.
	if node.NodeType == "floating_con" || parent.NodeType == "floating_con" {
		return nil
	}
	if node.FullscreenMode != 0 {
		return nil
	}
	if parent.Layout == "tabbed" || parent.Layout == "stacked" {
		return nil
	}

	split := "splith"
	if node.Rect.Height > node.Rect.Width {
		split = "splitv"
	}
	// Two different things happen depending on where the window sits, and both
	// are wanted. A window with no siblings is the layout of its parent, so the
	// command only has an effect when the direction really differs. A window
	// that has siblings gets wrapped in a container of its own instead — which
	// is the whole point: without it Sway would not split *this* window when
	// the next one opens, it would add another sibling and squeeze everything
	// on the workspace. Once wrapped the window has no siblings any more, so
	// the same command on the next focus event is a no-op: this settles rather
	// than nesting deeper and deeper.
	if len(parent.Nodes) == 1 && parent.Layout == split {
		return nil
	}

	// A new window is worth reporting; the adjustments that merely follow the
	// focus around are frequent and would drown the journal, so they stay at
	// debug level.
	level := slog.LevelDebug
	if isNew {
		level = slog.LevelInfo
	}
	ctx.Log.Log(context.Background(), level, "setting split direction",
		"con_id", node.ID, "split", split,
		"width", node.Rect.Width, "height", node.Rect.Height)
	return ctx.Command(fmt.Sprintf("[con_id=%s] %s", node.ID, split))
}

// placeNewWindow gives a freshly opened window either its configured share of
// the container, or — when that share would be too small to work in — a tab of
// its own, by turning the container into a tabbed (or, splitting vertically, a
// stacked) one. Sway splits evenly, so without this every new window would
// halve the space, however little is left.
//
// The choice is made from the size the window is *about* to get — the
// container's size times the ratio — rather than from the size it has on
// screen. Deciding after the fact would mean resizing the window and then
// reshuffling the container anyway: two layout passes, the first one visible.
//
// The `layout` command is addressed to the new window and not to its
// container, because that is what Sway's criteria match: like pressing the
// tabbed-layout binding with the window focused, it applies to the parent.
func (h *autotileHandler) placeNewWindow(ctx *WindowContext) error {
	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	node, parent := findNodeByID(tree, ctx.Info.Container.ID)
	if node == nil || parent == nil {
		return nil
	}
	if node.NodeType == "floating_con" || node.FullscreenMode != 0 {
		return nil
	}
	// Exactly two children means this window has just been paired with the one
	// that was there before, which is the case all of this is about. Anything
	// else (a tabbed or stacked parent, a container built by hand, the very
	// first window on a workspace) is left alone: tabbing a container of five
	// would fold away four windows the user never asked about.
	if len(parent.Nodes) != 2 {
		return nil
	}

	// Note "stacking" and not "stacked": the word the layout command takes is
	// not the one the tree reports back for the same layout.
	var axis, layoutCmd string
	var size, minSize int
	switch parent.Layout {
	case "splith":
		axis, layoutCmd = "width", "tabbed"
		size, minSize = parent.Rect.Width*h.splitRatio/100, h.tabMinWidth
	case "splitv":
		axis, layoutCmd = "height", "stacking"
		size, minSize = parent.Rect.Height*h.splitRatio/100, h.stackMinHeight
	default:
		return nil
	}

	if minSize > 0 && size < minSize {
		h.autoTabbed[parent.ID] = parent.Layout
		ctx.Log.Info("new window would be too small, tabbing the container",
			"con_id", node.ID, "container_id", parent.ID, "layout", layoutCmd,
			axis, size, "minimum", minSize)
		return ctx.Command(fmt.Sprintf("[con_id=%s] layout %s", node.ID, layoutCmd))
	}

	if h.splitRatio == 50 {
		return nil // already what Sway does: no point sending a command
	}
	ctx.Log.Info("resizing new window",
		"con_id", node.ID, "axis", axis, "ratio", h.splitRatio)
	return ctx.Command(fmt.Sprintf("[con_id=%s] resize set %s %d ppt",
		node.ID, axis, h.splitRatio))
}

// flattenAutoTabbed puts a container back the way it was once it is down to a
// single window. Sway keeps a tabbed container alive with one child left, tab
// bar and all, and a tab bar over a lone window is just a wasted row: the
// whole point of it was to hold the second window that has now gone.
//
// Only containers this handler tabbed itself are considered, and only while
// they are still tabbed — anything the user has since taken over is dropped
// from the map untouched.
func (h *autotileHandler) flattenAutoTabbed(ctx *WindowContext) error {
	if len(h.autoTabbed) == 0 {
		return nil
	}
	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}

	// Commands are collected first and sent afterwards: every command
	// invalidates the tree, and the loop below reads from this one snapshot.
	type restore struct {
		container NodeID
		window    NodeID
		layout    string
	}
	var todo []restore

	for id, layout := range h.autoTabbed {
		node, _ := findNodeByID(tree, id)
		if node == nil {
			delete(h.autoTabbed, id) // container gone along with its last window
			continue
		}
		if node.Layout != "tabbed" && node.Layout != "stacked" {
			delete(h.autoTabbed, id) // the user has changed it: not ours any more
			continue
		}
		if len(node.Nodes) != 1 {
			continue
		}
		// The command has to name a window, so a lone child that is itself a
		// container leaves nothing to aim at: naming a window inside it would
		// set the layout of that inner container instead. Rare enough to skip.
		child := &node.Nodes[0]
		if len(child.Nodes) > 0 || len(child.FloatingNodes) > 0 {
			continue
		}
		delete(h.autoTabbed, id)
		todo = append(todo, restore{container: id, window: child.ID, layout: layout})
	}

	for _, r := range todo {
		ctx.Log.Info("one window left, restoring the layout",
			"container_id", r.container, "layout", r.layout)
		if err := ctx.Command(fmt.Sprintf("[con_id=%s] layout %s", r.window, r.layout)); err != nil {
			return err
		}
	}
	return nil
}

// findFocusedLeaf returns the focused window along with its parent container.
// Only leaves are considered: a focused split container is not a window.
func findFocusedLeaf(root *Node) (node, parent *Node) {
	traverseDeepFirst(func(n *Node, p *Node, _ int) {
		if n.Focused && len(n.Nodes) == 0 && len(n.FloatingNodes) == 0 {
			node, parent = n, p
		}
	}, root, nil, 0)
	return node, parent
}

// findNodeByID returns a container by id along with its parent. Used for the
// window an event is about, which is not necessarily the focused one.
func findNodeByID(root *Node, id NodeID) (node, parent *Node) {
	traverseDeepFirst(func(n *Node, p *Node, _ int) {
		if n.ID == id {
			node, parent = n, p
		}
	}, root, nil, 0)
	return node, parent
}
