// Command oauth-setup mints an OAuth2 refresh token for imap-drain's
// auth = oauthbearer mode (e.g. Gmail as a source).
//
// One-time flow:
//  1. Create an OAuth client (Desktop app type) in Google Cloud Console;
//     note the client ID and secret.
//  2. Run: oauth-setup -client-id <id> -client-secret <secret>
//  3. Open the printed URL in a browser and approve access.
//  4. Google redirects to a localhost callback; the helper exchanges the
//     code and prints the refresh token.
//  5. Save it (mode 0600) as e.g. /etc/imap-drain/gmail-refresh-token and
//     point source_oauth_refresh_token_file at it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

func main() {
	log.SetFlags(0)
	clientID := flag.String("client-id", "", "OAuth2 client ID (required)")
	clientSecret := flag.String("client-secret", "", "OAuth2 client secret (required)")
	authURL := flag.String("auth-url", "https://accounts.google.com/o/oauth2/v2/auth", "OAuth2 authorization URL")
	tokenURL := flag.String("token-url", "https://oauth2.googleapis.com/token", "OAuth2 token URL")
	scopes := flag.String("scopes", "https://mail.google.com/", "comma-separated OAuth2 scopes")
	flag.Parse()

	if *clientID == "" || *clientSecret == "" {
		flag.Usage()
		os.Exit(2)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	redirectURL := "http://" + ln.Addr().String() + "/callback"

	conf := &oauth2.Config{
		ClientID:     *clientID,
		ClientSecret: *clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       strings.Split(*scopes, ","),
		Endpoint: oauth2.Endpoint{
			AuthURL:  *authURL,
			TokenURL: *tokenURL,
		},
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			fmt.Fprintln(w, "Authorization failed, you can close this tab.")
			errCh <- fmt.Errorf("authorization failed: %s", e)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- fmt.Errorf("no code in callback")
			return
		}
		fmt.Fprintln(w, "Got it — you can close this tab.")
		codeCh <- code
	})}
	go srv.Serve(ln)

	url := conf.AuthCodeURL("imap-drain-setup", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	fmt.Println("Open this URL in your browser:")
	fmt.Println()
	fmt.Println("  " + url)
	fmt.Println()
	fmt.Println("Waiting for the callback...")

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		log.Fatalf("callback: %v", err)
	case <-time.After(5 * time.Minute):
		log.Fatal("timed out waiting for the browser callback")
	}
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := conf.Exchange(ctx, code)
	if err != nil {
		log.Fatalf("token exchange: %v", err)
	}
	if tok.RefreshToken == "" {
		log.Fatal("no refresh token returned (did you approve offline access?)")
	}
	fmt.Println()
	fmt.Println("Refresh token — save with mode 0600, e.g. /etc/imap-drain/gmail-refresh-token:")
	fmt.Println()
	fmt.Println("  " + tok.RefreshToken)
}
