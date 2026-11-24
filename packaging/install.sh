#!/bin/sh
#
# Install Bask Media Server on Linux: fetch the release build for this machine,
# verify it against the published checksums, and set it up as a systemd service
# that starts at boot.
#
# Nothing is compiled. Run it as root, or with sudo:
#
#   curl -fsSL https://baskmedia.app/install.sh | sudo sh
#
# It is safe to run again: an existing configuration file is left alone, and an
# existing installation is upgraded in place.

set -eu

REPO=${REPO:-Ink-Iron-Apps/bask-media-server}
VERSION=${VERSION:-latest}
PREFIX=${PREFIX:-/usr/local}
CONF_DIR=${CONF_DIR:-/etc/bask-media-server}
SERVICE_USER=${SERVICE_USER:-bms}
UNIT=/etc/systemd/system/bask-media-server.service

say() { printf '==> %s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run this as root, or with sudo"

for tool in curl tar sha256sum install; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is needed and is not on PATH"
done

case "$(uname -s)" in
    Linux) ;;
    *) die "this installer is for Linux; see https://baskmedia.app/install for the others" ;;
esac

case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) die "no release build for $(uname -m); build from source instead" ;;
esac

# The version, as a tag. Asking the API for "latest" keeps the download URL
# honest about which release was installed, so an upgrade can tell.
if [ "$VERSION" = latest ]; then
    say "finding the latest release"
    VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
        sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
    [ -n "$VERSION" ] || die "could not work out the latest release; set VERSION=vX.Y.Z"
fi

ARCHIVE="bms_${VERSION#v}_linux_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

say "downloading $ARCHIVE"
curl -fsSL -o "$WORK/$ARCHIVE" "$BASE/$ARCHIVE" || die "could not download $BASE/$ARCHIVE"
curl -fsSL -o "$WORK/SHA256SUMS" "$BASE/SHA256SUMS" || die "could not download the checksums"

# Verify before anything is unpacked. A checksum file covering every archive is
# filtered to this one, so a missing line fails rather than passing silently.
say "checking the download"
grep " $ARCHIVE\$" "$WORK/SHA256SUMS" > "$WORK/expected" ||
    die "$ARCHIVE is not in the published checksums"
( cd "$WORK" && sha256sum -c expected >/dev/null ) || die "the download does not match its checksum"

tar -xzf "$WORK/$ARCHIVE" -C "$WORK"
[ -f "$WORK/bms" ] || die "the archive does not contain the binary"

say "installing $PREFIX/bin/bms"
install -D -m 0755 "$WORK/bms" "$PREFIX/bin/bms"

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    say "creating the $SERVICE_USER service account"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER" ||
        die "could not create the $SERVICE_USER account"
fi

mkdir -p "$CONF_DIR"
if [ -f "$CONF_DIR/bms.conf" ]; then
    say "keeping the configuration already at $CONF_DIR/bms.conf"
    install -m 0644 "$WORK/packaging/bms.conf.example" "$CONF_DIR/bms.conf.example"
else
    say "writing $CONF_DIR/bms.conf"
    install -m 0640 "$WORK/packaging/bms.conf.example" "$CONF_DIR/bms.conf"
    chown "root:$SERVICE_USER" "$CONF_DIR/bms.conf"
fi

say "installing the service"
install -m 0644 "$WORK/packaging/bask-media-server.service" "$UNIT"
systemctl daemon-reload
systemctl enable bask-media-server >/dev/null 2>&1 || true

cat <<MESSAGE

Bask Media Server $VERSION is installed.

Next, two things only you can decide:

  1. Edit $CONF_DIR/bms.conf and add your libraries.
     A library line looks like
         section = Movies:movies:/mnt/media/Movies

  2. Give the $SERVICE_USER account read access to those folders, for example
         setfacl -R -m u:$SERVICE_USER:rX /mnt/media
     or by adding $SERVICE_USER to the group that owns them.

Then start it and read the token it prints on the first run:

     systemctl start bask-media-server
     journalctl -u bask-media-server | grep 'admin token'

The admin interface is at http://<this machine>:32500/admin.

MESSAGE
