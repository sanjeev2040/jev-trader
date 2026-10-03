package kiteauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/config"
)

// Kite Connect endpoints.
const (
	DefaultAPIBaseURL = "https://api.kite.trade"
	DefaultLoginURL   = "https://kite.zerodha.com/connect/login"
)

const (
	kiteVersion     = "3"
	maxResponseSize = 1 << 20
	defaultTimeout  = 30 * time.Second
)

// Client calls the Kite Connect session endpoints. It never places orders.
type Client struct {
	APIKey    config.Secret
	APISecret config.Secret
	// BaseURL defaults to DefaultAPIBaseURL and LoginBaseURL to
	// DefaultLoginURL; tests point them at a fake server.
	BaseURL      string
	LoginBaseURL string
	// HTTPClient defaults to a client with a 30s timeout.
	HTTPClient *http.Client
}

// NewClient returns a Client for the Kite credentials in secrets.
func NewClient(secrets config.Secrets) *Client {
	return &Client{APIKey: secrets.KiteAPIKey, APISecret: secrets.KiteAPISecret}
}

// Profile is the subset of the Kite user profile that kiteauth uses.
type Profile struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
}

// APIError is an error response from Kite Connect.
type APIError struct {
	HTTPStatus int
	// ErrorType is Kite's error_type, such as "TokenException" or
	// "InputException". It is empty when the body was not a Kite error.
	ErrorType string
	Message   string
}

func (e *APIError) Error() string {
	kind := e.ErrorType
	if kind == "" {
		kind = "error"
	}
	return fmt.Sprintf("kite %s (HTTP %d): %s", kind, e.HTTPStatus, e.Message)
}

// IsTokenException reports whether err is a Kite TokenException: the access
// token has expired or was invalidated by a logout. The caller must stop new
// entries and alert; kiteauth never re-logs in on its own.
func IsTokenException(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.ErrorType == "TokenException"
}

// LoginURL returns the Kite login page URL for this API key. Kite passes
// redirectParams back to the registered redirect URL as query parameters.
func (c *Client) LoginURL(redirectParams url.Values) string {
	q := url.Values{"v": {kiteVersion}, "api_key": {c.APIKey.Reveal()}}
	if len(redirectParams) > 0 {
		q.Set("redirect_params", redirectParams.Encode())
	}
	return orDefault(c.LoginBaseURL, DefaultLoginURL) + "?" + q.Encode()
}

// Checksum is the SHA-256 hex digest of apiKey + requestToken + apiSecret that
// Kite requires to exchange a request token.
func Checksum(apiKey, requestToken, apiSecret string) string {
	sum := sha256.Sum256([]byte(apiKey + requestToken + apiSecret))
	return hex.EncodeToString(sum[:])
}

// ExchangeRequestToken trades a one-time request_token from the login
// redirect for an access token.
func (c *Client) ExchangeRequestToken(ctx context.Context, requestToken string) (userID string, accessToken config.Secret, err error) {
	if requestToken == "" {
		return "", config.Secret{}, errors.New("kite: empty request token")
	}
	form := url.Values{
		"api_key":       {c.APIKey.Reveal()},
		"request_token": {requestToken},
		"checksum":      {Checksum(c.APIKey.Reveal(), requestToken, c.APISecret.Reveal())},
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/session/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", config.Secret{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var data struct {
		UserID      string `json:"user_id"`
		AccessToken string `json:"access_token"`
	}
	if err := c.do(req, &data); err != nil {
		return "", config.Secret{}, fmt.Errorf("exchange request token: %w", err)
	}
	if data.AccessToken == "" || data.UserID == "" {
		return "", config.Secret{}, errors.New("exchange request token: response has no access token or user id")
	}
	return data.UserID, config.NewSecret(data.AccessToken), nil
}

// Profile fetches the user profile, which proves that accessToken is live.
func (c *Client) Profile(ctx context.Context, accessToken config.Secret) (Profile, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/user/profile", nil)
	if err != nil {
		return Profile{}, err
	}
	req.Header.Set("Authorization", "token "+c.APIKey.Reveal()+":"+accessToken.Reveal())
	var p Profile
	if err := c.do(req, &p); err != nil {
		return Profile{}, fmt.Errorf("fetch profile: %w", err)
	}
	return p, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, orDefault(c.BaseURL, DefaultAPIBaseURL)+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Kite-Version", kiteVersion)
	return req, nil
}

// do sends req and decodes the "data" field of a successful Kite envelope
// into out. Kite failures become *APIError.
func (c *Client) do(req *http.Request, out any) error {
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return err
	}
	var env struct {
		Status    string          `json:"status"`
		Data      json.RawMessage `json:"data"`
		Message   string          `json:"message"`
		ErrorType string          `json:"error_type"`
	}
	jsonErr := json.Unmarshal(body, &env)
	if resp.StatusCode != http.StatusOK || env.Status != "success" {
		apiErr := &APIError{HTTPStatus: resp.StatusCode, ErrorType: env.ErrorType, Message: env.Message}
		if jsonErr != nil || apiErr.Message == "" {
			apiErr.Message = http.StatusText(resp.StatusCode)
		}
		return apiErr
	}
	if jsonErr != nil {
		return fmt.Errorf("decode kite response: %w", jsonErr)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode kite response data: %w", err)
	}
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
