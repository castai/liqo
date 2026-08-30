#!/bin/sh

# File holding the running total.
countfile=$(mktemp)
echo 0 >"$countfile"

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

# Ensure the counter file and the background timer are cleaned up on exit.
cleanup() {
    rm -f "$countfile" "$countfile.new"
    kill "$timer_pid" 2>/dev/null || true
}
trap 'cleanup' INT TERM EXIT

# Continuously capture HTTP data packets and atomically increment the counter.
tcpdump -l -n -i any 'tcp port 80 and tcp[13] & 8 != 0' 2>/dev/null | while read -r _; do
    old=$(cat "$countfile")
    new=$((old + 1))
    printf '%s\n' "$new" >"$countfile.new"
    mv "$countfile.new" "$countfile"
done