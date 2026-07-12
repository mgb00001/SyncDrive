package auth

import (
	"encoding/json"
	"fmt"

	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
)

// Token persistence via the native OS credential vault:
// Windows Credential Manager (DPAPI-encrypted) on Windows, Secret Service
// (gnome-keyring / KWallet) on Linux. Tokens never touch the SQLite DB or
// plain files.

const vaultService = "SyncDrive"

// SaveToken stores the OAuth token (including the refresh token) for a
// Google account under the OS credential vault.
func SaveToken(accountEmail string, tok *oauth2.Token) error {
	raw, err := json.Marshal(tok)
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}
	if err := keyring.Set(vaultService, accountEmail, string(raw)); err != nil {
		return fmt.Errorf("store token in credential vault: %w", err)
	}
	return nil
}

// LoadToken retrieves a previously stored token for the account.
func LoadToken(accountEmail string) (*oauth2.Token, error) {
	raw, err := keyring.Get(vaultService, accountEmail)
	if err != nil {
		return nil, fmt.Errorf("read token from credential vault: %w", err)
	}
	var tok oauth2.Token
	if err := json.Unmarshal([]byte(raw), &tok); err != nil {
		return nil, fmt.Errorf("unmarshal token: %w", err)
	}
	return &tok, nil
}

// DeleteToken removes the stored token (sign-out).
func DeleteToken(accountEmail string) error {
	return keyring.Delete(vaultService, accountEmail)
}

// persistingTokenSource wraps a TokenSource and writes refreshed tokens back
// to the vault so a rotated refresh token is never lost.
type persistingTokenSource struct {
	account string
	src     oauth2.TokenSource
	last    *oauth2.Token
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		return nil, err
	}
	if p.last == nil || tok.AccessToken != p.last.AccessToken || tok.RefreshToken != p.last.RefreshToken {
		p.last = tok
		if err := SaveToken(p.account, tok); err != nil {
			return nil, err
		}
	}
	return tok, nil
}
