package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// maximizeMark is the Sway mark that drives this handler. Marks beginning
// with an underscore are not drawn in window titles, so it stays invisible.
const maximizeMark = "_swayvane_max"

// maximizeStash is the workspace the other windows are parked on while one is
// maximized. Its number puts it after the ten workspaces in everyday use, and
// it is in plain sight in the bar on purpose: should anything go wrong, the
// windows are there to be fetched by hand.
const maximizeStash = "11:⏸"

// Marks used for a moment while putting things back, to give `move container
// to mark` something to aim at.
const (
	maximizeDestMark  = "_swayvane_max_dest"
	maximizeAfterMark = "_swayvane_max_after"
)

// maximizeHandler gives a window the whole workspace for a while without
// hiding the bar or ignoring the gaps, which is what `fullscreen` does and why
// it is the wrong tool for a quick look at something.
//
// It works by parking everything else on another workspace. All the tiled
// windows of the workspace are first wrapped in a single container, which is
// moved to maximizeStash in one piece; the marked window is then brought back,
// alone. Sway lets a container moved onto an empty workspace merge into it,
// layout and all, so the parked windows keep their tree, their container ids
// and their proportions while they are away, and the same move done the other
// way round puts them back. The only thing that really changes is the place
// the marked window was taken from, so that is what the plan records.
//
// The trigger is the mark rather than a command socket, because Sway can set a
// mark from a plain binding and reports it as a window event this daemon is
// already subscribed to:
//
//	bindsym $mod+z mark --add --toggle _swayvane_max
//
// A mark belongs to one window at a time, so marking another window while one
// is maximized restores the first and maximizes the second by itself.
type maximizeHandler struct {
	// plan describes the maximized window and how to put it back, or is nil
	// when nothing is maximized.
	plan *maximizePlan
	// stateFile keeps the plan across a restart of the daemon, so that the
	// parked windows are brought back even then. Empty when there is nowhere
	// to keep it.
	stateFile string
	// endOn lists the `nop` bindings that end a maximize first: those of the
	// exposés, which are there to show every window, and most of them are
	// parked.
	endOn []string
	// paused, when set and true, makes the handler ignore every event: see
	// exposeHandler.
	paused func() bool
}

// maximizePlan is what it takes to put the marked window back where it was.
//
// The window was taken out of Container, at Index. When it was wrapped in
// containers of its own, holding nothing else, those went away with it and
// Wrappers lists their layouts, innermost first, to build them again; Index
// is then the position of the outermost of them.
type maximizePlan struct {
	Window        NodeID `json:"window"`
	Workspace     NodeID `json:"workspace"`
	WorkspaceName string `json:"workspace_name"`
	// Container is empty when the window hung from the workspace itself.
	Container NodeID   `json:"container"`
	Index     int      `json:"index"`
	Wrappers  []string `json:"wrappers,omitempty"`
	// Layout, Children and Percents are the container as it was, so that its
	// proportions can be given back once the window is in again; AxisSize is
	// its size in pixels along the split.
	Layout   string    `json:"layout"`
	Children []NodeID  `json:"children"`
	Percents []float64 `json:"percents"`
	AxisSize int       `json:"axis_size"`
}

func newMaximizeHandler() *maximizeHandler {
	h := &maximizeHandler{}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		h.stateFile = filepath.Join(dir, "swayvane", "maximize.json")
	}
	return h
}

func (h *maximizeHandler) Name() string { return "maximize" }

// HandleConnect finishes a maximize left halfway by a previous run of the
// daemon: the windows it parked would otherwise stay on the stash workspace.
func (h *maximizeHandler) HandleConnect(ctx *WindowContext) error {
	if h.plan != nil || h.stateFile == "" {
		return nil
	}
	data, err := os.ReadFile(h.stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", h.stateFile, err)
	}
	var plan maximizePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		h.forget(ctx)
		return fmt.Errorf("parse %s: %w", h.stateFile, err)
	}
	ctx.Log.Info("found a maximized window from a previous run, restoring it", "con_id", plan.Window)
	h.plan = &plan
	return h.restoreLayout(ctx)
}

// HandleBinding ends a maximize when an exposé is called for.
func (h *maximizeHandler) HandleBinding(ctx *WindowContext, command string) error {
	if h.plan == nil || (h.paused != nil && h.paused()) {
		return nil
	}
	for _, arg := range h.endOn {
		if isNopBinding(command, arg) {
			return h.restoreLayout(ctx)
		}
	}
	return nil
}

func (h *maximizeHandler) HandleWindow(ctx *WindowContext) error {
	if h.paused != nil && h.paused() {
		return nil
	}
	switch ctx.Info.Change {
	case WinMark:
	case WinClose, WinMove, WinFloating:
		// Not triggers, but the maximized window may have just gone away or
		// left its workspace, and the parked windows have to follow.
		if h.plan == nil {
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

	if h.plan != nil {
		if marked != nil && marked.ID == h.plan.Window && h.plan.atHome(tree) {
			return nil // still maximized: the event was about something else
		}
		if err = h.restoreLayout(ctx); err != nil {
			return err
		}
		// The windows have just been moved back, so the tree is a new one.
		if tree, err = ctx.Tree(); err != nil {
			return fmt.Errorf("get_tree: %w", err)
		}
		marked = findMarkedWindow(tree, maximizeMark)
	}
	if marked == nil {
		return nil
	}
	return h.maximize(ctx, tree, marked)
}

// atHome tells whether the maximized window is still a tiled window of the
// workspace it was maximized on.
func (p *maximizePlan) atHome(tree *Node) bool {
	path := ancestorPath(tree, p.Window)
	if path == nil {
		return false
	}
	home := false
	for _, n := range path {
		if n.NodeType == "floating_con" {
			return false
		}
		if n.NodeType == "workspace" {
			home = n.ID == p.Workspace
		}
	}
	return home
}

// maximize parks every other tiled window of the workspace on maximizeStash.
func (h *maximizeHandler) maximize(ctx *WindowContext, tree *Node, window *Node) error {
	if window.FullscreenMode != 0 {
		return nil // already covering everything, by the user's own choice
	}
	path := ancestorPath(tree, window.ID)
	wsAt := -1
	for i, n := range path {
		if n.NodeType == "workspace" {
			wsAt = i
		}
		if n.NodeType == "floating_con" {
			return nil // a floating window is already free of the layout
		}
	}
	if wsAt < 0 || wsAt == len(path)-1 {
		return nil // not in a workspace, or is the workspace itself
	}
	ws := path[wsAt]
	if workspaceName(ws) == maximizeStash {
		return nil // nowhere to park the others
	}
	if len(tiledWindows(ws)) < 2 {
		return nil // nothing in the way: the window already has the space
	}
	if stash := findWorkspaceByName(tree, maximizeStash); stash != nil && len(stash.Nodes) > 0 {
		// Parking more windows there would mix them up with what is already
		// there, and the way back would take the lot.
		ctx.Log.Warn("the stash workspace is not empty, not maximizing", "workspace", maximizeStash)
		return nil
	}

	// Climb past the containers holding nothing but the window: they go away
	// as soon as it leaves, and are built again on the way back.
	top := len(path) - 1
	for top-1 > wsAt && len(path[top-1].Nodes) == 1 {
		top--
	}
	var wrappers []string
	for i := len(path) - 2; i >= top; i-- {
		wrappers = append(wrappers, path[i].Layout)
	}
	container := path[top-1]
	plan := &maximizePlan{
		Window:        window.ID,
		Workspace:     ws.ID,
		WorkspaceName: workspaceName(ws),
		Wrappers:      wrappers,
		Layout:        container.Layout,
	}
	if container.NodeType != "workspace" {
		plan.Container = container.ID
	}
	for i := range container.Nodes {
		c := &container.Nodes[i]
		if c.ID == path[top].ID {
			plan.Index = i
		}
		pct := 0.0
		if c.Percent != nil {
			pct = *c.Percent
		}
		plan.Children = append(plan.Children, c.ID)
		plan.Percents = append(plan.Percents, pct)
	}
	switch container.Layout {
	case "splith":
		plan.AxisSize = container.Rect.Width
	case "splitv":
		plan.AxisSize = container.Rect.Height
	}

	// Parking windows moves the focus around, and it goes back where it was,
	// unless that was on this workspace: then it goes to the window being
	// maximized, since any other window here is about to be parked, and
	// following the focus there would show the stash.
	refocus := refocusCommand(tree)
	if f := focusedWorkspace(tree); f != nil && f.ID == ws.ID {
		refocus = fmt.Sprintf("[con_id=%s] focus", window.ID)
	}

	// Everything tiled goes into one container first, so that it can travel
	// as a single piece and keep its shape.
	if err := ctx.Command(wrapCommand(ws)); err != nil {
		return err
	}
	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	if ws, _ = findNodeByID(tree, ws.ID); ws == nil || len(ws.Nodes) != 1 {
		return errors.New("could not gather the workspace in a single container")
	}

	cmds := []string{
		fmt.Sprintf("[con_id=%s] move container to workspace %s", ws.Nodes[0].ID, swayQuote(maximizeStash)),
		fmt.Sprintf("[con_id=%s] move container to workspace %s", window.ID, swayQuote(plan.WorkspaceName)),
	}
	// A tabbed or stacked workspace keeps its tab bar even over a single
	// window, which is all this is about getting rid of. The way back sets
	// the layout again on its own.
	if ws.Layout == "tabbed" || ws.Layout == "stacked" {
		cmds = append(cmds, layoutCommand(true, window.ID, "splith", refocus))
	} else if refocus != "" {
		cmds = append(cmds, refocus)
	}
	if err = ctx.Command(strings.Join(cmds, "; ")); err != nil {
		return err
	}

	// A workspace left empty without the focus is destroyed on the spot, and
	// the window then lands on a new one by the same name: the plan must name
	// the workspace the window is on now.
	if tree, err = ctx.Tree(); err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	for _, n := range ancestorPath(tree, window.ID) {
		if n.NodeType == "workspace" {
			plan.Workspace, plan.WorkspaceName = n.ID, workspaceName(n)
		}
	}

	h.plan = plan
	h.save(ctx)
	ctx.Log.Info("maximized", "con_id", window.ID, "workspace", plan.WorkspaceName)
	return nil
}

// restoreLayout brings the parked windows back and puts the maximized one in
// its place among them.
//
// Should the maximized window have gone in the meantime — closed, moved to
// another workspace, made floating — the others come back all the same, as if
// it had gone from the original layout. Windows opened while it was maximized
// end up next to it.
func (h *maximizeHandler) restoreLayout(ctx *WindowContext) error {
	p := h.plan
	h.plan = nil
	h.forget(ctx)

	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	ws, _ := findNodeByID(tree, p.Workspace)
	if ws != nil && ws.NodeType != "workspace" {
		ws = nil
	}
	wsName := p.WorkspaceName
	if ws != nil {
		wsName = workspaceName(ws)
	}
	home := p.atHome(tree)
	window, _ := findNodeByID(tree, p.Window)
	refocus := refocusCommand(tree)

	stash := findWorkspaceByName(tree, maximizeStash)
	if stash == nil || len(stash.Nodes) == 0 {
		ctx.Log.Warn("nothing parked to bring back", "workspace", maximizeStash)
		return h.unmark(ctx, window)
	}

	// The parked windows travel back as one piece, which needs a container.
	if err = ctx.Command(wrapCommand(stash)); err != nil {
		return err
	}
	if tree, err = ctx.Tree(); err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	if stash = findWorkspaceByName(tree, maximizeStash); stash == nil || len(stash.Nodes) != 1 {
		return errors.New("could not gather the stash workspace in a single container")
	}
	parked := &stash.Nodes[0]
	if ws != nil {
		ws, _ = findNodeByID(tree, ws.ID)
	}

	// Where the window goes: its old container, or the piece standing for
	// the workspace itself.
	dest, index := parked, p.Index
	if p.Container != "" {
		if n, _ := findNodeByID(parked, p.Container); n != nil {
			dest = n
		} else {
			index = -1 // gone with all of its windows: anywhere will do
		}
	}

	var cmds []string
	var floating []NodeID
	var extras []*Node
	if ws != nil {
		for i := range ws.FloatingNodes {
			floating = append(floating, ws.FloatingNodes[i].ID)
		}
		for _, n := range tiledWindows(ws) {
			if !home || n.ID != p.Window {
				extras = append(extras, n)
			}
		}
	}
	// Sway only lets the piece merge into the workspace when there is nothing
	// else on it, floating windows included, so those step aside for a moment.
	for _, id := range floating {
		cmds = append(cmds, fmt.Sprintf("[con_id=%s] move container to workspace %s", id, swayQuote(maximizeStash)))
	}
	cmds = append(cmds, fmt.Sprintf("[con_id=%s] mark --add %s", dest.ID, maximizeDestMark))
	anchor := maximizeDestMark
	if home {
		// Moved onto a container, a window becomes its last child; swapping
		// it with the ones before walks it back to its place.
		cmds = append(cmds,
			fmt.Sprintf("[con_id=%s] move container to mark %s", p.Window, maximizeDestMark),
			fmt.Sprintf("[con_id=%s] mark --add %s", p.Window, maximizeAfterMark))
		if index >= 0 {
			for k := len(dest.Nodes) - 1; k >= index; k-- {
				cmds = append(cmds, fmt.Sprintf("[con_id=%s] swap container with con_id %s", p.Window, dest.Nodes[k].ID))
			}
		}
		anchor = maximizeAfterMark
	}
	// Moved onto a window, a window goes right after it: going through them
	// backwards keeps their order.
	for i := len(extras) - 1; i >= 0; i-- {
		cmds = append(cmds, fmt.Sprintf("[con_id=%s] move container to mark %s", extras[i].ID, anchor))
	}
	cmds = append(cmds,
		fmt.Sprintf("unmark %s", maximizeDestMark),
		fmt.Sprintf("unmark %s", maximizeAfterMark),
		fmt.Sprintf("[con_id=%s] move container to workspace %s", parked.ID, swayQuote(wsName)))
	for _, id := range floating {
		cmds = append(cmds, fmt.Sprintf("[con_id=%s] move container to workspace %s", id, swayQuote(wsName)))
	}
	if refocus != "" {
		cmds = append(cmds, refocus)
	}
	// All in one go, so that the workspace is never seen empty.
	if err = ctx.Command(strings.Join(cmds, "; ")); err != nil {
		return err
	}

	if home && index >= 0 {
		if err = h.rebuildWrappers(ctx, p); err != nil {
			return err
		}
		if err = h.restoreProportions(ctx, p); err != nil {
			return err
		}
	}
	ctx.Log.Info("restored", "con_id", p.Window, "workspace", wsName)

	if tree, err = ctx.Tree(); err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	window, _ = findNodeByID(tree, p.Window)
	return h.unmark(ctx, window)
}

// rebuildWrappers wraps the window again in the containers that held nothing
// else and went away with it, innermost first.
func (h *maximizeHandler) rebuildWrappers(ctx *WindowContext, p *maximizePlan) error {
	node := p.Window
	for _, layout := range p.Wrappers {
		tree, err := ctx.Tree()
		if err != nil {
			return fmt.Errorf("get_tree: %w", err)
		}
		// With no siblings, split would change the layout of the parent
		// instead of wrapping: that only happens if every other window of
		// the container has gone, and then there is nothing to rebuild.
		if _, parent := findNodeByID(tree, node); parent == nil || len(parent.Nodes) < 2 {
			return nil
		}
		cmd := fmt.Sprintf("[con_id=%s] splith", node)
		switch layout {
		case "splitv":
			cmd = fmt.Sprintf("[con_id=%s] splitv", node)
		case "tabbed", "stacked":
			cmd += fmt.Sprintf("; [con_id=%s] layout %s", node, layoutWord(layout))
		}
		if err = ctx.Command(cmd); err != nil {
			return err
		}
		if tree, err = ctx.Tree(); err != nil {
			return fmt.Errorf("get_tree: %w", err)
		}
		_, wrapper := findNodeByID(tree, node)
		if wrapper == nil {
			return nil
		}
		node = wrapper.ID
	}
	return nil
}

// restoreProportions gives the window's container back the proportions it had.
// Putting the window in again gives it an even share, taken from the others.
func (h *maximizeHandler) restoreProportions(ctx *WindowContext, p *maximizePlan) error {
	if p.Layout != "splith" && p.Layout != "splitv" {
		return nil // tabbed or stacked: every child has all of it
	}
	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	var container *Node
	if p.Container == "" {
		container, _ = findNodeByID(tree, p.Workspace)
	} else {
		container, _ = findNodeByID(tree, p.Container)
	}
	// Only when the container holds what it held, in the same order: with
	// windows closed or opened meanwhile the old proportions mean nothing.
	if container == nil || container.Layout != p.Layout || len(container.Nodes) != len(p.Children) {
		return nil
	}
	for i := range container.Nodes {
		if i != p.Index && container.Nodes[i].ID != p.Children[i] {
			return nil
		}
	}
	cmds := resizeCommands(container, p.Percents, p.AxisSize)
	if len(cmds) == 0 {
		return nil
	}
	return ctx.Command(strings.Join(cmds, "; "))
}

// resizeCommands returns the commands that give the children of a split
// container the shares of it in want, size being its length in pixels along
// the split. Nothing for a tabbed or stacked container, where every child has
// all of it.
//
// `resize set` would not do: it takes the difference from both neighbours, so
// fixing one child unsettles the one fixed before it. Growing a child towards
// its right (or bottom) edge only moves the border it shares with the next
// one, so going through them in order leaves each one right, and the last one
// gets what is left.
func resizeCommands(container *Node, want []float64, size int) []string {
	edge := map[string]string{"splith": "right", "splitv": "down"}[container.Layout]
	if edge == "" || size <= 0 || len(want) != len(container.Nodes) {
		return nil
	}
	current := make([]float64, len(container.Nodes))
	for i := range container.Nodes {
		if pct := container.Nodes[i].Percent; pct != nil {
			current[i] = *pct
		}
	}
	var cmds []string
	for i := 0; i < len(current)-1; i++ {
		delta := want[i] - current[i]
		px := int(math.Round(delta * float64(size)))
		if px == 0 {
			continue
		}
		verb := "grow"
		if px < 0 {
			verb, px = "shrink", -px
		}
		cmds = append(cmds, fmt.Sprintf("[con_id=%s] resize %s %s %d px", container.Nodes[i].ID, verb, edge, px))
		current[i+1] -= delta
	}
	return cmds
}

// unmark takes the mark off the window, when it still has it: once the window
// is back in the layout, a mark left on it would maximize it again at the next
// mark event.
func (h *maximizeHandler) unmark(ctx *WindowContext, window *Node) error {
	if window == nil {
		return nil
	}
	for _, m := range window.Marks {
		if m == maximizeMark {
			return ctx.Command(fmt.Sprintf("[con_id=%s] unmark %s", window.ID, maximizeMark))
		}
	}
	return nil
}

// save writes the plan where the next run of the daemon will look for it.
// Failing to is not worth stopping for: it only matters if the daemon dies
// before the window is restored.
func (h *maximizeHandler) save(ctx *WindowContext) {
	if h.stateFile == "" {
		return
	}
	data, err := json.Marshal(h.plan)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(h.stateFile), 0o700)
	}
	if err == nil {
		err = os.WriteFile(h.stateFile, data, 0o600)
	}
	if err != nil {
		ctx.Log.Warn("could not save the maximize state", "file", h.stateFile, "error", err)
	}
}

// forget removes the saved plan.
func (h *maximizeHandler) forget(ctx *WindowContext) {
	if h.stateFile == "" {
		return
	}
	if err := os.Remove(h.stateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		ctx.Log.Warn("could not remove the maximize state", "file", h.stateFile, "error", err)
	}
}

// wrapCommand gathers every tiled child of the workspace in a new container
// with the workspace's own layout, so that nothing changes on screen.
//
// It relies on the quirk layoutCommand works around: addressed to a child of a
// workspace, `layout` wraps all of the workspace's children in a new container
// rather than changing the workspace. That only happens when the layout asked
// for is not the one the workspace already has, hence the detour.
func wrapCommand(ws *Node) string {
	other := "splitv"
	if ws.Layout == "splitv" {
		other = "splith"
	}
	child := ws.Nodes[0].ID
	return fmt.Sprintf("[con_id=%s] layout %s; [con_id=%s] layout %s", child, other, child, layoutWord(ws.Layout))
}

// tiledWindows returns the tiled windows under a node, floating ones left out.
func tiledWindows(n *Node) []*Node {
	if len(n.Nodes) == 0 {
		if n.NodeType == "con" {
			return []*Node{n}
		}
		return nil
	}
	var windows []*Node
	for i := range n.Nodes {
		windows = append(windows, tiledWindows(&n.Nodes[i])...)
	}
	return windows
}

// findWorkspaceByName returns the workspace with the given name, if any.
func findWorkspaceByName(root *Node, name string) *Node {
	var found *Node
	traverseDeepFirst(func(n *Node, _ *Node, _ int) {
		if n.NodeType == "workspace" && n.Name != nil && *n.Name == name {
			found = n
		}
	}, root, nil, 0)
	return found
}

// swayQuote quotes a workspace name as an argument of a Sway command.
//
// Nothing is escaped: `workspace`, `rename` and `move to workspace` take what
// is between the quotes as it is, backslashes included. A double quote cannot
// be written at all (Sway drops it), so no workspace name ever holds one.
func swayQuote(s string) string {
	return `"` + s + `"`
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
