// Copyright 2014 The oauth2 Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package oauth2 provides support for making
// OAuth2 authorized and authenticated HTTP requests.
// It can additionally grant authorization with Bearer JWT.
package oauth2

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"
)

// Context can be an golang.org/x/net.Context, or an App Engine Context.
// In the future these will be unified.
// If you don't care and aren't running on App Engine, you may use NoContext.
type Context interface{}

// NoContext is the default context. If you're not running this code
// on App Engine or not using golang.org/x/net.Context to provide a custom
// HTTP client, you should use NoContext.
var NoContext Context = nil

// Config describes a typical 3-legged OAuth2 flow, with both the
// client application information and the server's URLs.
type Config struct {
	// ClientID is the application's ID.
	ClientID string

	// ClientSecret is the application's secret.
	ClientSecret string

	// Endpoint contains the resource server's token endpoint
	// URLs.  These are supplied by the server and are often
	// available via site-specific packages (for example,
	// google.Endpoint or github.Endpoint)
	Endpoint Endpoint

	// RedirectURL is the URL to redirect users going through
	// the OAuth flow, after the resource owner's URLs.
	RedirectURL string

	// Scope specifies optional requested permissions.
	Scopes []string
}

// A TokenSource is anything that can return a token.
type TokenSource interface {
	// Token returns a token or an error.
	Token() (*Token, error)
}

// Endpoint contains the OAuth 2.0 provider's authorization and token
// endpoint URLs.
type Endpoint struct {
	AuthURL  string
	TokenURL string
}

// Token represents the crendentials used to authorize
// the requests to access protected resources on the OAuth 2.0
// provider's backend.
//
// Most users of this package should not access fields of Token
// directly. They're exported mostly for use by related packages
// implementing derivate OAuth2 flows.
type Token struct {
	// AccessToken is the token that authorizes and authenticates
	// the requests.
	AccessToken string `json:"access_token"`

	// TokenType is the type of token.
	// The Type method returns either this or "Bearer", the default.
	TokenType string `json:"token_type,omitempty"`

	// RefreshToken is a token that's used by the application
	// (as opposed to the user) to refresh the access token
	// if it expires.
	RefreshToken string `json:"refresh_token,omitempty"`

	// Expiry is the optional expiration time of the access token.
	//
	// If zero, TokenSource implementations will reuse the same
	// token forever and RefreshToken or equivalent
	// mechanisms for that TokenSource will not be used.
	Expiry time.Time `json:"expiry,omitempty"`

	// raw optionally contains extra metadata from the server
	// when updating a token.
	raw interface{}
}

// Type returns t.TokenType if non-empty, else "Bearer".
func (t *Token) Type() string {
	if t.TokenType != "" {
		return t.TokenType
	}
	return "Bearer"
}

// SetAuthHeader sets the Authorization header to r using the access
// token in t.
//
// This method is unnecessary when using Transport or an HTTP Client
// returned by this package.
func (t *Token) SetAuthHeader(r *http.Request) {
	r.Header.Set("Authorization", t.Type()+" "+t.AccessToken)
}

// Extra returns an extra field returned from the server during token
// retrieval.
func (t *Token) Extra(key string) string {
	if vals, ok := t.raw.(url.Values); ok {
		return vals.Get(key)
	}
	if raw, ok := t.raw.(map[string]interface{}); ok {
		if val, ok := raw[key].(string); ok {
			return val
		}
	}
	return ""
}

// Expired returns true if there is no access token or the
// access token is expired.
func (t *Token) Expired() bool {
	if t.AccessToken == "" {
		return true
	}
	if t.Expiry.IsZero() {
		return false
	}
	return t.Expiry.Before(time.Now())
}

var (
	// AccessTypeOnline and AccessTypeOffline are options passed
	// to the Options.AuthCodeURL method. They modify the
	// "access_type" field that gets sent in the URL returned by
	// AuthCodeURL.
	//
	// Online (the default if neither is specified) is the default.
	// If your application needs to refresh access tokens when the
	// user is not present at the browser, then use offline. This
	// will result in your application obtaining a refresh token
	// the first time your application exchanges an authorization
	// code for a user.
	AccessTypeOnline  AuthCodeOption = setParam{"access_type", "online"}
	AccessTypeOffline AuthCodeOption = setParam{"access_type", "offline"}

	// ApprovalForce forces the users to view the consent dialog
	// and confirm the permissions request at the URL returned
	// from AuthCodeURL, even if they've already done so.
	ApprovalForce AuthCodeOption = setParam{"approval_prompt", "force"}
)

type setParam struct{ k, v string }

func (p setParam) setValue(m url.Values) { m.Set(p.k, p.v) }

// An AuthCodeOption is passed to Config.AuthCodeURL.
type AuthCodeOption interface {
	setValue(url.Values)
}

// AuthCodeURL returns a URL to OAuth 2.0 provider's consent page
// that asks for permissions for the required scopes explicitly.
//
// State is a token to protect the user from CSRF attacks. You must
// always provide a non-zero string and validate that it matches the
// the state query parameter on your redirect callback.
// See http://tools.ietf.org/html/rfc6749#section-10.12 for more info.
//
// Opts may include AccessTypeOnline or AccessTypeOffline, as well
// as ApprovalForce.
func (c *Config) AuthCodeURL(state string, opts ...AuthCodeOption) string {
	var buf bytes.Buffer
	buf.WriteString(c.Endpoint.AuthURL)
	v := url.Values{
		"response_type": {"code"},
		"client_id":     {c.ClientID},
		"redirect_uri":  condVal(c.RedirectURL),
		"scope":         condVal(strings.Join(c.Scopes, " ")),
		"state":         condVal(state),
	}
	for _, opt := range opts {
		opt.setValue(v)
	}
	if strings.Contains(c.Endpoint.AuthURL, "?") {
		buf.WriteByte('&')
	} else {
		buf.WriteByte('?')
	}
	buf.WriteString(v.Encode())
	return buf.String()
}

// Exchange converts an authorization code into a token.
//
// It is used after a resource provider redirects the user back
// to the Redirect URI (the URL obtained from AuthCodeURL).
//
// The HTTP client to use is derived from the context. If nil,
// http.DefaultClient is used. See the Context type's documentation.
//
// The code will be in the *http.Request.FormValue("code"). Before
// calling Exchange, be sure to validate FormValue("state").
func (c *Config) Exchange(ctx Context, code string) (*Token, error) {
	return retrieveToken(ctx, c, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": condVal(c.RedirectURL),
		"scope":        condVal(strings.Join(c.Scopes, " ")),
	})
}

// contextClientFunc is a func which tries to return an *http.Client
// given a Context value. If it returns an error, the search stops
// with that error.  If it returns (nil, nil), the search continues
// down the list of registered funcs.
type contextClientFunc func(Context) (*http.Client, error)

var contextClientFuncs []contextClientFunc

func registerContextClientFunc(fn contextClientFunc) {
	contextClientFuncs = append(contextClientFuncs, fn)
}

func contextClient(ctx Context) (*http.Client, error) {
	for _, fn := range contextClientFuncs {
		c, err := fn(ctx)
		if err != nil {
			return nil, err
		}
		if c != nil {
			return c, nil
		}
	}
	if xc, ok := ctx.(context.Context); ok {
		if hc, ok := xc.Value(HTTPClient).(*http.Client); ok {
			return hc, nil
		}
	}
	return http.DefaultClient, nil
}

func contextTransport(ctx Context) http.RoundTripper {
	hc, err := contextClient(ctx)
	if err != nil {
		// This is a rare error case (somebody using nil on App Engine),
		// so I'd rather not everybody do an error check on this Client
		// method. They can get the error that they're doing it wrong
		// later, at client.Get/PostForm time.
		return errorTransport{err}
	}
	return hc.Transport
}

// Client returns an HTTP client using the provided token.
// The token will auto-refresh as necessary. The underlying
// HTTP transport will be obtained using the provided context.
// The returned client and its Transport should not be modified.
func (c *Config) Client(ctx Context, t *Token) *http.Client {
	return NewClient(ctx, c.TokenSource(ctx, t))
}

// TokenSource returns a TokenSource that returns t until t expires,
// automatically refreshing it as necessary using the provided context.
// See the the Context documentation.
//
// Most users will use Config.Client instead.
func (c *Config) TokenSource(ctx Context, t *Token) TokenSource {
	nwn := &newWhenNeededSource{t: t}
	nwn.new = tokenRefresher{
		ctx:      ctx,
		conf:     c,
		oldToken: &nwn.t,
	}
	return nwn
}

// tokenRefresher is a TokenSource that makes "grant_type"=="refresh_token"
// HTTP requests to renew a token using a RefreshToken.
type tokenRefresher struct {
	ctx      Context // used to get HTTP requests
	conf     *Config
	oldToken **Token // pointer to old *Token w/ RefreshToken
}

func (tf tokenRefresher) Token() (*Token, error) {
	t := *tf.oldToken
	if t == nil {
		return nil, errors.New("oauth2: attempted use of nil Token")
	}
	if t.RefreshToken == "" {
		return nil, errors.New("oauth2: token expired and refresh token is not set")
	}
	return retrieveToken(tf.ctx, tf.conf, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.RefreshToken},
	})
}

// newWhenNeededSource is a TokenSource that holds a single token in memory
// and validates its expiry before each call to retrieve it with
// Token. If it's expired, it will be auto-refreshed using the
// new TokenSource.
//
// The first call to TokenRefresher must be SetToken.
type newWhenNeededSource struct {
	new TokenSource // called when t is expired.

	mu sync.Mutex // guards t
	t  *Token
}

// Token returns the current token if it's still valid, else will
// refresh the current token (using r.Context for HTTP client
// information) and return the new one.
func (s *newWhenNeededSource) Token() (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.t != nil && !s.t.Expired() {
		return s.t, nil
	}
	t, err := s.new.Token()
	if err != nil {
		return nil, err
	}
	s.t = t
	return t, nil
}

func retrieveToken(ctx Context, c *Config, v url.Values) (*Token, error) {
	hc, err := contextClient(ctx)
	if err != nil {
		return nil, err
	}
	v.Set("client_id", c.ClientID)
	bustedAuth := !providerAuthHeaderWorks(c.Endpoint.TokenURL)
	if bustedAuth && c.ClientSecret != "" {
		v.Set("client_secret", c.ClientSecret)
	}
	req, err := http.NewRequest("POST", c.Endpoint.TokenURL, strings.NewReader(v.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if !bustedAuth && c.ClientSecret != "" {
		req.SetBasicAuth(c.ClientID, c.ClientSecret)
	}
	r, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	body, err := ioutil.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("oauth2: cannot fetch token: %v", err)
	}
	if code := r.StatusCode; code < 200 || code > 299 {
		return nil, fmt.Errorf("oauth2: cannot fetch token: %v\nResponse: %s", r.Status, body)
	}

	token := &Token{}
	expires := 0
	content, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch content {
	case "application/x-www-form-urlencoded", "text/plain":
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		token.AccessToken = vals.Get("access_token")
		token.TokenType = vals.Get("token_type")
		token.RefreshToken = vals.Get("refresh_token")
		token.raw = vals
		e := vals.Get("expires_in")
		if e == "" {
			// TODO(jbd): Facebook's OAuth2 implementation is broken and
			// returns expires_in field in expires. Remove the fallback to expires,
			// when Facebook fixes their implementation.
			e = vals.Get("expires")
		}
		expires, _ = strconv.Atoi(e)
	default:
		b := make(map[string]interface{}) // TODO: don't use a map[string]interface{}; make a type
		if err = json.Unmarshal(body, &b); err != nil {
			return nil, err
		}
		token.AccessToken, _ = b["access_token"].(string)
		token.TokenType, _ = b["token_type"].(string)
		token.RefreshToken, _ = b["refresh_token"].(string)
		token.raw = b
		e, ok := b["expires_in"].(float64)
		if !ok {
			// TODO(jbd): Facebook's OAuth2 implementation is broken and
			// returns expires_in field in expires. Remove the fallback to expires,
			// when Facebook fixes their implementation.
			e, _ = b["expires"].(float64)
		}
		expires = int(e)
	}
	// Don't overwrite `RefreshToken` with an empty value
	// if this was a token refreshing request.
	if token.RefreshToken == "" {
		token.RefreshToken = v.Get("refresh_token")
	}
	if expires == 0 {
		token.Expiry = time.Time{}
	} else {
		token.Expiry = time.Now().Add(time.Duration(expires) * time.Second)
	}
	return token, nil
}

func condVal(v string) []string {
	if v == "" {
		return nil
	}
	return []string{v}
}

// providerAuthHeaderWorks reports whether the OAuth2 server identified by the tokenURL
// implements the OAuth2 spec correctly
// See https://code.google.com/p/goauth2/issues/detail?id=31 for background.
// In summary:
// - Reddit only accepts client secret in the Authorization header
// - Dropbox accepts either it in URL param or Auth header, but not both.
// - Google only accepts URL param (not spec compliant?), not Auth header
func providerAuthHeaderWorks(tokenURL string) bool {
	if strings.HasPrefix(tokenURL, "https://accounts.google.com/") ||
		strings.HasPrefix(tokenURL, "https://github.com/") ||
		strings.HasPrefix(tokenURL, "https://api.instagram.com/") ||
		strings.HasPrefix(tokenURL, "https://www.douban.com/") ||
		strings.HasPrefix(tokenURL, "https://api.dropbox.com/") ||
		strings.HasPrefix(tokenURL, "https://api.soundcloud.com/") ||
		strings.HasPrefix(tokenURL, "https://www.linkedin.com/") {
		// Some sites fail to implement the OAuth2 spec fully.
		return false
	}

	// Assume the provider implements the spec properly
	// otherwise. We can add more exceptions as they're
	// discovered. We will _not_ be adding configurable hooks
	// to this package to let users select server bugs.
	return true
}

// HTTPClient is the context key to use with golang.org/x/net/context's
// WithValue function to associate an *http.Client value with a context.
var HTTPClient contextKey

// contextKey is just an empty struct. It exists so HTTPClient can be
// an immutable public variable with a unique type. It's immutable
// because nobody else can create a contextKey, being unexported.
type contextKey struct{}

// NewClient creates an *http.Client from a Context and TokenSource.
// The client's lifetime does not extend beyond the lifetime of the context.
func NewClient(ctx Context, src TokenSource) *http.Client {
	return &http.Client{
		Transport: &Transport{
			Base:   contextTransport(ctx),
			Source: src,
		},
	}
}
