package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

const exampleConf = `# one instance per source server; copy to /etc/imap-drain/<name>.conf
source_host     = imap.mail.yahoo.com
source_port     = 993
source_user     = mchugh19@yahoo.com
source_passfile = /etc/mbsync/yahoo.pass
source_mailbox  = Inbox
target_host     = imap.purelymail.com
target_port     = 993
target_user     = mmchugh@silverapple.eu
target_passfile = /etc/mbsync/purelymail.pass
target_mailbox  = INBOX
`

func TestLoadExampleConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "yahoo.conf")
	if err := os.WriteFile(p, []byte(exampleConf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SourceUser != "mchugh19@yahoo.com" {
		t.Fatalf("source_user = %q", cfg.SourceUser)
	}
	if cfg.TargetUser != "mmchugh@silverapple.eu" {
		t.Fatalf("target_user = %q", cfg.TargetUser)
	}
	if cfg.SourceMailbox != "Inbox" || cfg.TargetMailbox != "INBOX" {
		t.Fatalf("mailboxes = %q/%q", cfg.SourceMailbox, cfg.TargetMailbox)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidateRequiresFields(t *testing.T) {
	cfg := defaultConfig()
	if err := cfg.validate(); err == nil {
		t.Fatal("expected validation error for empty hosts, got nil")
	}
	cfg.SourceHost, cfg.SourceUser, cfg.SourcePassFile = "a", "b", "c"
	cfg.TargetHost, cfg.TargetUser, cfg.TargetPassFile = "d", "e", "f"
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidateOAuthBearer(t *testing.T) {
	cfg := defaultConfig()
	cfg.SourceHost, cfg.SourceUser = "imap.gmail.com", "user@gmail.com"
	cfg.SourceAuth = "oauthbearer"
	if err := cfg.validate(); err == nil {
		t.Fatal("expected validation error for incomplete oauth config, got nil")
	}
	cfg.SourceOAuth.ClientID = "id"
	cfg.SourceOAuth.ClientSecretFile = "/etc/imap-drain/gmail-secret"
	cfg.SourceOAuth.RefreshTokenFile = "/etc/imap-drain/gmail-refresh"
	cfg.TargetHost, cfg.TargetUser, cfg.TargetPassFile = "d", "e", "f"
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidateBadAuth(t *testing.T) {
	cfg := defaultConfig()
	cfg.SourceHost, cfg.SourceUser, cfg.SourcePassFile = "a", "b", "c"
	cfg.TargetHost, cfg.TargetUser, cfg.TargetPassFile = "d", "e", "f"
	cfg.SourceAuth = "magic"
	if err := cfg.validate(); err == nil {
		t.Fatal("expected validation error for unknown auth, got nil")
	}
}

func TestLoadConfigBadKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.conf")
	if err := os.WriteFile(p, []byte("bogus_key = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(p); err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
}

func TestLoadConfigMissingFallsBack(t *testing.T) {
	cfg, err := loadConfig("/nonexistent/path/drain.conf")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SourceMailbox != "INBOX" || cfg.TargetMailbox != "INBOX" {
		t.Fatalf("expected INBOX defaults, got %+v", cfg)
	}
}

func TestLockName(t *testing.T) {
	if got := lockName("/etc/imap-drain/yahoo.conf"); got != "yahoo.lock" {
		t.Fatalf("lockName = %q", got)
	}
	if got := lockName("/etc/imap-drain/drain.conf"); got != "drain.lock" {
		t.Fatalf("lockName = %q", got)
	}
}

func TestHeaderValue(t *testing.T) {
	raw := []byte("From: Alice <alice@example.com>\r\n" +
		"Subject: hello\r\n\tworld\r\n" +
		"Message-ID: <abc123@example.com>\r\n" +
		"\r\nbody here")
	if got := headerValue(raw, "Message-ID"); got != "<abc123@example.com>" {
		t.Fatalf("Message-ID = %q", got)
	}
	if got := headerValue(raw, "subject"); got != "hello world" {
		t.Fatalf("Subject unfolded = %q", got)
	}
	if got := headerValue(raw, "X-Missing"); got != "" {
		t.Fatalf("X-Missing = %q, want empty", got)
	}
}

func TestSafeForSearch(t *testing.T) {
	if !safeForSearch("<abc123@example.com>") {
		t.Fatal("normal message-id should be safe")
	}
	for _, bad := range []string{"", `a"b`, `a\b`, "a\rb", "a\nb"} {
		if safeForSearch(bad) {
			t.Fatalf("%q should not be safe", bad)
		}
	}
}

func TestAppendFlags(t *testing.T) {
	in := []imap.Flag{imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDraft, imap.FlagDeleted, "\\Recent"}
	out := appendFlags(in)
	joined := strings.Join([]string{string(out[0]), string(out[1]), string(out[2]), string(out[3])}, ",")
	if len(out) != 4 || strings.Contains(joined, "Deleted") || strings.Contains(joined, "Recent") {
		t.Fatalf("appendFlags = %q", out)
	}
}

func TestParseMailboxPairs(t *testing.T) {
	pairs, err := parseMailboxPairs("Inbox -> INBOX, Spam -> Yahoo-Quarantine")
	if err != nil {
		t.Fatalf("parseMailboxPairs: %v", err)
	}
	want := [][2]string{{"Inbox", "INBOX"}, {"Spam", "Yahoo-Quarantine"}}
	if len(pairs) != len(want) {
		t.Fatalf("pairs = %v, want %v", pairs, want)
	}
	for i := range want {
		if pairs[i] != want[i] {
			t.Fatalf("pairs[%d] = %v, want %v", i, pairs[i], want[i])
		}
	}
}

func TestParseMailboxPairsSingle(t *testing.T) {
	pairs, err := parseMailboxPairs("Inbox->INBOX")
	if err != nil {
		t.Fatalf("parseMailboxPairs: %v", err)
	}
	if len(pairs) != 1 || pairs[0] != [2]string{"Inbox", "INBOX"} {
		t.Fatalf("pairs = %v", pairs)
	}
}

func TestParseMailboxPairsBad(t *testing.T) {
	for _, bad := range []string{
		"",
		"   ",
		"Inbox",          // no arrow
		"Inbox -> ",      // empty target
		" -> INBOX",      // empty source
		"Inbox, Spam -> S", // first entry malformed
	} {
		if _, err := parseMailboxPairs(bad); err == nil {
			t.Fatalf("parseMailboxPairs(%q): expected error, got nil", bad)
		}
	}
}

func TestLoadConfigMailboxPairsDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "yahoo.conf")
	if err := os.WriteFile(p, []byte(exampleConf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.MailboxPairs) != 1 || cfg.MailboxPairs[0] != [2]string{"Inbox", "INBOX"} {
		t.Fatalf("default MailboxPairs = %v", cfg.MailboxPairs)
	}
}

func TestLoadConfigMailboxPairs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "yahoo.conf")
	conf := exampleConf + "mailbox_pairs = Inbox -> INBOX, Spam -> Yahoo-Quarantine, Bulk -> Old-Bulk\n"
	if err := os.WriteFile(p, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := [][2]string{{"Inbox", "INBOX"}, {"Spam", "Yahoo-Quarantine"}, {"Bulk", "Old-Bulk"}}
	if len(cfg.MailboxPairs) != len(want) {
		t.Fatalf("MailboxPairs = %v, want %v", cfg.MailboxPairs, want)
	}
	for i := range want {
		if cfg.MailboxPairs[i] != want[i] {
			t.Fatalf("MailboxPairs[%d] = %v, want %v", i, cfg.MailboxPairs[i], want[i])
		}
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
