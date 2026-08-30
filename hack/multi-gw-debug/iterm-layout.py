#!/usr/bin/env python3
"""iTerm2 Python API helper for multi-gateway debugging layout.

This script is invoked by debug-multi-gw.sh. It splits the current iTerm2
window into two columns (one per Kubernetes context) and runs the requested
debugging script on every gateway pod, aligned by row.
"""

import asyncio
import os
import shlex
import sys

import iterm2


def usage() -> None:
    print(
        "Usage: iterm-layout.py <mode> <ctx1> <ctx2> <pods1> <pods2> <script_path>",
        file=sys.stderr,
    )
    print("  pods1/pods2 are comma-separated lists of namespace/name values.", file=sys.stderr)


def parse_pods(value: str) -> list[str]:
    return [p for p in value.split(",") if p]


def build_command(ctx: str, pod: str, script_path: str) -> str:
    ns, name = pod.split("/", 1)
    script_name = os.path.basename(script_path)
    remote_path = f"/tmp/{script_name}"

    # Clear the terminal (fallback to ANSI escape if clear is unavailable)
    # and run the copied script with /bin/sh so execute permissions are not
    # strictly required.
    inner_command = f"clear 2>/dev/null || printf '\\033[2J\\033[H'; sh {remote_path}"

    return (
        f"kubectl --context {shlex.quote(ctx)} cp {shlex.quote(script_path)} "
        f"{shlex.quote(ns)}/{shlex.quote(name)}:{remote_path} && "
        f"kubectl --context {shlex.quote(ctx)} -n {shlex.quote(ns)} "
        f"exec -it {shlex.quote(name)} -- "
        f"/bin/sh -c {shlex.quote(inner_command)}\n"
    )


async def build_grid(
    tab: iterm2.Tab,
    pods1: list[str],
    pods2: list[str],
) -> tuple[list[iterm2.Session], list[iterm2.Session]]:
    """Create a two-column grid of sessions aligned by row.

    The algorithm builds the layout row-by-row to avoid splitting the same pane
    in two different directions, which can confuse iTerm2's splitter tree.
    """
    sessions_col1: list[iterm2.Session] = []
    sessions_col2: list[iterm2.Session] = []

    # Use the current session for column 1 if available; otherwise use it for
    # column 2 so we never leave the original pane empty.
    if pods1:
        sessions_col1.append(tab.current_session)
    elif pods2:
        sessions_col2.append(tab.current_session)

    # Create the first row of column 2 by splitting the first column 1 pane
    # vertically. From this point on each pane is only split horizontally.
    if pods1 and pods2:
        first_col2 = await sessions_col1[0].async_split_pane(vertical=True)
        sessions_col2.append(first_col2)
        await asyncio.sleep(0.1)

    max_rows = max(len(pods1), len(pods2))
    for _row in range(1, max_rows):
        if len(sessions_col1) < len(pods1):
            new_session = await sessions_col1[-1].async_split_pane(vertical=False)
            sessions_col1.append(new_session)
        if len(sessions_col2) < len(pods2):
            new_session = await sessions_col2[-1].async_split_pane(vertical=False)
            sessions_col2.append(new_session)
        await asyncio.sleep(0.05)

    return sessions_col1, sessions_col2


async def main(connection: iterm2.Connection) -> None:
    if len(sys.argv) < 7:
        usage()
        sys.exit(1)

    _mode = sys.argv[1]
    ctx1 = sys.argv[2]
    ctx2 = sys.argv[3]
    pods1 = parse_pods(sys.argv[4])
    pods2 = parse_pods(sys.argv[5])
    script_path = sys.argv[6]

    if not pods1 and not pods2:
        print("No gateway pods to debug.", file=sys.stderr)
        sys.exit(1)

    app = await iterm2.async_get_app(connection)
    window = app.current_terminal_window
    if window is None:
        print("No current iTerm2 terminal window found.", file=sys.stderr)
        sys.exit(1)

    tab = window.current_tab
    if tab is None:
        print("No current tab found in iTerm2 window.", file=sys.stderr)
        sys.exit(1)

    sessions_col1, sessions_col2 = await build_grid(tab, pods1, pods2)

    # Label each pane so it is easy to tell which pod it belongs to.
    for i, pod in enumerate(pods1):
        try:
            await sessions_col1[i].async_set_name(f"{ctx1}: {pod}")
        except Exception:
            pass
    for i, pod in enumerate(pods2):
        try:
            await sessions_col2[i].async_set_name(f"{ctx2}: {pod}")
        except Exception:
            pass

    # Let the layout settle before typing commands.
    await asyncio.sleep(0.5)

    for i, pod in enumerate(pods1):
        await sessions_col1[i].async_send_text(build_command(ctx1, pod, script_path))

    for i, pod in enumerate(pods2):
        await sessions_col2[i].async_send_text(build_command(ctx2, pod, script_path))


iterm2.run_until_complete(main)
