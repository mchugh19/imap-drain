// Command imap-drain drains IMAP source folders into target IMAP folders,
// deleting each source message only after it is confirmed on the
// target. Each run drains every "source -> target" pair in mailbox_pairs
// (default: the single source_mailbox -> target_mailbox pair).
//
// It is meant to run every couple of minutes from a systemd timer (the
// imap-drain@.service template takes an instance name; each instance reads
// /etc/imap-drain/<name>.conf). Each run opens one short IMAP connection to
// the source and SELECTs the source mailbox. If the mailbox is empty it
// exits having done nothing else -- no connection to the target, no state
// file, no spawned processes.
//
// When messages are present it drains them one by one:
//  1. FETCH the full message bytes, flags and internal date from the source.
//  2. Ask the target (one indexed SEARCH per message) whether a message
//     with the same Message-ID is already there. If yes it is a duplicate:
//     skip the copy. If no (or the message has no usable Message-ID),
//     APPEND the bytes to the target preserving flags and internal date,
//     so the original arrival date survives the move.
//  3. Only after the APPEND succeeded (or the duplicate was confirmed) is
//     the source copy flagged \Deleted. A single EXPUNGE at the end removes
//     exactly those messages.
//
// The safety rule is ordering, not bookkeeping: nothing is deleted from the
// source before it is confirmed on the target. Any failure leaves the message
// in the source mailbox and the next tick retries it. There is no watermark
// file to go stale and no full-folder header scan: the per-run cost is
// proportional to the handful of messages actually waiting, not to the
// thousands already archived on the target.
//
// A message already flagged \Deleted on the source is treated as delivered
// by an earlier run whose EXPUNGE did not complete: it is re-queued for
// expunge without being copied again. This keeps Message-ID-less mail from
// being duplicated if a run dies between flagging and expunging.
//
// A lock file prevents overlapping runs. Logging goes to stdout/stderr
// (captured by the systemd journal).
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"golang.org/x/oauth2"
)

const defaultConfigPath = "/etc/imap-drain/drain.conf"

type Config struct {
	SourceHost      string
	SourcePort      string
	SourceUser      string
	SourcePassFile  string // for auth = login
	SourceMailbox   string
	SourceAuth      string // "login" (default) or "oauthbearer"
	SourceOAuth     OAuthConfig
	TargetHost      string
	TargetPort      string
	TargetUser      string
	TargetPassFile  string // for auth = login
	TargetMailbox   string
	TargetAuth      string // "login" (default) or "oauthbearer"
	TargetOAuth     OAuthConfig
	// MailboxPairs lists the source -> target folder pairs drained each run,
	// in order. When the mailbox_pairs key is absent it defaults to the
	// single pair {SourceMailbox, TargetMailbox}.
	MailboxPairs    [][2]string
	StateDir        string // lock file location
	DialTimeout     time.Duration
	TotalTimeout    time.Duration
}

// OAuthConfig holds the pieces needed to mint an access token from a
// long-lived refresh token. Used when auth = oauthbearer (e.g. Gmail,
// where password LOGIN is not available).
type OAuthConfig struct {
	ClientID       string
	ClientSecretFile string
	RefreshTokenFile string
	TokenURL       string
	AuthURL        string // only used by the oauth-setup helper, not the drain
}

func defaultConfig() *Config {
	return &Config{
		SourcePort:    "993",
		SourceMailbox: "INBOX",
		SourceAuth:    "login",
		SourceOAuth: OAuthConfig{
			TokenURL: "https://oauth2.googleapis.com/token",
			AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		},
		TargetPort:    "993",
		TargetMailbox: "INBOX",
		TargetAuth:    "login",
		TargetOAuth: OAuthConfig{
			TokenURL: "https://oauth2.googleapis.com/token",
			AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		},
		StateDir:      "/var/lib/imap-drain",
		DialTimeout:   20 * time.Second,
		TotalTimeout:  90 * time.Second,
	}
}

// loadConfig reads a "key = value" file. Unknown keys are an error.
func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("bad config line (want key = value): %q", line)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		setDuration := func(d *time.Duration) error {
			parsed, err := time.ParseDuration(val)
			if err != nil {
				return fmt.Errorf("bad duration for %q: %w", key, err)
			}
			*d = parsed
			return nil
		}
		switch key {
		case "source_host":
			cfg.SourceHost = val
		case "source_port":
			cfg.SourcePort = val
		case "source_user":
			cfg.SourceUser = val
		case "source_passfile":
			cfg.SourcePassFile = val
		case "source_mailbox":
			cfg.SourceMailbox = val
		case "source_auth":
			cfg.SourceAuth = strings.ToLower(val)
		case "source_oauth_client_id":
			cfg.SourceOAuth.ClientID = val
		case "source_oauth_client_secret_file":
			cfg.SourceOAuth.ClientSecretFile = val
		case "source_oauth_refresh_token_file":
			cfg.SourceOAuth.RefreshTokenFile = val
		case "source_oauth_token_url":
			cfg.SourceOAuth.TokenURL = val
		case "target_host":
			cfg.TargetHost = val
		case "target_port":
			cfg.TargetPort = val
		case "target_user":
			cfg.TargetUser = val
		case "target_passfile":
			cfg.TargetPassFile = val
		case "target_mailbox":
			cfg.TargetMailbox = val
		case "mailbox_pairs":
			pairs, err := parseMailboxPairs(val)
			if err != nil {
				return nil, err
			}
			cfg.MailboxPairs = pairs
		case "target_auth":
			cfg.TargetAuth = strings.ToLower(val)
		case "target_oauth_client_id":
			cfg.TargetOAuth.ClientID = val
		case "target_oauth_client_secret_file":
			cfg.TargetOAuth.ClientSecretFile = val
		case "target_oauth_refresh_token_file":
			cfg.TargetOAuth.RefreshTokenFile = val
		case "target_oauth_token_url":
			cfg.TargetOAuth.TokenURL = val
		case "state_dir":
			cfg.StateDir = val
		case "dial_timeout":
			if err := setDuration(&cfg.DialTimeout); err != nil {
				return nil, err
			}
		case "total_timeout":
			if err := setDuration(&cfg.TotalTimeout); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown config key %q", key)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(cfg.MailboxPairs) == 0 {
		cfg.MailboxPairs = [][2]string{{cfg.SourceMailbox, cfg.TargetMailbox}}
	}
	return cfg, nil
}

// parseMailboxPairs parses a comma-separated list of "source -> target"
// folder pairs, e.g. "Inbox -> INBOX, Spam -> Yahoo-Quarantine".
// Arbitrary length: every pair is drained each run, in order.
func parseMailboxPairs(val string) ([][2]string, error) {
	var pairs [][2]string
	for _, part := range strings.Split(val, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		src, dst, ok := strings.Cut(part, "->")
		if !ok {
			return nil, fmt.Errorf("bad mailbox pair %q (want \"source -> target\")", part)
		}
		src, dst = strings.TrimSpace(src), strings.TrimSpace(dst)
		if src == "" || dst == "" {
			return nil, fmt.Errorf("bad mailbox pair %q (want \"source -> target\")", part)
		}
		pairs = append(pairs, [2]string{src, dst})
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("mailbox_pairs is empty")
	}
	return pairs, nil
}

// validate reports missing required fields.
func (cfg *Config) validate() error {
	for _, kv := range [][2]string{
		{"source_host", cfg.SourceHost},
		{"source_user", cfg.SourceUser},
		{"source_mailbox", cfg.SourceMailbox},
		{"target_host", cfg.TargetHost},
		{"target_user", cfg.TargetUser},
		{"target_mailbox", cfg.TargetMailbox},
	} {
		if kv[1] == "" {
			return fmt.Errorf("%s is required", kv[0])
		}
	}
	if err := validateSide("source", cfg.SourceAuth, cfg.SourcePassFile, &cfg.SourceOAuth); err != nil {
		return err
	}
	if err := validateSide("target", cfg.TargetAuth, cfg.TargetPassFile, &cfg.TargetOAuth); err != nil {
		return err
	}
	return nil
}

func validateSide(side, auth, passfile string, oauth *OAuthConfig) error {
	switch auth {
	case "login":
		if passfile == "" {
			return fmt.Errorf("%s_passfile is required for auth = login", side)
		}
	case "oauthbearer":
		for _, kv := range [][2]string{
			{side + "_oauth_client_id", oauth.ClientID},
			{side + "_oauth_client_secret_file", oauth.ClientSecretFile},
			{side + "_oauth_refresh_token_file", oauth.RefreshTokenFile},
			{side + "_oauth_token_url", oauth.TokenURL},
		} {
			if kv[1] == "" {
				return fmt.Errorf("%s is required for auth = oauthbearer", kv[0])
			}
		}
	default:
		return fmt.Errorf("%s_auth = %q: want \"login\" or \"oauthbearer\"", side, auth)
	}
	return nil
}

// accessToken exchanges the refresh token for a fresh access token.
// Called once per run; the token is short-lived and never persisted.
func (o *OAuthConfig) accessToken(ctx context.Context) (string, error) {
	secret, err := readPassFile(o.ClientSecretFile)
	if err != nil {
		return "", err
	}
	refresh, err := readPassFile(o.RefreshTokenFile)
	if err != nil {
		return "", err
	}
	conf := &oauth2.Config{
		ClientID:     o.ClientID,
		ClientSecret: secret,
		Endpoint:     oauth2.Endpoint{TokenURL: o.TokenURL},
	}
	tok, err := conf.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh}).Token()
	if err != nil {
		return "", fmt.Errorf("oauth token refresh: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("oauth token refresh returned no access token")
	}
	return tok.AccessToken, nil
}

func readPassFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open passfile: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return "", fmt.Errorf("passfile %s is empty", path)
	}
	pass := strings.TrimSpace(sc.Text())
	if pass == "" {
		return "", fmt.Errorf("passfile %s is empty", path)
	}
	return pass, nil
}

type imapConn struct {
	client *imapclient.Client
	conn   net.Conn
	done   chan struct{}
}

type credentials struct {
	user   string
	method string // "login" or "oauthbearer"
	pass   string // for login
	oauth  *OAuthConfig
}

func dialIMAP(host, port string, cred credentials, cfg *Config) (*imapConn, error) {
	addr := net.JoinHostPort(host, port)
	dialer := &net.Dialer{Timeout: cfg.DialTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	ic := &imapConn{client: imapclient.New(conn, nil), conn: conn, done: make(chan struct{})}
	// Failsafe: never hang longer than totalTimeout even if a server stalls.
	go func() {
		select {
		case <-ic.done:
		case <-time.After(cfg.TotalTimeout):
			conn.Close()
		}
	}()
	switch cred.method {
	case "oauthbearer":
		ctx, cancel := context.WithTimeout(context.Background(), cfg.DialTimeout)
		token, tokErr := cred.oauth.accessToken(ctx)
		cancel()
		if tokErr != nil {
			ic.Close()
			return nil, fmt.Errorf("oauth token for %s@%s: %w", cred.user, host, tokErr)
		}
		portNum, _ := strconv.Atoi(port)
		if err := ic.client.Authenticate(sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: cred.user,
			Token:    token,
			Host:     host,
			Port:     portNum,
		})); err != nil {
			ic.Close()
			return nil, fmt.Errorf("oauthbearer %s@%s: %w", cred.user, host, err)
		}
	default: // "login"
		if err := ic.client.Login(cred.user, cred.pass).Wait(); err != nil {
			ic.Close()
			return nil, fmt.Errorf("login %s@%s: %w", cred.user, host, err)
		}
	}
	return ic, nil
}

func (ic *imapConn) Close() {
	close(ic.done)
	_ = ic.client.Logout().Wait()
	ic.client.Close()
}

// headerValue returns the unfolded value of a header field from raw message
// bytes, or "" if absent. Case-insensitive on the field name.
func headerValue(raw []byte, name string) string {
	head := raw
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		head = raw[:i]
	}
	// Unfold: continuation lines start with space or tab.
	unfolded := bytes.ReplaceAll(head, []byte("\r\n "), []byte(" "))
	unfolded = bytes.ReplaceAll(unfolded, []byte("\r\n\t"), []byte(" "))
	want := strings.ToLower(name) + ":"
	for _, line := range bytes.Split(unfolded, []byte("\r\n")) {
		if len(line) > len(want) && strings.ToLower(string(line[:len(want)])) == want {
			return strings.TrimSpace(string(line[len(want):]))
		}
	}
	return ""
}

// safeForSearch reports whether s can be embedded in a SEARCH HEADER
// criterion without risking IMAP quoting issues.
func safeForSearch(s string) bool {
	if s == "" {
		return false
	}
	return !strings.ContainsAny(s, "\"\\\r\n")
}

// appendFlags maps source flags to APPEND-able flags. \Deleted is never
// carried over (the source copy is deleted explicitly after delivery) and
// \Recent cannot be set by APPEND.
func appendFlags(src []imap.Flag) []imap.Flag {
	var out []imap.Flag
	for _, f := range src {
		switch f {
		case imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDraft:
			out = append(out, f)
		}
	}
	return out
}

// buildCredentials reads the password file for auth = login; for
// auth = oauthbearer the token is minted fresh at dial time, so nothing
// secret is read here.
func buildCredentials(user, method, passfile string, oauth *OAuthConfig) (credentials, error) {
	cred := credentials{user: user, method: method, oauth: oauth}
	if method == "login" {
		pass, err := readPassFile(passfile)
		if err != nil {
			return cred, err
		}
		cred.pass = pass
	}
	return cred, nil
}

func numSetEmpty(ns imap.NumSet) bool {
	switch s := ns.(type) {
	case imap.UIDSet:
		return len(s) == 0
	case imap.SeqSet:
		return len(s) == 0
	}
	return true
}

func drain(cfg *Config) error {
	srcCred, err := buildCredentials(cfg.SourceUser, cfg.SourceAuth, cfg.SourcePassFile, &cfg.SourceOAuth)
	if err != nil {
		return fmt.Errorf("source credentials: %w", err)
	}
	src, err := dialIMAP(cfg.SourceHost, cfg.SourcePort, srcCred, cfg)
	if err != nil {
		return err
	}
	defer src.Close()

	// The target connection is opened lazily: if every source folder is
	// empty the run touches only the source.
	var dst *imapConn
	defer func() {
		if dst != nil {
			dst.Close()
		}
	}()

	var totalDrained, totalDupes, totalFailed int
	for _, pair := range cfg.MailboxPairs {
		drained, dupes, failed, err := drainPair(src, &dst, cfg, pair[0], pair[1])
		if err != nil {
			return err
		}
		totalDrained += drained
		totalDupes += dupes
		totalFailed += failed
	}
	log.Printf("drain done: %d copied, %d duplicates, %d failed", totalDrained, totalDupes, totalFailed)
	return nil
}

// drainPair drains one source folder into one target folder. dstConn is a
// target connection shared across pairs, dialed on first use.
func drainPair(src *imapConn, dstConn **imapConn, cfg *Config, srcBox, dstBox string) (drained, dupes, failed int, err error) {
	sel, err := src.client.Select(srcBox, nil).Wait()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("source select %q: %w", srcBox, err)
	}
	if sel.NumMessages == 0 {
		log.Printf("[%s -> %s] source mailbox empty, nothing to drain", srcBox, dstBox)
		return 0, 0, 0, nil
	}
	log.Printf("[%s -> %s] %d message(s), draining", srcBox, dstBox, sel.NumMessages)

	fetchCmd := src.client.Fetch(
		imap.SeqSet{{Start: 1, Stop: sel.NumMessages}},
		&imap.FetchOptions{
			UID:          true,
			Flags:        true,
			InternalDate: true,
			BodySection: []*imap.FetchItemBodySection{
				{Specifier: imap.PartSpecifierNone, Peek: true}, // BODY.PEEK[]
			},
		},
	)
	msgs, err := fetchCmd.Collect()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("source fetch: %w", err)
	}

	if *dstConn == nil {
		dstCred, err := buildCredentials(cfg.TargetUser, cfg.TargetAuth, cfg.TargetPassFile, &cfg.TargetOAuth)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("target credentials: %w", err)
		}
		d, err := dialIMAP(cfg.TargetHost, cfg.TargetPort, dstCred, cfg)
		if err != nil {
			return 0, 0, 0, err
		}
		*dstConn = d
	}
	dst := *dstConn
	if _, err := dst.client.Select(dstBox, nil).Wait(); err != nil {
		return 0, 0, 0, fmt.Errorf("target select %q: %w", dstBox, err)
	}

	var toDelete imap.UIDSet
	for _, m := range msgs {
		if len(m.BodySection) == 0 {
			log.Printf("source uid %d: no body fetched, leaving for next tick", m.UID)
			failed++
			continue
		}
		// Already flagged \Deleted by an earlier run whose EXPUNGE did not
		// complete. The flag is only ever set after confirmed delivery, so
		// re-queue it for expunge without copying it again.
		alreadyGone := false
		for _, f := range m.Flags {
			if f == imap.FlagDeleted {
				alreadyGone = true
				break
			}
		}
		if alreadyGone {
			log.Printf("uid %d: already flagged deleted, will expunge", m.UID)
			toDelete = append(toDelete, imap.UIDRange{Start: m.UID, Stop: m.UID})
			continue
		}
		body := m.BodySection[0].Bytes
		subj := headerValue(body, "Subject")
		from := headerValue(body, "From")
		msgID := headerValue(body, "Message-ID")

		deliver := true
		if safeForSearch(msgID) {
			sr, err := dst.client.Search(&imap.SearchCriteria{
				Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: msgID}},
			}, nil).Wait()
			if err != nil {
				log.Printf("uid %d (%q): target search failed: %v, leaving for next tick", m.UID, subj, err)
				failed++
				continue
			}
			if !numSetEmpty(sr.All) {
				log.Printf("uid %d (%q from %q): already on target, skipping copy", m.UID, subj, from)
				dupes++
				deliver = false
			}
		}

		if deliver {
			ac := dst.client.Append(dstBox, int64(len(body)), &imap.AppendOptions{
				Flags: appendFlags(m.Flags),
				Time:  m.InternalDate,
			})
			if _, err := ac.Write(body); err != nil {
				ac.Close()
				log.Printf("uid %d (%q): append write failed: %v, leaving for next tick", m.UID, subj, err)
				failed++
				continue
			}
			if err := ac.Close(); err != nil {
				log.Printf("uid %d (%q): append close failed: %v, leaving for next tick", m.UID, subj, err)
				failed++
				continue
			}
			if _, err := ac.Wait(); err != nil {
				log.Printf("uid %d (%q): append failed: %v, leaving for next tick", m.UID, subj, err)
				failed++
				continue
			}
			log.Printf("uid %d (%q from %q): copied to target", m.UID, subj, from)
			drained++
		}

		// Delivered (copied or confirmed duplicate): safe to delete the
		// source copy. This only runs after confirmed delivery above.
		toDelete = append(toDelete, imap.UIDRange{Start: m.UID, Stop: m.UID})
	}

	if len(toDelete) > 0 {
		sc := src.client.Store(toDelete, &imap.StoreFlags{
			Op:     imap.StoreFlagsAdd,
			Silent: true,
			Flags:  []imap.Flag{imap.FlagDeleted},
		}, nil)
		if err := sc.Close(); err != nil {
			return 0, 0, 0, fmt.Errorf("source flag deleted: %w", err)
		}
		if _, err := src.client.Expunge().Collect(); err != nil {
			return 0, 0, 0, fmt.Errorf("source expunge: %w", err)
		}
		log.Printf("[%s -> %s] deleted %d message(s) from source", srcBox, dstBox, len(toDelete))
	}
	log.Printf("[%s -> %s] done: %d copied, %d duplicates, %d failed", srcBox, dstBox, drained, dupes, failed)
	return drained, dupes, failed, nil
}

// lockName derives a per-instance lock name from the config path so
// multiple instances (imap-drain@foo, imap-drain@bar) do not block each
// other.
func lockName(configPath string) string {
	base := strings.TrimSuffix(filepath.Base(configPath), filepath.Ext(configPath))
	if base == "" || base == "." {
		base = "drain"
	}
	return base + ".lock"
}

func main() {
	log.SetFlags(log.LstdFlags)
	configPath := flag.String("config", defaultConfigPath, "path to drain config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if _, err := os.Stat(*configPath); err == nil {
		log.Printf("using config %s", *configPath)
	} else {
		log.Printf("config %s not found, using built-in defaults", *configPath)
	}
	if err := cfg.validate(); err != nil {
		log.Fatalf("config: %v", err)
	}

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		log.Fatalf("state dir: %v", err)
	}
	lockPath := filepath.Join(cfg.StateDir, lockName(*configPath))
	lockF, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		log.Fatalf("lock: %v", err)
	}
	if err := syscall.Flock(int(lockF.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Printf("another drain run is active, exiting")
		os.Exit(0)
	}
	defer func() {
		syscall.Flock(int(lockF.Fd()), syscall.LOCK_UN)
		lockF.Close()
	}()

	if err := drain(cfg); err != nil {
		log.Fatalf("drain: %v", err)
	}
}
