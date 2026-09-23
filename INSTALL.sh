#!/bin/bash
# Install the generic imap-drain and migrate the yahoo-drain instance to it.
# Run as root on stratos from the staging directory.
set -u

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
cp "$STAGE"/imap-drain.conf.example "$STAGE"/README.md "$SRCB"/ 2>/dev/null || true

echo "== systemd template units =="
cp "$STAGE/systemd/imap-drain@.service" /etc/systemd/system/
cp "$STAGE/systemd/imap-drain@.timer"   /etc/systemd/system/
chmod 0644 /etc/systemd/system/imap-drain@.service /etc/systemd/system/imap-drain@.timer

echo "== config dir =="
mkdir -p "$CONFD"
chmod 0750 "$CONFD"

echo "== migrate yahoo-drain instance =="
OLDCONF=/etc/yahoo-drain/drain.conf
NEWCONF=$CONFD/yahoo.conf
if [ -f "$OLDCONF" ] && [ ! -f "$NEWCONF" ]; then
    cp "$OLDCONF" "$OLDCONF.bak-imapdrain-$(date +%Y%m%d-%H%M%S)"
    sed -e 's/^yahoo_host/source_host/' \
        -e 's/^yahoo_port/source_port/' \
        -e 's/^yahoo_user/source_user/' \
        -e 's/^yahoo_passfile/source_passfile/' \
        -e 's/^purelymail_host/target_host/' \
        -e 's/^purelymail_port/target_port/' \
        -e 's/^purelymail_user/target_user/' \
        -e 's/^purelymail_passfile/target_passfile/' \
        "$OLDCONF" > "$NEWCONF"
    # Yahoo advertises its inbox as "Inbox"; make it explicit.
    grep -q '^source_mailbox' "$NEWCONF" || echo 'source_mailbox  = Inbox' >> "$NEWCONF"
    grep -q '^target_mailbox' "$NEWCONF" || echo 'target_mailbox  = INBOX' >> "$NEWCONF"
    chmod 0640 "$NEWCONF"
    echo "converted $OLDCONF -> $NEWCONF"
elif [ -f "$NEWCONF" ]; then
    echo "keeping existing $NEWCONF"
else
    cp "$STAGE/imap-drain.conf.example" "$CONFD/drain.conf.example"
    echo "no old config found; wrote $CONFD/drain.conf.example - edit and copy to <name>.conf"
fi

echo "== cutover: disable the old yahoo-drain timer (kept for rollback) =="
systemctl disable --now yahoo-drain.timer 2>/dev/null || true

echo "== enable the new timer =="
systemctl daemon-reload
if [ -f "$CONFD/yahoo.conf" ]; then
    systemctl enable --now 'imap-drain@yahoo.timer'
    echo "enabled imap-drain@yahoo.timer"
    echo "rollback: systemctl disable --now 'imap-drain@yahoo.timer' && systemctl enable --now yahoo-drain.timer"
else
    echo "no instance configured; enable one with: systemctl enable --now 'imap-drain@<name>.timer'"
fi

echo "done."
