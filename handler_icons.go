package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// fallbackIcon is shown when no config entry matches a window.
const fallbackIcon = "▨"

// ── Tree traversal ────────────────────────────────────────────────────────────

func traverseDeepFirst(f func(n *Node, parent *Node, depth int), n *Node, parent *Node, depth int) {
	f(n, parent, depth)
	for i := range n.Nodes {
		traverseDeepFirst(f, &n.Nodes[i], n, depth+1)
	}
	for i := range n.FloatingNodes {
		traverseDeepFirst(f, &n.FloatingNodes[i], n, depth+1)
	}
}

func getWorkspaceNodes(root *Node) []*Node {
	var ws []*Node
	traverseDeepFirst(func(n *Node, _ *Node, _ int) {
		if n.NodeType == "workspace" && n.Num != nil && *n.Num > 0 {
			ws = append(ws, n)
		}
	}, root, nil, 0)
	return ws
}

func extractLeaves(root *Node) []*Node {
	var leaves []*Node
	traverseDeepFirst(func(n *Node, _ *Node, _ int) {
		if len(n.Nodes) == 0 && len(n.FloatingNodes) == 0 && n.NodeType != "workspace" {
			leaves = append(leaves, n)
		}
	}, root, nil, 0)
	return leaves
}

// ── Icon resolution ───────────────────────────────────────────────────────────

func stringOfNode(conf []IconConf, n *Node) string {
	if n.WindowProps == nil {
		// Wayland / Sway path: match by app_id + optional name regex
		if n.AppID == nil {
			return fallbackIcon
		}
		appID := strings.ToLower(*n.AppID)
		var byAppID []IconConf
		for _, r := range conf {
			if r.AppID != nil && *r.AppID == appID {
				byAppID = append(byAppID, r)
			}
		}
		if n.Name != nil {
			nodeName := strings.ToLower(*n.Name)
			// Take first record where name is nil (matches any) or regex matches.
			for _, r := range byAppID {
				if r.Name == nil {
					return r.Icon
				}
				re, err := regexp.Compile(*r.Name)
				if err == nil && re.MatchString(nodeName) {
					return r.Icon
				}
			}
			return fallbackIcon
		}
		for _, r := range byAppID {
			if r.Name == nil {
				return r.Icon
			}
		}
		return fallbackIcon
	}

	// XWayland path: match by window class and/or instance
	wp := n.WindowProps
	switch {
	case wp.Class_ == nil && wp.Instance == nil:
		return fallbackIcon
	case wp.Class_ == nil:
		if icon := searchInstance(conf, *wp.Instance); icon != nil {
			return *icon
		}
		return strings.ToLower(*wp.Instance)
	case wp.Instance == nil:
		if icon := searchClass(conf, *wp.Class_); icon != nil {
			return *icon
		}
		return strings.ToLower(*wp.Class_)
	default:
		if icon := searchClassInstance(conf, *wp.Class_, *wp.Instance); icon != nil {
			return *icon
		}
		if icon := searchClass(conf, *wp.Class_); icon != nil {
			return *icon
		}
		return strings.ToLower(*wp.Class_)
	}
}

// removeDups removes consecutive duplicates (mirrors OCaml remove_dups).
func removeDups(xs []string) []string {
	if len(xs) == 0 {
		return xs
	}
	out := []string{xs[0]}
	for _, x := range xs[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}

// ── Handler ───────────────────────────────────────────────────────────────────

// iconHandler renames every workspace after the applications it contains.
type iconHandler struct {
	conf []IconConf
}

func newIconHandler(conf []IconConf) *iconHandler { return &iconHandler{conf: conf} }

func (h *iconHandler) Name() string { return "icons" }

func (h *iconHandler) HandleWindow(ctx *WindowContext) error {
	switch ctx.Info.Change {
	case WinNew, WinClose, WinTitle, WinMove:
	default:
		return nil
	}

	tree, err := ctx.Tree()
	if err != nil {
		return fmt.Errorf("get_tree: %w", err)
	}
	for _, ws := range getWorkspaceNodes(tree) {
		// The windows parked there by maximize belong to another workspace,
		// and the name says so.
		if ws.Name != nil && *ws.Name == maximizeStash {
			continue
		}
		if err := h.renameWorkspace(ctx, ws); err != nil {
			ctx.Log.Error("rename_workspace failed", "error", err)
		}
	}
	return nil
}

func (h *iconHandler) renameWorkspace(ctx *WindowContext, ws *Node) error {
	wsName := "N/A"
	if ws.Name != nil {
		wsName = *ws.Name
	}
	wsNum := 0
	if ws.Num != nil {
		wsNum = *ws.Num
	}

	leaves := extractLeaves(ws)
	icons := make([]string, len(leaves))
	for i, leaf := range leaves {
		icons[i] = stringOfNode(h.conf, leaf)
	}
	icons = removeDups(icons)

	newName := strings.Join(icons, "|")
	if newName == "" {
		newName = strconv.Itoa(wsNum)
	} else {
		newName = fmt.Sprintf("%d:%s", wsNum, newName)
	}

	if wsName == newName {
		return nil
	}

	return ctx.Command(fmt.Sprintf(`rename workspace "%s" to "%s"`, wsName, newName))
}
