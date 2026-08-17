#!/bin/sh

# Count all HTTP data packets (PSH flag set) seen since the script started and
# print a single cumulative number every second, clearing the screen before each update.
#
# This version keeps a single continuous tcpdump capture (no gaps) and uses a
# tiny background loop only for the once-per-second display.

clean

# File holding the running total.
countfile=$(mktemp)
echo 0 >"$countfile"

# Ensure the counter file and the background timer are cleaned up on exit.
trap 'rm -f "$countfile"; kill "$timer_pid" 2>/dev/null; exit' INT TERM EXIT

# Background timer: every second clear the screen and print the current total.
print_count() {
    while true; do
        sleep 1
        clear
        echo "Total HTTP packets: $(cat "$countfile")"
    done
}

print_count &
timer_pid=$!

# Continuously capture HTTP data packets and atomically increment the counter.
tcpdump -l -n -i any 'tcp port 80 and tcp[13] & 8 != 0' 2>/dev/null | while read -r _; do
    old=$(cat "$countfile")
    new=$((old + 1))
    printf '%s\n' "$new" >"$countfile.new"
    mv "$countfile.new" "$countfile"
done
