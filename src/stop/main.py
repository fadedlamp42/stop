"""
stop: top, but for spaces ("space top")

uses yabai to show where terminals (tmux, kitty) are running and where
there's free space for something new. one-shot command — prints a
color-coded table of all spaces across all displays and exits.

macOS only (yabai is macOS-only).

# data sources

queries up to three things:

  1. yabai -m query --spaces
     all spaces across all displays. each has an index, display number,
     focus/visibility state, and an optional label.

  2. yabai -m query --windows
     all windows. each has an app name, title, space assignment, PID,
     and visibility flags. grouped by space for display.

  3. tmux list-sessions (optional)
     if tmux is running, shows session summary at the bottom as extra
     context. not correlated to specific spaces (that would require
     tty-to-window mapping which is complex and kitty-version-dependent).

# terminal detection

terminal emulator app names are matched against a known set (kitty,
iTerm2, Terminal.app, Alacritty, WezTerm, etc.). terminals are shown
in green with their window title (which often contains the tmux session
name or current directory, set via escape sequences by the shell/tmux).

# output format

spaces grouped by display, each with:
  - index number
  - focus indicator (* = focused, · = visible on another display)
  - space label if set in yabai
  - window summary (terminals in green, others grouped by app name)
  - empty spaces shown as dim "--"

summary line at bottom: free space count + terminal space count.
tmux session list if tmux is running.
"""

import json
import os
import subprocess
import sys
from typing import Optional


# terminal emulator app names as reported by macOS / yabai.
# matched exactly against the window's "app" field.
TERMINAL_APPS = {
    "kitty",
    "iTerm2",
    "Terminal",
    "Alacritty",
    "WezTerm",
    "Hyper",
    "Rio",
    "Tabby",
}


# -- ansi helpers --
#
# simple ANSI color wrappers. disabled when stdout isn't a tty or
# NO_COLOR env is set (https://no-color.org/).


def _use_color():
    """check if stdout supports color output."""
    if os.environ.get("NO_COLOR"):
        return False
    return hasattr(sys.stdout, "isatty") and sys.stdout.isatty()


_COLOR = _use_color()


def _ansi(code, text):
    """wrap text in ANSI escape codes if color is enabled."""
    if _COLOR:
        return "\033[%(code)sm%(text)s\033[0m" % {"code": code, "text": text}
    return text


def dim(text):
    return _ansi("2", text)


def green(text):
    return _ansi("32", text)


def cyan(text):
    return _ansi("36", text)


def bold(text):
    return _ansi("1", text)


def yellow(text):
    return _ansi("33", text)


# -- yabai queries --


def query_yabai(domain):
    """query yabai for spaces, windows, or displays.

    returns parsed JSON list, or None if yabai isn't running / errored.
    """
    try:
        result = subprocess.run(
            ["yabai", "-m", "query", "--%(domain)s" % {"domain": domain}],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode != 0:
            return None
        return json.loads(result.stdout)
    except (subprocess.TimeoutExpired, FileNotFoundError, json.JSONDecodeError):
        return None


def get_tmux_sessions():
    """get tmux session names with window counts for the summary line.

    returns a list of strings like ["dev:3w", "build:2w"].
    returns empty list if tmux isn't running.
    """
    try:
        result = subprocess.run(
            ["tmux", "list-sessions", "-F", "#{session_name}:#{session_windows}w"],
            capture_output=True,
            text=True,
            timeout=2,
        )
        if result.returncode != 0:
            return []
        return [
            line.strip() for line in result.stdout.strip().splitlines() if line.strip()
        ]
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return []


# -- formatting --


def format_windows_for_space(windows):
    """format a space's windows into a single display line.

    terminals are shown individually in green with their window title
    (which often reveals tmux session or cwd via shell title-setting).
    non-terminals are grouped by app name with a count for duplicates.
    """
    if not windows:
        return dim("--")

    terminals = [w for w in windows if w["app"] in TERMINAL_APPS]
    others = [w for w in windows if w["app"] not in TERMINAL_APPS]

    parts = []

    # terminals: show app + title (title is the useful part)
    for w in terminals:
        title = w.get("title", "").strip()
        if title:
            if len(title) > 50:
                title = title[:47] + "..."
            parts.append(
                green("%(app)s: %(title)s" % {"app": w["app"], "title": title})
            )
        else:
            parts.append(green(w["app"]))

    # non-terminals: group by app, show count when >1
    app_counts: dict[str, int] = {}
    for w in others:
        app = w["app"]
        app_counts[app] = app_counts.get(app, 0) + 1

    for app in sorted(app_counts):
        count = app_counts[app]
        if count > 1:
            parts.append("%(app)s (%(n)d)" % {"app": app, "n": count})
        else:
            parts.append(app)

    return "  ".join(parts) if parts else dim("--")


# -- main --


def cli():
    """entry point: query yabai and print the space overview table."""
    raw_spaces = query_yabai("spaces")
    if raw_spaces is None:
        print("error: couldn't query yabai (is it running?)")
        sys.exit(1)

    raw_windows = query_yabai("windows") or []

    # index windows by space, skipping hidden and minimized.
    # hidden = app hidden via cmd+H, minimized = window in dock.
    # neither occupies visual space, so we don't count them.
    windows_by_space: dict[int, list] = {}
    for w in raw_windows:
        space_index = w.get("space", 0)
        if space_index <= 0:
            continue
        if w.get("is-hidden", False):
            continue
        if w.get("is-minimized", False):
            continue
        if space_index not in windows_by_space:
            windows_by_space[space_index] = []
        windows_by_space[space_index].append(w)

    # group spaces by display for sectioned output
    by_display: dict[int, list] = {}
    for s in raw_spaces:
        display = s.get("display", 1)
        if display not in by_display:
            by_display[display] = []
        by_display[display].append(s)

    free_indices: list[int] = []
    terminal_indices: list[int] = []

    for display_idx in sorted(by_display):
        spaces = sorted(by_display[display_idx], key=lambda s: s.get("index", 0))
        print(
            cyan("display %(d)d" % {"d": display_idx})
            + dim(" (%(n)d spaces)" % {"n": len(spaces)})
        )

        for space in spaces:
            idx = space.get("index", 0)
            label = space.get("label", "")
            focused = space.get("has-focus", False)
            visible = space.get("is-visible", False)

            # focus indicator:
            #   * = focused (your cursor is here)
            #   · = visible on another display (you can see it without switching)
            #   (space) = not currently visible
            indicator = " "
            if focused:
                indicator = "*"
            if not focused and visible:
                indicator = "\u00b7"

            wins = windows_by_space.get(idx, [])
            win_text = format_windows_for_space(wins)

            # optional space label from yabai config
            label_text = ""
            if label:
                label_text = dim("[%(l)s] " % {"l": label})

            if not wins:
                free_indices.append(idx)
            if any(w["app"] in TERMINAL_APPS for w in wins):
                terminal_indices.append(idx)

            print(
                "  %(idx)2d %(ind)s  %(label)s%(wins)s"
                % {"idx": idx, "ind": indicator, "label": label_text, "wins": win_text}
            )

        print()

    # summary: free count + terminal count with space indices
    summary_parts = []
    if free_indices:
        free_list = ", ".join(str(i) for i in free_indices)
        summary_parts.append(
            green("%(n)d free" % {"n": len(free_indices)})
            + dim(" (%(l)s)" % {"l": free_list})
        )
    else:
        summary_parts.append(yellow("0 free"))

    if terminal_indices:
        term_list = ", ".join(str(i) for i in terminal_indices)
        summary_parts.append(
            "%(n)d with terminals" % {"n": len(terminal_indices)}
            + dim(" (%(l)s)" % {"l": term_list})
        )

    print("  ".join(summary_parts))

    # tmux session summary (not correlated to spaces, just extra context)
    tmux = get_tmux_sessions()
    if tmux:
        print(dim("tmux: %(s)s" % {"s": "  ".join(tmux)}))


if __name__ == "__main__":
    cli()
