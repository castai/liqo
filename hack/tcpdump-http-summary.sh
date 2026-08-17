#!/bin/sh

# Capture data packets (PSH flag set) on port 80 in both directions.
# A packet going *to* port 80 is a request, a packet coming *from* port 80 is a response.
#
# When capturing on '-i any', tcpdump prefixes lines with 'In' or 'Out'.
# We use sed to extract src/dst reliably regardless of that extra field.
tcpdump -l -n -i any 'tcp port 80 and tcp[13] & 8 != 0' 2>/dev/null | while read -r line; do
    src=$(echo "$line" | sed -n 's/.* IP \([^ ]*\) > \([^:]*\):.*/\1/p')
    dst=$(echo "$line" | sed -n 's/.* IP \([^ ]*\) > \([^:]*\):.*/\2/p')
    ts=$(echo "$line" | awk '{print $1}')

    src_port=${src##*.}
    dst_port=${dst##*.}

    if [ "$dst_port" = "80" ]; then
        direction="request"
    else
        direction="response"
    fi

    echo "HTTP $direction at $ts ($src -> $dst)"
done
