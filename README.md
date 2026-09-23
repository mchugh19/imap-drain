# imap-drain

Moves mail from one IMAP mailbox to another, a few messages at a time, on a schedule.

Built to migrate a legacy Yahoo mailbox into a personal PurelyMail server,
after mbsync and imapsync proved too slow and too stateful for the job. It
does one thing: drain the source mailbox until it's empty, then keep draining
it on a timer.

## How it works

Each run:

1. Lists the messages in the source mailbox.
2. For each message, searches the target for its Message-ID and skips it if
   it's already there. That counts as a duplicate, not a failure.
3. Copies the message to the target with its original arrival date
   (INTERNALDATE) and its flags, including `\Seen`.
4. Only after the copy is confirmed, flags the source message `\Deleted`.
5. Expunges deleted messages from the source.

Messages already flagged `\Deleted` on the source are expunged without being
copied. The flag means an earlier run already delivered them.

There is no local database and no watermark. Every run is independent: if
one fails halfway, the next run picks up where it left off. Failed messages
are retried, never silently dropped.

## Configuration

One file per instance, e.g. `/etc/imap-drain/yahoo.conf`:

```ini
source_host     = imap.mail.yahoo.com
source_port     = 993
source_user     = user@yahoo.com
source_passfile = /etc/imap-drain/yahoo.pass
source_mailbox  = Inbox
source_auth     = login

target_host     = mail.purelymail.com
target_port     = 993
target_user     = user@purelymail.com
target_passfile = /etc/imap-drain/purelymail.pass
target_mailbox  = INBOX
target_auth     = login

state_dir     = /var/lib/imap-drain
dial_timeout  = 20s
total_timeout = 90s
```

There are no built-in credentials: the drain refuses to run unless every
required field is set. `source_mailbox` must match the exact spelling the
server advertises. Yahoo calls it `Inbox`, not `INBOX`.

`source_auth` / `target_auth` are `login` (the default) or `oauthbearer`.
Both sides are independent: one end can use a password while the other uses
OAuth2, and either side can be Gmail.

## OAuth2 (Gmail and others)

Password LOGIN doesn't exist on Gmail. With `oauthbearer`, each run
exchanges a refresh token for a fresh access token and authenticates with
SASL OAUTHBEARER (RFC 7628). The short-lived token is never written to disk.

One-time setup:

1. Create an OAuth client (Desktop app type) in Google Cloud Console. Note
   the client ID and secret.
2. Run the helper on any machine with a browser:

   ```sh
   oauth-setup -client-id <id> -client-secret <secret>
   ```

   Open the printed URL, approve access. The helper prints a refresh token.
3. Save the refresh token with mode 0600, e.g.
   `/etc/imap-drain/gmail-refresh-token`, and point the config at it:

   ```ini
   source_auth                     = oauthbearer
   source_oauth_client_id          = xxxxx.apps.googleusercontent.com
   source_oauth_client_secret_file = /etc/imap-drain/gmail-client-secret
   source_oauth_refresh_token_file = /etc/imap-drain/gmail-refresh-token
   source_oauth_token_url          = https://oauth2.googleapis.com/token
   ```

## Running it

Build:

```sh
cd src && go vet ./... && go test ./... && go build -o imap-drain .
```

`INSTALL.sh` installs the binary to `/usr/local/bin` and template systemd units
(`imap-drain@.service` / `imap-drain@.timer`) that read
`/etc/imap-drain/<name>.conf` and run every 2 minutes. One timer per
mailbox pair.

Check a config by hand before enabling its timer:

```sh
imap-drain -config /etc/imap-drain/yahoo.conf
```

## Requirements

- Go 1.26+ to build
- Two IMAP accounts, TLS on 993 (or wherever yours listen)
- Dependencies are fetched by `go build`:
  `github.com/emersion/go-imap/v2`, `golang.org/x/oauth2`
