// Package auth implements the Google OAuth2 desktop "loopback" flow and
// secure token storage in the native OS credential vault.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// DriveScope grants access to Drive files. Per the spec this is the full
// drive scope; swap for drive.file if staying under Google's restricted-scope
// certification threshold.
const DriveScope = "https://www.googleapis.com/auth/drive"

// userinfo scope lets us resolve which email just authenticated.
const emailScope = "https://www.googleapis.com/auth/userinfo.email"

// LoadClientConfig reads the OAuth client secrets JSON exported from the
// Google Cloud Console (Desktop Application credential type).
func LoadClientConfig(secretsPath string) (*oauth2.Config, error) {
	raw, err := os.ReadFile(secretsPath)
	if err != nil {
		return nil, fmt.Errorf("read client secrets: %w", err)
	}
	cfg, err := google.ConfigFromJSON(raw, DriveScope, emailScope)
	if err != nil {
		return nil, fmt.Errorf("parse client secrets: %w", err)
	}
	return cfg, nil
}

// Authenticate runs the loopback-redirect flow:
//  1. bind an ephemeral listener on 127.0.0.1,
//  2. open the consent URL in the system browser,
//  3. capture the authorization code on the redirect,
//  4. exchange it for tokens and persist them in the OS credential vault.
//
// Returns the authenticated account's email address.
func Authenticate(ctx context.Context, cfg *oauth2.Config) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("bind loopback listener: %w", err)
	}
	defer ln.Close()

	local := *cfg // shallow copy so we can set the per-run redirect URL
	local.RedirectURL = fmt.Sprintf("http://%s/callback", ln.Addr().String())

	state, err := randomState()
	if err != nil {
		return "", err
	}
	verifier := oauth2.GenerateVerifier()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth state mismatch")
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, "authorization declined", http.StatusBadRequest)
			errCh <- fmt.Errorf("authorization declined: %s", e)
			return
		}
		fmt.Fprint(w, "<html><body><h2>SyncDrive is connected.</h2>You can close this tab and return to the app.</body></html>")
		codeCh <- q.Get("code")
	})}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := local.AuthCodeURL(state,
		oauth2.AccessTypeOffline,             // request a refresh token
		oauth2.ApprovalForce,                 // re-issue refresh token on re-auth
		oauth2.S256ChallengeOption(verifier)) // PKCE
	if err := openBrowser(authURL); err != nil {
		return "", fmt.Errorf("open browser: %w", err)
	}

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(5 * time.Minute):
		return "", fmt.Errorf("authentication timed out")
	}

	tok, err := local.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return "", fmt.Errorf("exchange authorization code: %w", err)
	}

	email, err := fetchEmail(ctx, &local, tok)
	if err != nil {
		return "", err
	}
	if err := SaveToken(email, tok); err != nil {
		return "", err
	}
	return email, nil
}

// TokenSource returns an auto-refreshing, vault-persisting token source for
// a previously authenticated account.
func TokenSource(ctx context.Context, cfg *oauth2.Config, accountEmail string) (oauth2.TokenSource, error) {
	tok, err := LoadToken(accountEmail)
	if err != nil {
		return nil, err
	}
	return &persistingTokenSource{
		account: accountEmail,
		src:     cfg.TokenSource(ctx, tok),
		last:    tok,
	}, nil
}

func fetchEmail(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (string, error) {
	client := cfg.Client(ctx, tok)
	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return "", fmt.Errorf("fetch userinfo: %w", err)
	}
	defer resp.Body.Close()
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode userinfo: %w", err)
	}
	if info.Email == "" {
		return "", fmt.Errorf("userinfo response contained no email")
	}
	return info.Email, nil
}

func randomState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// openBrowser launches the system default browser without shell tricks
// (no hidden windows — keeps AV heuristics happy).
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
