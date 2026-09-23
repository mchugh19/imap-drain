#!/bin/bash
# Install imap-drain: binary, sources, systemd template units, config dir.
# Run as root from the repository root (or a copy of it).
set -u

if [ "$(id -u)" -ne 0 ]; then
    echo "run as root" >&2
    exit 1
fi

STAGE="$(cd "$(dirname "$0")" && pwd)"
BIN=/usr/local/bin/imap-drain
SRCB=/usr/local/src/imap-drain
CONFD=/etc/imap-drain

echo "== drain binary =="
install -m 0755 "$STAGE/src/imap-drain" "$BIN"

echo "== sources (for review / future rebuilds) =="
mkdir -p "$SRCB"
cp "$STAGE/src"/drain.go "$STAGE/src"/drain_test.go "$STAGE/src"/go.mod "$STAGE/src"/go.sum "$SRCB"/
mkdir -p "$SRCB/cmd/oauth-setup"
cp "$STAGE/src"/cmd/oauth-setup/main.go "$SRCB/cmd/oauth-setup"/
cp "$STAGE"/imap-drain.conf.example "$STAGE"/README.md "$SRCB"/

echo "== systemd template units =="
cp "$STAGE/systemd/imap-drain@.service" /etc/systemd/system/
cp "$STAGE/systemd/imap-drain@.timer"   /etc/systemd/system/
chmod 0644 /etc/systemd/system/imap-drain@.service /etc/systemd/system/imap-drain@.timer
systemctl daemon-reload

echo "== config dir =="
mkdir -p "$CONFD"
chmod 0750 "$CONFD"
if [ ! -f "$CONFD/drain.conf.example" ]; then
    cp "$STAGE/imap-drain.conf.example" "$CONFD/drain.conf.example"
fi

cat <<EOF
done.

To drain a mailbox pair:
  1. cp $CONFD/drain.conf.example $CONFD/<name>.conf and edit it
  2. imap-drain -config $CONFD/<name>.conf   # test it by hand first
  3. systemctl enable --now 'imap-drain@<name>.timer'
EOF
