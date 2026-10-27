# ws-rename

`ws-rename` is a small background helper for the [Sway](https://swaywm.org/)
window manager. It watches your workspaces and automatically renames each one
to show an icon for every application you have open in it, so instead of a
bare workspace number your bar shows something like:

```
1:🦊|📝   2:🎵   3:💻|💬
```

No more guessing which workspace has your browser, your editor, or your chat
app — you can see it at a glance in the bar.

## How it works

Every time you open, close, move, or rename a window, `ws-rename` looks at
which applications are present on each workspace and renames the workspace to
a string made of the workspace number followed by one icon (or short label)
per open application, separated by `|`. If it doesn't recognize an
application, the fallback depends on the window: native Wayland windows fall
back to a generic icon (▨); windows running through XWayland (apps that
haven't been ported to native Wayland) fall back to their own class (or
instance, if class isn't set) in lowercase — see below for why the two
differ.

The program runs quietly in the background and does not require any
interaction once it's set up.

## Requirements

- Sway, with a status bar that displays workspace names (the default bar
  does).
- A font that can render the icons or emoji you choose to use (most modern
  system fonts and emoji fonts work fine).

## Installing

The simplest way is to grab a prebuilt binary from the
[Releases](../../releases) page — `ws-rename-linux-amd64` or
`ws-rename-linux-arm64`, depending on your machine — and copy it somewhere on
your `PATH`, for example `~/.local/bin/ws-rename` (remember to `chmod +x` it).

`ws-rename` is written in Go and has no external dependencies, so if you'd
rather build it yourself all you need is a Go toolchain (Go 1.26 or newer):

```sh
go build -o ws-rename .
```

Copy the resulting `ws-rename` binary somewhere on your `PATH`, for example
`~/.local/bin/`.

## Setting up your icons

You tell `ws-rename` which icon or label to show for each application through
a configuration file named `app-icons.json`. By default the program looks for
it in:

1. `$XDG_CONFIG_HOME/sway/app-icons.json` (if `XDG_CONFIG_HOME` is set)
2. `~/.config/sway/app-icons.json`

The file is a JSON list, one entry per application. Native Wayland windows are
matched by *app ID* and, optionally, by a pattern on the window title; windows
running through XWayland (apps that haven't been ported to native Wayland)
are matched by *window class* and/or *window instance* instead, the same
properties X11 windows have always exposed. A minimal example:

```json
[
  { "window_class": "firefox", "icon": "🦊" },
  { "window_class": "code", "window_instance": "code", "icon": "📝" },
  { "app_id": "firefox", "icon": "🦊" },
  { "app_id": "foot", "icon": "💻" },
  { "app_id": "foot", "name": "^weechat", "icon": "💬" }
]
```

- `window_class` / `window_instance` — used for XWayland windows, these
  correspond to the values `xprop` shows for a window (`WM_CLASS`).
- `app_id` — the Wayland application identifier, used for native Wayland
  windows.
- `name` — optional; matches against the window title, letting you show a
  different icon for the same application depending on what's open in it
  (e.g. a terminal running a chat client vs. a plain shell).
- `icon` — the text shown on the workspace: typically a single emoji or a
  glyph from an icon font, but any short string works.

When more than one entry could match a window, the most specific one wins:
for XWayland windows, class+instance beats class alone, which beats instance
alone; for native Wayland windows, app_id+name beats app_id alone. If nothing
matches, XWayland windows fall back to their own class (or instance) in
lowercase, since X11 windows essentially always have one; native Wayland
windows are only guaranteed an app_id, so there's nothing meaningful to fall
back to and you get a generic icon (▨) instead.

You don't need to restart `ws-rename` after editing this file — just restart
the program (see below) to pick up your changes.

## Extra behaviours

Because the program is already listening to every window event the compositor
emits, it can host other event-driven behaviours at no extra cost — a single
IPC subscription instead of one background script per feature. Both of the
following are off unless you turn them on.

### Firefox title rules

Sway evaluates `for_window` criteria once, when a window is mapped. Firefox on
Wayland sets its real title through a separate `title` event that arrives
*later*, so `for_window [app_id="org.mozilla.firefox" title="..."]` often never
fires. This feature applies the same rules when the title actually shows up.

It turns itself on if a file named `firefox-title-rules.conf` exists in the
same directories searched for `app-icons.json`. One rule per line, split on the
first `=`; blank lines and lines starting with `#` are ignored:

```
.*\| Brilliant — Mozilla Firefox=move container to workspace number 5, workspace number 5
```

The left-hand side is a regular expression matched against the window title
(unanchored, like sway's own criteria); the right-hand side is the list of
commands, applied exactly as they would be inside a `for_window` block. Each
rule runs at most once per window.

### Autotiling

With `--autotiling`, the split direction of the focused window is kept in sync
with its shape: wider than tall means the next window opens beside it, taller
than wide means below. Without it, Sway keeps splitting the same axis until
you are left with unusably narrow columns. Floating and fullscreen
windows, and containers in a tabbed or stacked parent, are left alone.

A new window also gets only part of the space instead of half of it: by
default 30%, leaving 70% to the window that was already there, on the theory
that what you were working on shouldn't be cut in half by what you just
opened. Use `--split-ratio` to change the share, or `--split-ratio 50` to go
back to even splits. Note that this applies to newly opened windows only —
a window *moved* into an existing container still lands on an even split.

For that share to be taken from the window you are looking at, the window has
to be one Sway is willing to split. Left to itself, Sway only splits the
focused window when it is alone in its container: with anything else beside it,
a new window is added as one more sibling and the whole row is squeezed to make
room — which is why, without this, the ratio quietly stopped applying past the
second window on a workspace. `--autotiling` therefore wraps a focused window
that has siblings in a container of its own, so that whatever you open next
divides *that* window and leaves the rest of the layout where it is. The extra
container changes nothing on screen, and a window that already has one is left
alone.

#### Tabs instead of slivers

There is a point past which splitting again stops making sense: a column four
hundred pixels wide is not enough for an editor, and what you actually want is
both windows at full size, one in front of the other. `--autotiling` does that
by itself. Before a window opens, the space it is going to get is compared
against `--tab-min-width` (500 pixels by default); if it falls short, the
container is turned into a tabbed one instead of being split, and the new
window becomes a tab of it. `--stack-min-height` (300 by default) is the same
rule for a vertical split, where the container is stacked rather than tabbed.
Pass `0` to either flag to switch that half off.

Both defaults are deliberately low: splitting a window you wanted tabbed only
leaves you where you were before, whereas tabbing one you wanted split hides a
window you were looking at. Raise them until the program tabs as often as you
would have done by hand.

The comparison is made on the size the window is *about* to get, so it opens
straight into its tab: nothing is resized first and reshuffled afterwards.
Only a container holding exactly two windows is ever tabbed this way — one
that already has three or more is a layout you built on purpose, and folding
it away would hide windows you never asked about.

When one of the two windows is closed and a tab bar over a lone window is all
that would be left, the container goes back to the layout it had. This only
happens to containers the program tabbed itself: if you make one tabbed by
hand, or take over one of these, it stays exactly as you left it.

### Maximize without fullscreen

`fullscreen` covers the entire output, bar included, which is more than you
want when all you need is a good look at one window for a minute. With
`--maximize` the program offers a gentler version: the marked window is given
the whole workspace — bar still there, gaps still respected — by parking every
other window of the workspace on a workspace of its own, `11:⏸`, for the
duration. Removing the mark brings them back, layout, proportions and all, and
puts the window back in its place among them.

The trigger is a Sway mark rather than a command of its own, so a plain binding
is all it takes:

```
bindsym $mod+z mark --add --toggle _wsr_max
```

Sway reports the mark as a window event, which is how the program hears about
it. A mark belongs to one window at a time, so marking another window while one
is maximized restores the first and maximizes the second in one go. The mark
name begins with an underscore, which is Sway's convention for marks that
should not be drawn in window titles.

The parked windows travel as a single piece, so they keep their arrangement
while they are away, and `11:⏸` stays in the bar for as long as they are
there: should anything ever go wrong, that is where to fetch them. Its number
puts it after the ten workspaces most people use; its name is left alone by the
icon renaming. Floating windows are not parked, they stay where they are.

If the maximized window goes away in the meantime — closed, moved to another
workspace, made floating — the others come back on their own, as if it had
gone from the original layout. Windows opened while it is maximized end up next
to it once the others are back. What to restore is also written to
`$XDG_RUNTIME_DIR/ws-rename/maximize.json`, so if the program is restarted
while a window is maximized, it puts everything back as soon as it starts. And
with `--expose` on, calling up the exposé ends the maximize first, since most of
what it would show is parked.

### Exposé

Tabs and stacks are a great way to keep a workspace tidy, and an equally good
way to forget what is in it. With `--expose` one key lays out every window of
the focused workspace side by side, and pressing it again puts everything back
the way it was:

```
bindsym XF86Tools nop ws-rename-expose
```

Any key will do; `nop` is a Sway command that does nothing, but Sway still
reports the binding to the programs listening, which is how this one hears
about it. Under the hood every tabbed or stacked container on the workspace is
turned into a split — side by side if it is wider than tall, one above the
other otherwise — and back again: no window is moved, so there is nothing that
could end up in the wrong place.

A tabbed container shows the window that last had the focus, so the exposé
doubles as a window picker: move the focus to the window you are after, with
the mouse or the keyboard, and press the key again to land on it. Pressing the
key on another workspace ends the exposé on the first one and starts it there.
While it lasts, `--autotiling` and `--maximize` stand aside, so that they do not
rearrange what the exposé has just laid out.

It only shows what fits: a tabbed container with many windows in a narrow
column becomes as many narrow slivers. And what to restore lives in the running
program, so restarting it in the middle of an exposé leaves the containers
split.

### Scattered exposé

`--scatter` is the exposé taken one step further, closer to the one on macOS:
every window of the focused workspace leaves the layout, becomes floating and
is dealt out over the screen, roughly in the order they were on screen. Click a
window and everything goes back in place, with the focus on it; pressing the
key again does the same, keeping the focus where it was.

There are two ways to deal the windows out, each on a binding of its own, so
both can be on at once:

```
bindsym $mod+F4       nop ws-rename-scatter
bindsym $mod+Shift+F4 nop ws-rename-scatter-uniform
```

- **`ws-rename-scatter`, on a grid.** The screen is split into cells of the
  same size, as many columns as give the cells the most room, and each window
  is shrunk to fit its own cell. The windows come out about as big as each
  other, whatever their size was: a small one is easy to spot, but a big one
  is shrunk more than the others, and a tall window in a wide cell leaves room
  on its sides.
- **`ws-rename-scatter-uniform`, all shrunk alike.** Every window is shrunk by
  the same factor, as macOS does, so a window twice as big as another stays
  twice as big; they are laid in rows, each row centred. The factor is the
  largest that fits them all on the screen. The screen fills better with
  windows of mixed shapes, and their sizes still tell which is which, but next
  to a big window a small one stays small.

Neither enlarges a window. Either key ends a scatter started with the other.

Unlike macOS, Sway cannot shrink what a window shows, so these are not
thumbnails: each window really gets smaller, and redraws itself to fit — a
terminal wraps its lines, a web page may switch to its mobile layout — and
again on the way back. Windows that were floating already are dealt out too,
and go back where they were.

Any focus change ends it: a click on a window, but also moving the focus with
the keyboard or opening a new window. For the same reason the focus stops
following the mouse while it lasts, or crossing a window on the way to another
would pick it; afterwards `focus_follows_mouse` is set back to `yes`, Sway's
default. Windows closed meanwhile are simply left out, windows opened meanwhile
go in wherever Sway puts a new window. Pressing the key on another workspace
puts the first one back and scatters the second.

It can be on together with `--expose`, on another key: each key ends the
other mode. When the exposé is on, or a window is maximized, the key only ends
that, and a second press scatters. What to restore is kept in
`$XDG_RUNTIME_DIR/ws-rename/scatter.json`, so restarting the program in the
middle puts the workspace back as soon as it starts.

## Running it

Add a line like this to your Sway configuration file so the program starts
automatically with your session:

```
exec ws-rename --daemon
```

Available command-line options:

| Flag              | Description                                                          |
| ----------------- | --------------------------------------------------------------------- |
| `-d`, `--daemon`  | Run in the background instead of staying attached to your terminal.  |
| `-v`, `--verbose` | Log extra diagnostic detail, useful when troubleshooting.            |
| `-c`, `--conf`    | Path to a specific `app-icons.json` file, if you don't want to rely on the default locations. |
| `--autotiling`    | Keep the split direction of the focused window matched to its shape. |
| `--maximize`      | Give the window marked `_wsr_max` the whole workspace until the mark is removed, keeping the bar and the gaps; the other windows wait on workspace `11:⏸`. |
| `--expose`        | Lay out every window of the focused workspace side by side, tabbed and stacked ones included, until the key bound to `nop ws-rename-expose` is pressed again. |
| `--scatter`       | Take every window of the focused workspace out of the layout and deal them out as floating windows, until one is clicked or the key bound to `nop ws-rename-scatter` (on a grid) or `nop ws-rename-scatter-uniform` (all shrunk alike) is pressed again. |
| `--split-ratio`   | Percentage of the container given to a newly opened window (default: 30; 50 splits evenly). Requires `--autotiling`. |
| `--tab-min-width` | Smallest width, in pixels, a new window may get out of a horizontal split; below it the container is made tabbed instead (default: 500; 0 disables). Requires `--autotiling`. |
| `--stack-min-height` | The same for a vertical split, where the container is stacked instead (default: 300; 0 disables). Requires `--autotiling`. |
| `--firefox-rules` | Path to a specific Firefox title rules file; pass an empty value to disable the feature even when the file exists. |
| `--stderr`        | Log to stderr instead of the system log, handy when running in the foreground. |
| `--version`       | Print the program's version and exit.                                |
| `-u`, `--uniq`    | Accepted for backwards compatibility, but currently has no effect (consecutive duplicate icons are always collapsed). |

The program writes to the system log, so if something doesn't look like it
should — an icon not showing up, a workspace not renaming, a rule not firing —
that's the first place to check:

```sh
journalctl -t ws-rename -f
```

Running with `--verbose` adds a detailed trace of everything the program saw
and did, including the layout decisions it takes on every focus change.

## Restarting after a Sway reload

If Sway restarts (for example after a config reload), `ws-rename`
automatically reconnects and keeps working without needing to be started
again by hand.

## License

MIT — see [LICENSE](LICENSE).
