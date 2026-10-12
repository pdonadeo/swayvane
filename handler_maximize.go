package main

import "fmt"

// maximizeMark is the Sway mark that drives this handler. Marks beginning
// with an underscore are not drawn in window titles, so it stays invisible.
const maximizeMark = "_wsr_max"

// maximizeHandler gives a window the whole workspace for a while without
// hiding the bar, ignoring the gaps or disturbing the layout, which is what
// `fullscreen` does and why it is the wrong tool for a quick look at
// something.
//
// It works by making every container between the window and the workspace
// tabbed. The window is then the visible tab at each level, so it gets all the
// space there is, minus one tab bar per level; nothing moves, no window is
// closed, and the whole thing is undone by putting each container's layout
// back. Containers that hold a single window are skipped: tabbing them would
// cost a tab bar and hide nothing.
//
// The trigger is the mark rather than a command socket, because Sway can set a
// mark from a plain binding and reports it as a window event this daemon is
// already subscribed to:
//
//	bindsym $mod+z mark --add --toggle _wsr_max
//
// A mark belongs to one window at a time, so marking another window while one
// is maximized restores the first and maximizes the second by itself.
type maximizeHandler struct {
	// window is the marked window whose layout is currently folded away, or
	// empty when nothing is maximized.
	window NodeID
	// restore lists what to put back, innermost container first.
	restore []layoutRestore
	// paused, when set and true, makes the handler ignore every event: see
	// exposeHandler.
	paused func() bool
}

// layoutRestore records one container's previous layout. The container is not
// named directly but through its children: Sway's criteria select a node while
// the layout command lands on its *parent*, and a workspace cannot be named at
// all. All of the children are kept, not just the one the tabbing command was
// addressed to, because that one dies along with the window it holds — closing
// the maximized window would otherwise leave the layout folded for good.
type layoutRestore struct {
	children []NodeID // any of them will do, first one still in the tree wins
	layout   string   // the layout the container had before
}

func newMaximizeHandler() *maximizeHandler { return &maximizeHandler{} }

func (h *maximizeHandler) Name() string { return "maximize" }

func (h *maximizeHandler) HandleWindow(ctx *WindowContext) error {
	if h.paused != nil && h.paused() {
		return nil
	}
	switch ctx.Info.Change {
	case WinMark:
	case WinClose, WinMove:
		// Not triggers, but the maximized window may have just gone away or
		// left the containers we folded, and those would stay tabbed forever.
		if h.window == "" {
			return nil
		}
	default:
		return nil
	}

	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	marked := findMarkedWindow(tree, maximizeMark)

	if h.window != "" && (marked == nil || marked.ID != h.window) {
		if err := h.restoreLayout(ctx); err != nil {
			return err
		}
		if marked == nil {
			return nil
		}
		// The restore commands invalidated the tree the marked window was
		// found in, and its containers have just been reshaped.
		if tree, err = ctx.Tree(); err != nil {
			return fmt.Errorf("get_tree: %w", err)
		}
		if marked = findMarkedWindow(tree, maximizeMark); marked == nil {
			return nil
		}
	}
	if marked == nil || marked.ID == h.window {
		return nil
	}
	return h.maximize(ctx, tree, marked)
}

// maximize tabs every container between the marked window and its workspace.
func (h *maximizeHandler) maximize(ctx *WindowContext, tree *Node, window *Node) error {
	if window.FullscreenMode != 0 {
		return nil // already covering everything, by the user's own choice
	}
	path := ancestorPath(tree, window.ID)
	// Everything above the workspace — the output and the root — is not a
	// layout the user has any use for.
	ws := -1
	for i, n := range path {
		if n.NodeType == "workspace" {
			ws = i
		}
		if n.NodeType == "floating_con" {
			return nil // a floating window is already free of the layout
		}
	}
	if ws < 0 || ws == len(path)-1 {
		return nil // not in a workspace, or is the workspace itself
	}

	// Innermost container first, so that the ids collected on the way stay
	// valid: changing a workspace's layout makes Sway wrap its children in a
	// new container, which would move anything left to do.
	var restore []layoutRestore
	for i := len(path) - 2; i >= ws; i-- {
		container, child := path[i], path[i+1]
		if len(container.Nodes) < 2 {
			continue // nothing hidden by tabbing it, so it would only cost a tab bar
		}
		if container.Layout == "tabbed" || container.Layout == "stacked" {
			continue // the window is already the one on show at this level
		}
		// The addressed child goes first only for readability; on the way back
		// any surviving sibling names the same container just as well.
		children := []NodeID{child.ID}
		for i := range container.Nodes {
			if container.Nodes[i].ID != child.ID {
				children = append(children, container.Nodes[i].ID)
			}
		}
		restore = append(restore, layoutRestore{children: children, layout: container.Layout})
		if err := ctx.Command(fmt.Sprintf("[con_id=%s] layout tabbed", child.ID)); err != nil {
			return err
		}
	}
	if len(restore) == 0 {
		return nil // nothing was in the way: the window already had the space
	}
	h.window, h.restore = window.ID, restore
	ctx.Log.Info("maximized", "con_id", window.ID, "containers", len(restore))
	return nil
}

// restoreLayout undoes maximize, outermost container first — the reverse of
// the order they were folded in.
func (h *maximizeHandler) restoreLayout(ctx *WindowContext) error {
	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	restore, window := h.restore, h.window
	h.window, h.restore = "", nil

	done := 0
	for i := len(restore) - 1; i >= 0; i-- {
		r := restore[i]
		// A container that went away with all of its windows has no layout
		// left to restore, and naming a node that is gone only draws a warning.
		child := firstInTree(tree, r.children)
		if child == "" {
			continue
		}
		if err := ctx.Command(fmt.Sprintf("[con_id=%s] layout %s", child, r.layout)); err != nil {
			return err
		}
		done++
		// Restoring a layout reshapes the tree the remaining lookups read.
		if tree, err = ctx.Tree(); err != nil {
			return fmt.Errorf("get_tree: %w", err)
		}
	}
	ctx.Log.Info("restored", "con_id", window, "containers", done)
	return nil
}

// firstInTree returns the first of the given nodes that is still part of the
// tree, or an empty id when none of them is.
func firstInTree(tree *Node, ids []NodeID) NodeID {
	for _, id := range ids {
		if node, _ := findNodeByID(tree, id); node != nil {
			return id
		}
	}
	return ""
}

// findMarkedWindow returns the window carrying the given mark. Sway keeps a
// mark on one window at a time, so there is never more than one.
func findMarkedWindow(root *Node, mark string) *Node {
	var found *Node
	traverseDeepFirst(func(n *Node, _ *Node, _ int) {
		for _, m := range n.Marks {
			if m == mark {
				found = n
				return
			}
		}
	}, root, nil, 0)
	return found
}

// ancestorPath returns the chain of nodes from the root down to the given id,
// the node itself included, or nil when it is not in the tree. Unlike
// findNodeByID it keeps the whole line of descent, which is what a layout
// spanning several levels of containers needs.
func ancestorPath(root *Node, id NodeID) []*Node {
	if root.ID == id {
		return []*Node{root}
	}
	for _, children := range [][]Node{root.Nodes, root.FloatingNodes} {
		for i := range children {
			if sub := ancestorPath(&children[i], id); sub != nil {
				return append([]*Node{root}, sub...)
			}
		}
	}
	return nil
}
