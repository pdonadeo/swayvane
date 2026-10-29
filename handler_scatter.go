package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// scatterCommand is the argument of the `nop` binding that toggles the
// scattered exposé:
//
//	bindsym $mod+F4 nop ws-rename-scatter
const scatterCommand = "ws-rename-scatter"

// scatterUniformCommand does the same, with every window shrunk by the same
// factor instead of each one fitted to a cell of its own (see scatterUniform).
const scatterUniformCommand = "ws-rename-scatter-uniform"

// scatterLayouts maps each scatter binding to the way it deals the windows out.
var scatterLayouts = map[string]func(area Rect, sizes []Rect) []Rect{
	scatterCommand:        scatterGrid,
	scatterUniformCommand: scatterUniform,
}

// scatterBinding returns the layout of the scatter binding a command is, or
// nil when it is none of them.
func scatterBinding(command string) func(area Rect, sizes []Rect) []Rect {
	for arg, layout := range scatterLayouts {
		if isNopBinding(command, arg) {
			return layout
		}
	}
	return nil
}

// scatterAnchor is the mark `move container to mark` aims at while the windows
// are put back, moved from window to window as they go in.
const scatterAnchor = "_wsr_scatter"

// scatterArmed is the mark set at the end of the scatter commands: see
// scatterHandler.armed.
const scatterArmed = "_wsr_scatter_armed"

// scatterClick is the binding that takes over the left button while the
// windows are scattered. Picking a window must not click into it as well: the
// focus changes as soon as the button goes down, so the windows would be put
// back with the button still down, and whatever moves under it then — the
// window itself, the pointer, autotiling — would reach the window as a drag.
// A mouse binding keeps the click from the window, and runs scatterClickCommand
// on the window under the pointer. Sway forgets it on reload, which is as well.
const scatterClick = "--whole-window button1"

// scatterPick ends every binding that picks a window while the windows are
// scattered: the binding event its `nop` brings tells the scatter to end even
// when the window picked had the focus already, and so no focus event comes.
const scatterPick = "ws-rename-scatter-pick"

// scatterClickCommand is what the click runs: `focus` picks the window under
// the pointer.
const scatterClickCommand = "focus; nop " + scatterPick

// scatterKeys is how many windows get a number in their title bar, and the
// key of that number to pick them with: 1 to 9.
const scatterKeys = 9

// scatterFocusBorder is the least width of the border of the window with the
// focus while the windows are scattered, so that it stands out.
const scatterFocusBorder = 4

// scatterGap is the room left between the scattered windows and around them,
// in pixels.
const scatterGap = 30

// scatterHandler takes every window of the focused workspace out of the
// layout, makes it floating and deals it out on a grid, like the exposé of
// macOS without the thumbnails: Sway cannot shrink what a window shows, so
// each one really gets smaller and redraws itself to fit. Clicking a window
// picks it: everything goes back in place, with the focus on it.
//
// Taking the windows out is the easy part. Putting them back is not, because
// once they are floating the containers they lived in are gone, and Sway has
// no command to build a layout from a description. So the plan records the
// tree, and the way back builds it again from the top: the first window of
// each container goes in where the container was, `split` wraps it in a new
// container, and the other children follow it in with `move container to
// mark`, which puts a window right after the one carrying the mark. Every
// command names a window, never a container, so the whole sequence is worked
// out from the plan before the first command is sent.
//
// Any focus change ends it, which is what makes a click a pick; for the same
// reason the focus no longer follows the mouse while it lasts, or crossing a
// window on the way to another would pick it. The keys 1 to 9 pick a window
// too: each window shows its number in a title bar of its own while scattered.
type scatterHandler struct {
	// plan describes the workspace as it was, or is nil when nothing is
	// scattered.
	plan *scatterPlan
	// armed is set once the focus events caused by this daemon have all gone
	// by, and a focus event can only come from the user. Commands sent with
	// criteria do not move the focus, but the ones putting a workspace back
	// do, and they may come just before a scatter, from this handler or
	// another one: their events are still on their way when it starts. Sway
	// reports events in the order they happen, so the scatter commands end by
	// setting a mark, and the focus counts from the event of that mark on.
	armed bool
	// stateFile keeps the plan across a restart of the daemon, so that the
	// windows go back even then. Empty when there is nowhere to keep it.
	stateFile string
	// paused, when set and true, makes the handler ignore its binding: see
	// exposeHandler.
	paused func() bool
}

// scatterPlan is what it takes to put the workspace back.
type scatterPlan struct {
	// Socket names the Sway instance the plan belongs to: ids start again
	// from scratch in a new one, so the plan would point at other windows.
	Socket        string `json:"socket"`
	Workspace     NodeID `json:"workspace"`
	WorkspaceName string `json:"workspace_name"`
	// Tree is the tiled part of the workspace, the workspace itself at its
	// root; nil when every window was floating already.
	Tree *scatterNode `json:"tree,omitempty"`
	// Floating lists the windows that were floating already, and where.
	Floating []scatterFloat `json:"floating,omitempty"`
	// Decor lists every window dealt out, in the order of their numbers, with
	// the border it had.
	Decor []scatterDecor `json:"decor,omitempty"`
	// FocusedColors holds the colors of client.focused, when the scatter
	// changed them.
	FocusedColors string `json:"focused_colors,omitempty"`
}

// scatterNode is a window, when it has no children, or a container.
type scatterNode struct {
	ID       NodeID         `json:"id"`
	Layout   string         `json:"layout,omitempty"`
	Percent  float64        `json:"percent,omitempty"`
	Children []*scatterNode `json:"children,omitempty"`
	// Shown is the window a container showed, for a tabbed or stacked one:
	// the one reached by following the focus down from it.
	Shown NodeID `json:"shown,omitempty"`

	// changed is set when some of the children have gone meanwhile, and the
	// proportions recorded for them no longer mean anything.
	changed bool
}

type scatterFloat struct {
	ID   NodeID `json:"id"`
	Rect Rect   `json:"rect"`
}

type scatterDecor struct {
	ID     NodeID `json:"id"`
	Border string `json:"border"`
	Width  int    `json:"width"`
}

func newScatterHandler() *scatterHandler {
	h := &scatterHandler{}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		h.stateFile = filepath.Join(dir, "ws-rename", "scatter.json")
	}
	return h
}

func (h *scatterHandler) Name() string { return "scatter" }

// active tells the other layout handlers to keep their hands off.
func (h *scatterHandler) active() bool { return h.plan != nil }

// HandleConnect puts back a workspace left scattered by a previous run of the
// daemon.
func (h *scatterHandler) HandleConnect(ctx *WindowContext) error {
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
	var plan scatterPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		h.forget(ctx)
		return fmt.Errorf("parse %s: %w", h.stateFile, err)
	}
	if plan.Socket != ctx.Socket() {
		ctx.Log.Info("dropping a scatter left by another Sway instance", "workspace", plan.WorkspaceName)
		h.forget(ctx)
		return nil
	}
	ctx.Log.Info("found a scattered workspace from a previous run, restoring it", "workspace", plan.WorkspaceName)
	h.plan = &plan
	return h.restoreLayout(ctx)
}

// HandleWindow ends the scatter on the first focus change: a click on one of
// the windows, or the focus moved anywhere else.
func (h *scatterHandler) HandleWindow(ctx *WindowContext) error {
	if h.plan == nil {
		return nil
	}
	switch ctx.Info.Change {
	case WinMark:
		if !h.armed && slices.Contains(ctx.Info.Container.Marks, scatterArmed) {
			h.armed = true
			return ctx.Command("unmark " + scatterArmed)
		}
	case WinFocus:
		if h.armed {
			return h.restoreLayout(ctx)
		}
	}
	return nil
}

func (h *scatterHandler) HandleBinding(ctx *WindowContext, command string) error {
	if isScatterPick(command) {
		if !h.active() {
			return nil // already ended by the focus event of the same click
		}
		return h.restoreLayout(ctx)
	}
	if isNopBinding(command, exposeCommand) && h.active() {
		return h.restoreLayout(ctx) // the other exposé ends this one
	}
	layout := scatterBinding(command)
	if layout == nil || (h.paused != nil && h.paused()) {
		return nil
	}
	if ctx.Sent() {
		// The key has just ended the exposé or a maximize, and the windows
		// are still taking the sizes that gave them back: Sway fits a
		// floating window to the size it reports, so they would end up the
		// size they had a moment ago rather than the size given to them
		// here. The next press scatters.
		return nil
	}
	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	ws := focusedWorkspace(tree)

	if h.active() {
		// Pressed again: on the scattered workspace that means "done",
		// anywhere else it means "this one instead".
		here := ws != nil && ws.ID == h.plan.Workspace
		if err = h.restoreLayout(ctx); err != nil {
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
	return h.scatter(ctx, ws, layout)
}

// scatter records the workspace and deals its windows out with layout.
func (h *scatterHandler) scatter(ctx *WindowContext, ws *Node, layout func(area Rect, sizes []Rect) []Rect) error {
	fullscreen := false
	traverseDeepFirst(func(n *Node, _ *Node, _ int) {
		// The workspace reports itself as fullscreen: only its content counts.
		if n.NodeType != "workspace" && n.FullscreenMode != 0 {
			fullscreen = true
		}
	}, ws, nil, 0)
	if fullscreen {
		ctx.Log.Info("a window is fullscreen, not scattering", "workspace", workspaceName(ws))
		return nil
	}

	tiled := tiledWindows(ws)
	units := append([]*Node(nil), tiled...)
	for i := range ws.FloatingNodes {
		units = append(units, &ws.FloatingNodes[i])
	}
	if len(units) < 2 {
		ctx.Log.Info("nothing to scatter", "workspace", workspaceName(ws))
		return nil
	}

	plan := &scatterPlan{Socket: ctx.Socket(), Workspace: ws.ID, WorkspaceName: workspaceName(ws)}
	if len(ws.Nodes) > 0 {
		plan.Tree = snapshotNode(ws)
	}
	for i := range ws.FloatingNodes {
		// The rect leaves the title bar out, while resizing and moving take
		// it in.
		n := &ws.FloatingNodes[i]
		r := n.Rect
		r.Y -= n.DecoRect.Height
		r.Height += n.DecoRect.Height
		plan.Floating = append(plan.Floating, scatterFloat{ID: n.ID, Rect: r})
	}

	// Dealt out in reading order of where they are now, so that every window
	// lands roughly where the eye expects it. The windows behind a tab share
	// the place of their container and keep the order of the tabs.
	sort.SliceStable(units, func(i, j int) bool {
		a, b := units[i].Rect, units[j].Rect
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		return a.X < b.X
	})
	sizes := make([]Rect, len(units))
	for i, u := range units {
		sizes[i] = u.Rect
	}
	places := layout(ws.Rect, sizes)

	config, err := ctx.Config()
	if err != nil {
		ctx.Log.Warn("could not read the configuration, the windows keep their colors", "error", err)
	}
	theme := readScatterTheme(config)

	cmds := []string{"focus_follows_mouse no", "bindsym " + scatterClick + ` "` + scatterClickCommand + `"`}
	if highlight := theme.highlight(); highlight != "" {
		plan.FocusedColors = strings.Join(theme.focused, " ")
		cmds = append(cmds, "client.focused "+highlight)
	}
	for _, n := range tiled {
		cmds = append(cmds, fmt.Sprintf("[con_id=%s] floating enable", n.ID))
	}
	// The title bar goes on before the windows are sized, as it is part of
	// the size given.
	for i, u := range units {
		plan.Decor = append(plan.Decor, scatterDecor{ID: u.ID, Border: u.Border, Width: u.CurrBorderW})
		width := u.CurrBorderW
		if u.Focused {
			width = max(width, scatterFocusBorder)
		}
		cmds = append(cmds, fmt.Sprintf("[con_id=%s] border normal %d", u.ID, width))
		if i < scatterKeys {
			cmds = append(cmds,
				fmt.Sprintf("[con_id=%s] title_format %s", u.ID, swayQuote(theme.label(i+1, u.Focused))),
				fmt.Sprintf(`bindsym %d "[con_id=%s] focus; nop %s"`, i+1, u.ID, scatterPick))
		}
	}
	for i, u := range units {
		p := places[i]
		cmds = append(cmds,
			fmt.Sprintf("[con_id=%s] resize set %d px %d px", u.ID, p.Width, p.Height),
			fmt.Sprintf("[con_id=%s] move absolute position %d px %d px", u.ID, p.X, p.Y))
	}
	cmds = append(cmds, fmt.Sprintf("[con_id=%s] mark --add %s", units[0].ID, scatterArmed))

	h.plan, h.armed = plan, false
	h.save(ctx)
	if err := ctx.Command(strings.Join(cmds, "; ")); err != nil {
		return err
	}
	ctx.Log.Info("scattered", "workspace", plan.WorkspaceName, "windows", len(units))
	return nil
}

// isScatterPick tells whether a binding command is one of those picking a
// window, the click or a number.
func isScatterPick(command string) bool {
	return strings.HasSuffix(command, "nop "+scatterPick)
}

// scatterTheme is what the scatter borrows from the configuration to mark the
// windows.
type scatterTheme struct {
	// focused holds the colors of client.focused as written there, variables
	// resolved, or is nil when they are not found.
	focused []string
	// markup tells whether titles take Pango markup, which a `pango:` font
	// turns on.
	markup bool
}

// readScatterTheme picks the colors of client.focused and the font out of the
// text of the configuration. Only the main file is there, not the ones it
// includes: what is set in those is not found, and the windows then keep the
// colors they have and get a plain number.
func readScatterTheme(config string) scatterTheme {
	var t scatterTheme
	vars := map[string]string{}
	for line := range strings.Lines(config) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "set":
			if len(f) >= 3 && strings.HasPrefix(f[1], "$") {
				vars[f[1]] = f[2]
			}
		case "font":
			t.markup = len(f) >= 2 && strings.HasPrefix(f[1], "pango:")
		case "client.focused":
			if len(f) < 4 {
				continue
			}
			t.focused = nil
			for _, c := range f[1:] {
				if v, ok := vars[c]; ok {
					c = v
				}
				t.focused = append(t.focused, c)
			}
		}
	}
	return t
}

// highlight returns the colors of client.focused with the border around the
// window in the color of the title bar's own, or an empty string when they
// are the same already or unknown.
func (t scatterTheme) highlight() string {
	if len(t.focused) < 3 {
		return ""
	}
	c := append([]string(nil), t.focused...)
	for len(c) < 5 {
		c = append(c, c[0]) // the indicator and the border around the window
	}
	if c[4] == c[0] {
		return ""
	}
	c[4] = c[0]
	return strings.Join(c[:5], " ")
}

// label is the title format giving a window its number: in the colors of the
// title bar with the focus, the other way round on that title bar itself.
func (t scatterTheme) label(n int, focused bool) string {
	if !t.markup || len(t.focused) < 3 {
		return fmt.Sprintf("[%d] %%title", n)
	}
	bg, fg := t.focused[1], t.focused[2]
	if focused {
		bg, fg = fg, bg
	}
	return fmt.Sprintf("<span background='%s' foreground='%s'> %d </span> %%title", bg, fg, n)
}

// undecorate takes the keys back, and gives client.focused its colors and the
// windows still around their borders and titles.
func (p *scatterPlan) undecorate(tree *Node) []string {
	var cmds []string
	for i := range min(len(p.Decor), scatterKeys) {
		cmds = append(cmds, fmt.Sprintf("unbindsym %d", i+1))
	}
	if p.FocusedColors != "" {
		cmds = append(cmds, "client.focused "+p.FocusedColors)
	}
	for i, d := range p.Decor {
		if n, _ := findNodeByID(tree, d.ID); n == nil {
			continue
		}
		border := d.Border
		if border == "normal" || border == "pixel" {
			border += " " + strconv.Itoa(d.Width)
		}
		cmds = append(cmds, fmt.Sprintf("[con_id=%s] border %s", d.ID, border))
		if i < scatterKeys {
			cmds = append(cmds, fmt.Sprintf("[con_id=%s] title_format %%title", d.ID))
		}
	}
	return cmds
}

// snapshotNode records a node and everything under it.
func snapshotNode(n *Node) *scatterNode {
	s := &scatterNode{ID: n.ID, Layout: n.Layout}
	if n.Percent != nil {
		s.Percent = *n.Percent
	}
	for i := range n.Nodes {
		s.Children = append(s.Children, snapshotNode(&n.Nodes[i]))
	}
	if n.Layout == "tabbed" || n.Layout == "stacked" {
		s.Shown = shownWindow(n)
	}
	return s
}

// shownWindow follows the focus down from a container to a window: the one a
// tabbed container shows, and the one it shows again once focused.
func shownWindow(n *Node) NodeID {
	for len(n.Nodes) > 0 {
		next := &n.Nodes[0]
	search:
		for _, id := range n.Focus {
			for i := range n.Nodes {
				if n.Nodes[i].ID == id {
					next = &n.Nodes[i]
					break search
				}
			}
		}
		n = next
	}
	return n.ID
}

// scatterGrid deals boxes of the given sizes out on a grid filling area, in
// order, row by row, the last row centred. The number of columns is the one
// giving the cells the most room, measured against the area itself so that
// cells shaped like the screen win; each box keeps its proportions and is
// shrunk to fit its cell, never enlarged.
func scatterGrid(area Rect, sizes []Rect) []Rect {
	n := len(sizes)
	cell := func(total, count int) int {
		return max((total-(count+1)*scatterGap)/count, 50)
	}
	cols, best := 1, -1.0
	for c := 1; c <= n; c++ {
		r := (n + c - 1) / c
		s := math.Min(float64(cell(area.Width, c))/float64(area.Width),
			float64(cell(area.Height, r))/float64(area.Height))
		if s > best {
			cols, best = c, s
		}
	}
	rows := (n + cols - 1) / cols
	cw, ch := cell(area.Width, cols), cell(area.Height, rows)

	places := make([]Rect, n)
	for i, size := range sizes {
		row, col := i/cols, i%cols
		inRow := cols
		if row == rows-1 {
			inRow = n - row*cols
		}
		x := area.X + scatterGap + col*(cw+scatterGap) + (cols-inRow)*(cw+scatterGap)/2
		y := area.Y + scatterGap + row*(ch+scatterGap)

		w, h := cw, ch
		if size.Width > 0 && size.Height > 0 {
			s := math.Min(1, math.Min(float64(cw)/float64(size.Width), float64(ch)/float64(size.Height)))
			w, h = int(float64(size.Width)*s), int(float64(size.Height)*s)
		}
		places[i] = Rect{X: x + (cw-w)/2, Y: y + (ch-h)/2, Width: w, Height: h}
	}
	return places
}

// scatterUniform deals boxes of the given sizes out the way macOS does: all
// shrunk by the same factor, so that they keep their sizes relative to each
// other, and laid in rows in order, each row and the whole of them centred.
// The factor is the largest one, never above 1, for which the rows fit area.
func scatterUniform(area Rect, sizes []Rect) []Rect {
	type row struct{ first, end, width, height int }
	width, height := area.Width-2*scatterGap, area.Height-2*scatterGap
	scaled := func(size Rect, s float64) (int, int) {
		return max(int(float64(size.Width)*s), 1), max(int(float64(size.Height)*s), 1)
	}
	// rows fills rows left to right at factor s and tells whether they fit.
	rows := func(s float64) ([]row, bool) {
		var rs []row
		for i, size := range sizes {
			w, h := scaled(size, s)
			if len(rs) == 0 || rs[len(rs)-1].width+scatterGap+w > width {
				rs = append(rs, row{first: i, width: -scatterGap})
			}
			r := &rs[len(rs)-1]
			r.end, r.width, r.height = i+1, r.width+scatterGap+w, max(r.height, h)
		}
		total := -scatterGap
		for _, r := range rs {
			total += scatterGap + r.height
			if r.width > width {
				return rs, false
			}
		}
		return rs, total <= height
	}

	s := 1.0
	if _, fit := rows(s); !fit {
		lo, hi := 0.0, 1.0
		for range 30 {
			mid := (lo + hi) / 2
			if _, fit := rows(mid); fit {
				lo = mid
			} else {
				hi = mid
			}
		}
		s = lo
	}
	rs, _ := rows(s)

	total := -scatterGap
	for _, r := range rs {
		total += scatterGap + r.height
	}
	places := make([]Rect, len(sizes))
	y := area.Y + scatterGap + (height-total)/2
	for _, r := range rs {
		x := area.X + scatterGap + (width-r.width)/2
		for i := r.first; i < r.end; i++ {
			w, h := scaled(sizes[i], s)
			places[i] = Rect{X: x, Y: y + (r.height-h)/2, Width: w, Height: h}
			x += w + scatterGap
		}
		y += r.height + scatterGap
	}
	return places
}

// restoreLayout puts the workspace back as the plan describes it.
//
// Windows closed or moved to another workspace meanwhile are left out, and so
// are the containers left empty by them. Windows opened meanwhile go in last,
// wherever Sway puts a window that stops floating. The focus stays where it
// is, which after a click is on the window clicked.
func (h *scatterHandler) restoreLayout(ctx *WindowContext) error {
	p := h.plan
	h.plan, h.armed = nil, false
	h.forget(ctx)

	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	refocus := refocusCommand(tree)
	// The borders go back before the windows are sized: the sizes recorded
	// for the floating ones take them in.
	cmds := append([]string{"unbindsym " + scatterClick, "unmark " + scatterArmed}, p.undecorate(tree)...)

	ws, _ := findNodeByID(tree, p.Workspace)
	if ws == nil || ws.NodeType != "workspace" {
		ctx.Log.Warn("the scattered workspace is gone", "workspace", p.WorkspaceName)
		return ctx.Command(strings.Join(append(cmds, "focus_follows_mouse yes"), "; "))
	}

	present := map[NodeID]bool{}
	tiled := tiledWindows(ws)
	for _, n := range tiled {
		present[n.ID] = true
	}
	for i := range ws.FloatingNodes {
		present[ws.FloatingNodes[i].ID] = true
	}
	if p.Tree != nil && !p.Tree.prune(present) {
		p.Tree = nil
	}
	inPlan := map[NodeID]bool{}
	var leaves []NodeID
	if p.Tree != nil {
		leaves = p.Tree.leaves()
		for _, id := range leaves {
			inPlan[id] = true
		}
	}

	// The tiled part is built again from nothing, so whatever is tiled now
	// leaves first: windows put back by hand, and windows opened meanwhile.
	var extras []NodeID
	for _, n := range tiled {
		cmds = append(cmds, fmt.Sprintf("[con_id=%s] floating enable", n.ID))
		if !inPlan[n.ID] {
			extras = append(extras, n.ID)
		}
	}
	var floating []scatterFloat
	for _, f := range p.Floating {
		if present[f.ID] {
			floating = append(floating, f)
		}
	}
	if p.Tree != nil {
		b := &scatterBuild{placed: map[NodeID]bool{}, leaves: leaves}
		for _, f := range floating {
			b.spare = append(b.spare, f.ID)
		}
		b.workspace(p.Tree)
		cmds = append(cmds, b.cmds...)
	}
	// Floating ones again after the tiled ones, which may have borrowed them.
	for _, f := range floating {
		cmds = append(cmds,
			fmt.Sprintf("[con_id=%s] floating enable", f.ID),
			fmt.Sprintf("[con_id=%s] resize set %d px %d px", f.ID, f.Rect.Width, f.Rect.Height),
			fmt.Sprintf("[con_id=%s] move absolute position %d px %d px", f.ID, f.Rect.X, f.Rect.Y))
	}
	if err = ctx.Command(strings.Join(cmds, "; ")); err != nil {
		return err
	}

	if p.Tree != nil {
		if err = h.restoreProportions(ctx, p.Tree); err != nil {
			return err
		}
	}

	// A tabbed container shows the child that last had the focus, so each
	// one gets the focus on the window it showed, the innermost first: the
	// outer ones then reach down through the inner ones already settled.
	// The focus is put back last, which also shows the window it is on.
	cmds = nil
	for _, id := range extras {
		cmds = append(cmds, fmt.Sprintf("[con_id=%s] floating disable", id))
	}
	if p.Tree != nil {
		p.Tree.postOrder(func(s *scatterNode) {
			if s.Shown != "" && present[s.Shown] {
				cmds = append(cmds, fmt.Sprintf("[con_id=%s] focus", s.Shown))
			}
		})
	}
	if refocus != "" {
		cmds = append(cmds, refocus)
	}
	if len(cmds) > 0 {
		if err = ctx.Command(strings.Join(cmds, "; ")); err != nil {
			return err
		}
	}

	// After a click the pointer is where the window clicked was while
	// scattered, most likely over another window now, which would take the
	// focus as soon as the mouse moved once it follows the mouse again. So
	// the pointer goes to the middle of the window with the focus first.
	if tree, err = ctx.Tree(); err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	cmds = nil
	if w, _ := findFocusedLeaf(tree); w != nil && w.NodeType != "workspace" {
		r := w.Rect
		cmds = append(cmds, fmt.Sprintf("seat - cursor set %d %d", r.X+r.Width/2, r.Y+r.Height/2))
	}
	cmds = append(cmds, "focus_follows_mouse yes")
	if err = ctx.Command(strings.Join(cmds, "; ")); err != nil {
		return err
	}
	ctx.Log.Info("scatter over", "workspace", p.WorkspaceName, "windows", len(leaves)+len(floating))
	return nil
}

// restoreProportions gives every split container its proportions back,
// outermost first: resizing a container resizes everything in it, so the
// containers one level down are only worked out once their parents are done.
func (h *scatterHandler) restoreProportions(ctx *WindowContext, root *scatterNode) error {
	for level := []*scatterNode{root}; len(level) > 0; {
		tree, err := ctx.Tree()
		if err != nil {
			return fmt.Errorf("get_tree: %w", err)
		}
		var next []*scatterNode
		var cmds []string
		for _, s := range level {
			for _, c := range s.Children {
				if len(c.Children) > 0 {
					next = append(next, c)
				}
			}
			if s.changed || len(s.Children) < 2 {
				continue
			}
			container := s.built(tree)
			if container == nil {
				continue
			}
			want := make([]float64, len(s.Children))
			for i, c := range s.Children {
				want[i] = c.Percent
			}
			size := container.Rect.Width
			if container.Layout == "splitv" {
				size = container.Rect.Height
			}
			cmds = append(cmds, resizeCommands(container, want, size)...)
		}
		if len(cmds) > 0 {
			if err = ctx.Command(strings.Join(cmds, "; ")); err != nil {
				return err
			}
		}
		level = next
	}
	return nil
}

// built finds the container built for s, by the windows it should hold: the
// deepest node holding both the first window of its first child and that of
// its second. It must have as many children as s, each holding the first
// window of the child of s in the same place, and the same layout; anything
// else means the layout did not come out as planned, and nil is returned.
func (s *scatterNode) built(tree *Node) *Node {
	a := ancestorPath(tree, s.Children[0].firstLeaf())
	b := ancestorPath(tree, s.Children[1].firstLeaf())
	var container *Node
	for i := 0; i < len(a) && i < len(b) && a[i].ID == b[i].ID; i++ {
		container = a[i]
	}
	if container == nil || container.Layout != s.Layout || len(container.Nodes) != len(s.Children) {
		return nil
	}
	for i, c := range s.Children {
		if n, _ := findNodeByID(&container.Nodes[i], c.firstLeaf()); n == nil {
			return nil
		}
	}
	return container
}

// prune drops the windows that are not in present, and the containers left
// with nothing in them. It reports whether anything is left of s.
func (s *scatterNode) prune(present map[NodeID]bool) bool {
	if len(s.Children) == 0 {
		return present[s.ID]
	}
	kept := s.Children[:0]
	for _, c := range s.Children {
		if c.prune(present) {
			kept = append(kept, c)
		} else {
			s.changed = true
		}
	}
	s.Children = kept
	return len(kept) > 0
}

func (s *scatterNode) firstLeaf() NodeID {
	for len(s.Children) > 0 {
		s = s.Children[0]
	}
	return s.ID
}

func (s *scatterNode) leaves() []NodeID {
	if len(s.Children) == 0 {
		return []NodeID{s.ID}
	}
	var ids []NodeID
	for _, c := range s.Children {
		ids = append(ids, c.leaves()...)
	}
	return ids
}

func (s *scatterNode) postOrder(f func(*scatterNode)) {
	for _, c := range s.Children {
		c.postOrder(f)
	}
	f(s)
}

// scatterBuild works out the commands that build the tiled part of the
// workspace again, from floating windows.
type scatterBuild struct {
	cmds []string
	// leaves lists every window of the plan, placed records the ones already
	// in their place.
	leaves []NodeID
	placed map[NodeID]bool
	// spare lists floating windows that may be borrowed for a moment: see
	// workspace.
	spare []NodeID
}

func (b *scatterBuild) add(format string, args ...any) {
	b.cmds = append(b.cmds, fmt.Sprintf(format, args...))
}

// workspace puts the first window back on the workspace, gives the workspace
// its layout, and builds the rest from there.
//
// A container alone on the workspace needs a companion for its first window
// first: with no siblings, `split` would change the layout of the workspace
// instead of wrapping the window. Any window still to be placed will do, as it
// will be moved away when its turn comes, or else one of the floating
// windows, which will float again. There is always one of the two, or there
// would have been a single window to scatter.
func (b *scatterBuild) workspace(root *scatterNode) {
	first := root.firstLeaf()
	b.add("[con_id=%s] floating disable", first)
	b.placed[first] = true
	b.cmds = append(b.cmds, layoutCommand(true, first, layoutWord(root.Layout), ""))
	if c := root.Children[0]; len(root.Children) == 1 && len(c.Children) > 0 {
		if companion := b.companion(first); companion != "" {
			b.moveAfter(companion, first)
			b.container(c)
		} else {
			b.children(c) // the container is left out, its children go in its place
		}
	} else {
		b.children(root)
	}
	b.add("unmark %s", scatterAnchor)
}

// children fills x, whose first window is already in place: the first
// windows of the other children go in after it, in order, and then each child
// that is a container is built around its own first window.
func (b *scatterBuild) children(x *scatterNode) {
	for i := 1; i < len(x.Children); i++ {
		w := x.Children[i].firstLeaf()
		b.moveAfter(w, x.Children[i-1].firstLeaf())
		b.placed[w] = true
	}
	for _, c := range x.Children {
		if len(c.Children) > 0 {
			b.container(c)
		}
	}
}

// container wraps the first window of c, already where c goes and with
// siblings around it, in a new container with the layout of c, and fills it.
//
// A container holding nothing but another container — a chain, which
// autotiling leaves behind when windows go — cannot be built from the top:
// once wrapped, the window has no siblings, and `split` on it would change
// the layout of the container instead of wrapping it again. So a chain is
// built from the bottom: the window is wrapped in the innermost container,
// which still has siblings, and each container is then wrapped in the one
// above it. Containers have no criteria to address them by, so the focus
// climbs to them from the window; `layout` lands on the parent of the node it
// is given, which is why the focus stops one level short for it.
func (b *scatterBuild) container(c *scatterNode) {
	chain := []*scatterNode{c}
	for last := c; len(last.Children) == 1 && len(last.Children[0].Children) > 0; {
		last = last.Children[0]
		chain = append(chain, last)
	}
	first := c.firstLeaf()
	inner := chain[len(chain)-1]
	b.add("[con_id=%s] %s", first, splitWord(inner.Layout))
	if inner.Layout == "tabbed" || inner.Layout == "stacked" {
		b.add("[con_id=%s] layout %s", first, layoutWord(inner.Layout))
	}
	for i := len(chain) - 2; i >= 0; i-- {
		climb := strings.Repeat("; focus parent", len(chain)-1-i)
		b.add("[con_id=%s] focus%s; %s", first, climb, splitWord(chain[i].Layout))
		if chain[i].Layout == "tabbed" || chain[i].Layout == "stacked" {
			b.add("[con_id=%s] focus%s; layout %s", first, climb, layoutWord(chain[i].Layout))
		}
	}
	b.children(inner)
}

// splitWord is the split command that creates a container with the given
// layout, or one to turn into it.
func splitWord(layout string) string {
	if layout == "splitv" {
		return "splitv"
	}
	return "splith"
}

func (b *scatterBuild) companion(window NodeID) NodeID {
	for _, id := range b.leaves {
		if !b.placed[id] && id != window {
			return id
		}
	}
	if len(b.spare) > 0 {
		return b.spare[0]
	}
	return ""
}

// moveAfter puts window right after anchor, in the same container.
func (b *scatterBuild) moveAfter(window, anchor NodeID) {
	b.add("[con_id=%s] floating disable", window)
	b.add("[con_id=%s] mark --add %s", anchor, scatterAnchor)
	b.add("[con_id=%s] move container to mark %s", window, scatterAnchor)
}

// save writes the plan where the next run of the daemon will look for it.
func (h *scatterHandler) save(ctx *WindowContext) {
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
		ctx.Log.Warn("could not save the scatter state", "file", h.stateFile, "error", err)
	}
}

// forget removes the saved plan.
func (h *scatterHandler) forget(ctx *WindowContext) {
	if h.stateFile == "" {
		return
	}
	if err := os.Remove(h.stateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		ctx.Log.Warn("could not remove the scatter state", "file", h.stateFile, "error", err)
	}
}
