// MIT License
//
// Copyright (c) 2023 TTBT Enterprises LLC
// Copyright (c) 2023 Robin Thellend <rthellend@rthellend.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package oidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
)

// Config contains the parameters of an OIDC provider.
type Config struct {
	// DiscoveryURL is the discovery URL of the OIDC provider. If set, it
	// is used to discover the values of AuthEndpoint and TokenEndpoint.
	DiscoveryURL string
	// AuthEndpoint is the authorization endpoint. It must be set only if
	// DiscoveryURL is not set.
	AuthEndpoint string
	// TokenEndpoint is the token endpoint. It must be set only if
	// DiscoveryURL is not set.
	TokenEndpoint string
	// RedirectURL is the OAUTH2 redirect URL. It must be managed by the
	// proxy.
	RedirectURL string
	// ClientID is the Client ID.
	ClientID string
	// ClientSecret is the Client Secret.
	ClientSecret string
}

type CookieManager interface {
	SetAuthTokenCookie(w http.ResponseWriter, userID, sessionID string) error
	ClearCookies(w http.ResponseWriter) error
}

type EventRecorder interface {
	Record(string)
}

// Provider handles the OIDC manual flow based on information from
// https://developers.google.com/identity/openid-connect/openid-connect and
// https://developers.facebook.com/docs/facebook-login/guides/advanced/oidc-token/
type Provider struct {
	cfg Config
	cm  CookieManager
	er  EventRecorder

	mu     sync.Mutex
	states map[string]*oauthState
}

type oauthState struct {
	Created      time.Time
	OriginalURL  string
	CodeVerifier string
	Seen         bool
}

func New(cfg Config, er EventRecorder, cm CookieManager) (*Provider, error) {
	p := &Provider{
		cfg:    cfg,
		cm:     cm,
		er:     er,
		states: make(map[string]*oauthState),
	}
	if p.cfg.DiscoveryURL != "" {
		resp, err := http.Get(p.cfg.DiscoveryURL)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("http get(%s): %s", cfg.DiscoveryURL, resp.Status)
		}
		var disc struct {
			AuthEndpoint  string `json:"authorization_endpoint"`
			TokenEndpoint string `json:"token_endpoint"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&disc); err != nil {
			return nil, fmt.Errorf("discovery document: %v", err)
		}
		p.cfg.AuthEndpoint = disc.AuthEndpoint
		p.cfg.TokenEndpoint = disc.TokenEndpoint
	}
	if _, err := url.Parse(p.cfg.AuthEndpoint); err != nil {
		return nil, fmt.Errorf("AuthEndpoint: %v", err)
	}
	if _, err := url.Parse(p.cfg.TokenEndpoint); err != nil {
		return nil, fmt.Errorf("TokenEndpoint: %v", err)
	}
	if _, err := url.Parse(p.cfg.RedirectURL); err != nil {
		return nil, fmt.Errorf("RedirectURL: %v", err)
	}
	return p, nil
}

func (p *Provider) RequestLogin(w http.ResponseWriter, req *http.Request, originalURL string) {
	var nonce [12]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nonceStr := hex.EncodeToString(nonce[:])
	var codeVerifier [32]byte
	if _, err := io.ReadFull(rand.Reader, codeVerifier[:]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	codeVerifierStr := base64.RawURLEncoding.EncodeToString(codeVerifier[:])
	cvh := sha256.Sum256([]byte(codeVerifierStr))
	p.mu.Lock()
	p.states[nonceStr] = &oauthState{
		Created:      time.Now(),
		OriginalURL:  originalURL,
		CodeVerifier: codeVerifierStr,
	}
	p.mu.Unlock()
	url := p.cfg.AuthEndpoint + "?" +
		"response_type=code" +
		"&client_id=" + url.QueryEscape(p.cfg.ClientID) +
		"&scope=" + url.QueryEscape("openid email") +
		"&redirect_uri=" + url.QueryEscape(p.cfg.RedirectURL) +
		"&state=" + nonceStr +
		"&nonce=" + nonceStr +
		"&code_challenge=" + base64.RawURLEncoding.EncodeToString(cvh[:]) +
		"&code_challenge_method=S256"
	http.Redirect(w, req, url, http.StatusFound)
	p.er.Record("oidc auth request")
}

func (p *Provider) HandleCallback(w http.ResponseWriter, req *http.Request) {
	p.er.Record("oidc auth callback")
	req.ParseForm()
	if req.Form.Get("logout") != "" {
		p.cm.ClearCookies(w)
		w.Write([]byte("logout successful"))
		return
	}

	p.mu.Lock()
	for k, v := range p.states {
		if time.Since(v.Created) > 5*time.Minute {
			delete(p.states, k)
		}
	}
	state, ok := p.states[req.Form.Get("state")]
	invalid := !ok || state.Seen
	if ok {
		state.Seen = true
	}
	p.mu.Unlock()

	if invalid {
		p.er.Record("invalid state")
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	code := req.Form.Get("code")

	form := url.Values{}
	form.Add("code", code)
	form.Add("client_id", p.cfg.ClientID)
	form.Add("client_secret", p.cfg.ClientSecret)
	form.Add("redirect_uri", p.cfg.RedirectURL)
	form.Add("grant_type", "authorization_code")
	form.Add("code_verifier", state.CodeVerifier)

	resp, err := http.PostForm(p.cfg.TokenEndpoint, form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	var data struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
		Nonce         string `json:"nonce"`
		jwt.RegisteredClaims
	}
	// We received the JWT directly from the identity provider. So, we
	// don't need to validate it.
	if _, _, err := (&jwt.Parser{}).ParseUnverified(data.IDToken, &claims); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.mu.Lock()
	state, ok = p.states[claims.Nonce]
	delete(p.states, claims.Nonce)
	p.mu.Unlock()
	if !ok {
		p.er.Record("invalid nonce")
		http.Error(w, "timeout", http.StatusForbidden)
		return
	}
	if claims.EmailVerified != nil && !*claims.EmailVerified {
		p.er.Record("email not verified")
		http.Error(w, "email not verified", http.StatusForbidden)
		return
	}
	if err := p.cm.SetAuthTokenCookie(w, claims.Email, claims.Nonce); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, req, state.OriginalURL, http.StatusFound)
}
