package main

import (
	"fmt"
	"strings"
)

// exposeCommand is the argument of the `nop` binding that toggles the exposé:
//
//	bindsym XF86LaunchA nop ws-rename-expose
const exposeCommand = "ws-rename-expose"

// exposeHandler lays out every window of the focused workspace side by side
// for a while, so that nothing is left hidden behind a tab bar or a stack, and
// puts the workspace back the way it was on the next press.
//
// It works by switching every tabbed or stacked container on the workspace to
// a split, which shows all of its children at once, and back again: nothing
// moves, no container is created or destroyed, so the way back is just the
// same command with the old layout. A tabbed container shows its focused
// child, so whichever window has the focus when the exposé ends is the tab on
// show afterwards: move the focus to a window and press again to land on it.
//
// While it lasts the other layout handlers stand aside (see paused): autotiling
// in particular would wrap the windows the exposé has just put side by side in
// containers of their own, and the containers to restore would be gone.
type exposeHandler struct {
	// workspace is the exposed workspace, or empty when nothing is exposed.
	workspace NodeID
	// restore lists what to put back, innermost container first.
	restore []exposeRestore
}

// exposeRestore records one container the exposé has split. Unlike maximize
// it keeps the container's own id: the exposé never reshapes the tree, so the
// container is still there to be found, and a child to address the command to
// can be picked at restore time among the ones it has then.
type exposeRestore struct {
	container NodeID
	layout    string // what it was: "tabbed" or "stacked"
	split     string // what the exposé made it: "splith" or "splitv"
}

func newExposeHandler() *exposeHandler { return &exposeHandler{} }

func (h *exposeHandler) Name() string { return "expose" }

// active tells the other layout handlers to keep their hands off.
func (h *exposeHandler) active() bool { return h.workspace != "" }

// HandleWindow does nothing: the exposé only follows its binding. Windows
// opened or closed meanwhile are dealt with at restore time.
func (h *exposeHandler) HandleWindow(*WindowContext) error { return nil }

func (h *exposeHandler) HandleBinding(ctx *WindowContext, command string) error {
	if f := strings.Fields(command); len(f) != 2 || f[0] != "nop" || f[1] != exposeCommand {
		return nil
	}
	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	ws := focusedWorkspace(tree)

	if h.active() {
		// Pressed again: on the exposed workspace that means "done", anywhere
		// else it means "this one instead".
		here := ws != nil && ws.ID == h.workspace
		if err := h.restoreLayout(ctx); err != nil {
			return err
		}
		if here || ws == nil {
			return nil
		}
		if tree, err = ctx.Tree(); err != nil {
			return fmt.Errorf("get_tree: %w", err)
		}
		if ws = focusedWorkspace(tree); ws == nil {
			return nil
		}
	}
	if ws == nil {
		return nil
	}
	return h.expose(ctx, ws)
}

// expose splits every tabbed or stacked container on the workspace.
//
// Each container goes side by side or one above the other depending on the
// shape it will have once its ancestors are split too, not the one it has on
// screen now: a tabbed container inside another one is drawn as big as its
// parent, but after the exposé it only gets its share. So the sizes are
// worked out top-down from the workspace, giving the children of a container
// that is about to be split equal shares, which is what Sway does with them.
func (h *exposeHandler) expose(ctx *WindowContext, ws *Node) error {
	type flip struct {
		exposeRestore
		child NodeID // the command lands on the parent of the node it names
	}
	var flips []flip

	var walk func(n *Node, width, height float64)
	walk = func(n *Node, width, height float64) {
		if len(n.Nodes) == 0 {
			return
		}
		layout := n.Layout
		split := false
		if (layout == "tabbed" || layout == "stacked") && len(n.Nodes) > 1 {
			layout = "splith"
			if height > width {
				layout = "splitv"
			}
			split = true
		}

		// The share of each child along the axis of the split: even for a
		// container about to be split, as on screen for one already split.
		total := 0
		for i := range n.Nodes {
			if layout == "splith" {
				total += n.Nodes[i].Rect.Width
			} else {
				total += n.Nodes[i].Rect.Height
			}
		}
		for i := range n.Nodes {
			c := &n.Nodes[i]
			share := 1.0 / float64(len(n.Nodes))
			if !split && total > 0 {
				if layout == "splith" {
					share = float64(c.Rect.Width) / float64(total)
				} else {
					share = float64(c.Rect.Height) / float64(total)
				}
			}
			switch layout {
			case "splith":
				walk(c, width*share, height)
			case "splitv":
				walk(c, width, height*share)
			default: // still tabbed or stacked: one child, the whole space
				walk(c, width, height)
			}
		}

		// Appended after the children, so the innermost containers come first.
		if split {
			flips = append(flips, flip{
				exposeRestore: exposeRestore{container: n.ID, layout: n.Layout, split: layout},
				child:         n.Nodes[0].ID,
			})
		}
	}
	walk(ws, float64(ws.Rect.Width), float64(ws.Rect.Height))

	if len(flips) == 0 {
		ctx.Log.Info("nothing to expose", "workspace", workspaceName(ws))
		return nil
	}

	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	refocus := refocusCommand(tree)

	restore := make([]exposeRestore, 0, len(flips))
	for _, f := range flips {
		if err := ctx.Command(layoutCommand(f.container == ws.ID, f.child, f.split, refocus)); err != nil {
			return err
		}
		restore = append(restore, f.exposeRestore)
	}
	h.workspace, h.restore = ws.ID, restore
	ctx.Log.Info("exposed", "workspace", workspaceName(ws), "containers", len(restore))
	return nil
}

// restoreLayout puts every container back, outermost first — the reverse of
// the order they were split in.
func (h *exposeHandler) restoreLayout(ctx *WindowContext) error {
	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	restore, ws := h.restore, h.workspace
	h.workspace, h.restore = "", nil

	done := 0
	for i := len(restore) - 1; i >= 0; i-- {
		r := restore[i]
		node, _ := findNodeByID(tree, r.container)
		// Gone with all of its windows, emptied, or given another layout by
		// hand in the meantime: there is nothing to put back, or it is no
		// longer ours to put back.
		if node == nil || len(node.Nodes) == 0 || node.Layout != r.split {
			continue
		}
		// Note "stacking" and not "stacked": the word the layout command takes
		// is not the one the tree reports back for the same layout.
		layout := r.layout
		if layout == "stacked" {
			layout = "stacking"
		}
		cmd := layoutCommand(node.NodeType == "workspace", node.Nodes[0].ID, layout, refocusCommand(tree))
		if err := ctx.Command(cmd); err != nil {
			return err
		}
		done++
		if tree, err = ctx.Tree(); err != nil {
			return fmt.Errorf("get_tree: %w", err)
		}
	}
	ctx.Log.Info("exposé over", "workspace_id", ws, "containers", done)
	return nil
}

// layoutCommand sets the layout of the container holding child.
//
// The command is addressed to the child because Sway's criteria select a node
// while `layout` lands on its parent. That does not work for a workspace,
// though: given a window, Sway leaves the workspace alone and wraps all of its
// children in a new container with the requested layout instead, and the
// container to restore is then gone. The workspace's own layout only changes
// when the workspace itself has the focus, so for a workspace the focus goes
// up there for the duration of the command and is then put back where it was.
func layoutCommand(isWorkspace bool, child NodeID, layout, refocus string) string {
	if !isWorkspace {
		return fmt.Sprintf("[con_id=%s] layout %s", child, layout)
	}
	cmd := fmt.Sprintf("[con_id=%s] focus; focus parent; layout %s", child, layout)
	if refocus != "" {
		cmd += "; " + refocus
	}
	return cmd
}

// refocusCommand returns the command that gives the focus back to whatever
// holds it now, or an empty string when nothing does.
func refocusCommand(root *Node) string {
	var focused *Node
	traverseDeepFirst(func(n *Node, _ *Node, _ int) {
		if n.Focused {
			focused = n
		}
	}, root, nil, 0)
	switch {
	case focused == nil:
		return ""
	case focused.NodeType == "workspace" && focused.Name != nil:
		return fmt.Sprintf("workspace --no-auto-back-and-forth %q", *focused.Name)
	default:
		return fmt.Sprintf("[con_id=%s] focus", focused.ID)
	}
}

// focusedWorkspace returns the workspace holding the focus, whether the focus
// is on a window, a container or the workspace itself (when it is empty).
func focusedWorkspace(root *Node) *Node {
	var focused *Node
	traverseDeepFirst(func(n *Node, _ *Node, _ int) {
		if n.Focused {
			focused = n
		}
	}, root, nil, 0)
	if focused == nil {
		return nil
	}
	var ws *Node
	for _, n := range ancestorPath(root, focused.ID) {
		if n.NodeType == "workspace" {
			ws = n
		}
	}
	return ws
}

func workspaceName(ws *Node) string {
	if ws.Name == nil {
		return string(ws.ID)
	}
	return *ws.Name
}
