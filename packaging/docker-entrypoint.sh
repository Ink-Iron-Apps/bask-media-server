#!/bin/sh
#
# Make the image work the same way for everybody.
#
# Three cases, in this order:
#
#   1. A configuration file is mounted at /config/bms.conf, which is the way
#      somebody running this for real will do it: one file to edit, no command
#      line to remember, and settings that survive recreating the container.
#   2. Arguments were given to docker run, which win over everything.
#   3. Neither, which still has to start and serve something, because a person
#      trying the image for the first time should not have to read a manual to
#      see it run.
#
# Nothing in here is specific to any one installation. There are no paths, no
# libraries and no keys baked into this image.

set -eu

CONFIG_DIR=${BMS_CONFIG_DIR:-/config}
CONFIG=$CONFIG_DIR/bms.conf
DATA=${BMS_DATA_DIR:-/var/lib/bask-media-server}

SAMPLE=/usr/share/bask-media-server/bms.conf.example

# Leave an annotated copy beside wherever the configuration goes, so somebody
# who mounted an empty folder has something to start from rather than having to
# find it on a website.
#
# A mounted folder belongs to the host, and this runs as 1000 -- so on a
# machine whose first user is not 1000 the copy cannot be written. That is the
# ordinary case, not an error, and saying nothing about it is how somebody ends
# up staring at an empty folder wondering what to put in it.
if [ -d "$CONFIG_DIR" ] && [ ! -e "$CONFIG_DIR/bms.conf.example" ]; then
    if cp "$SAMPLE" "$CONFIG_DIR/bms.conf.example" 2>/dev/null; then
        echo "bms: wrote $CONFIG_DIR/bms.conf.example to copy and edit" >&2
    else
        echo "bms: cannot write to $CONFIG_DIR (it belongs to $(stat -c '%u:%g' "$CONFIG_DIR") and this runs as $(id -u):$(id -g))." >&2
        echo "bms: for the annotated example, run:" >&2
        echo "bms:   docker run --rm --entrypoint cat <this image> $SAMPLE > bms.conf" >&2
        echo "bms: or pass --user \"\$(id -u):\$(id -g)\" so the folder is writable." >&2
    fi
fi

# The data directory is the one thing that must be writable, and the error
# from failing to open a database there is "unable to open database file (14)",
# which tells nobody anything. --user and a named volume is the combination
# that produces it: the volume is created owned by 1000 from the image, and
# then something else is asked to write to it.
if [ -d "$DATA" ] && [ ! -w "$DATA" ]; then
    echo "bms: $DATA is not writable (it belongs to $(stat -c '%u:%g' "$DATA") and this runs as $(id -u):$(id -g))." >&2
    echo "bms: the database, the artwork cache and the backups go there, so this will fail." >&2
    echo "bms: either drop --user, or mount a directory you own over it:" >&2
    echo "bms:   -v \"\$PWD/data:$DATA\"" >&2
fi

if [ -f "$CONFIG" ]; then
    exec /usr/local/bin/bms -config "$CONFIG" "$@"
fi

# No configuration file. If the first argument looks like a flag, the caller is
# driving it themselves and these defaults would fight with theirs.
if [ "$#" -gt 0 ]; then
    exec /usr/local/bin/bms "$@"
fi

exec /usr/local/bin/bms \
    -db "$DATA/bms.db" \
    -addr 0.0.0.0:32500 \
    -scan
