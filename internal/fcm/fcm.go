// Package fcm sends data messages through Firebase Cloud Messaging's HTTP
// v1 API with a service account, for the rendezvous server to wake phones.
// It signs its own OAuth assertion rather than pulling in Google's client
// libraries.
package fcm

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/vitaminmoo/peerbus/internal/transport"
)

const scope = "https://www.googleapis.com/auth/firebase.messaging"

// Client sends to one Firebase project.
type Client struct {
	project  string
	email    string
	key      *rsa.PrivateKey
	tokenURI string
	// Endpoint is FCM's base URL; tests point it elsewhere.
	Endpoint string
	HTTP     *http.Client

	mu      sync.Mutex
	access  string
	expires time.Time
}

type serviceAccount struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// Load reads a service account key file, as the Firebase console
// downloads it.
func Load(path string) (*Client, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sa serviceAccount
	if err := json.Unmarshal(b, &sa); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil || sa.ProjectID == "" || sa.ClientEmail == "" {
		return nil, fmt.Errorf("%s isn't a service account key", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an RSA key", path)
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &Client{
		project:  sa.ProjectID,
		email:    sa.ClientEmail,
		key:      key,
		tokenURI: sa.TokenURI,
		Endpoint: "https://fcm.googleapis.com",
		HTTP:     &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// assertion is a signed JWT asking for an access token.
func (c *Client) assertion(now time.Time) (string, error) {
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss":   c.email,
		"scope": scope,
		"aud":   c.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	unsigned := b64(head) + "." + b64(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + b64(sig), nil
}

func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.access != "" && now.Before(c.expires) {
		return c.access, nil
	}
	jwt, err := c.assertion(now)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {jwt}}
	req, _ := http.NewRequestWithContext(ctx, "POST", c.tokenURI, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&t) != nil || t.AccessToken == "" {
		return "", fmt.Errorf("fcm: token exchange failed: %s", resp.Status)
	}
	c.access = t.AccessToken
	c.expires = now.Add(time.Duration(t.ExpiresIn)*time.Second - time.Minute)
	return c.access, nil
}

// Push sends data to token as a high-priority data message, which wakes
// the app even when the phone dozes.
func (c *Client) Push(ctx context.Context, token, data string) error {
	access, err := c.token(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"message": map[string]any{
		"token":   token,
		"data":    map[string]string{"p": data},
		"android": map[string]any{"priority": "HIGH", "ttl": "86400s"},
	}})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.Endpoint+"/v1/projects/"+c.project+"/messages:send", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// An app that's gone, or a token it replaced: 404 UNREGISTERED, or
	// 400 for a token that was never valid.
	if resp.StatusCode == http.StatusNotFound || bytes.Contains(msg, []byte("UNREGISTERED")) ||
		(resp.StatusCode == http.StatusBadRequest && bytes.Contains(msg, []byte("registration token"))) {
		return transport.ErrUnregistered
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.mu.Lock()
		c.access = ""
		c.mu.Unlock()
	}
	return errors.New("fcm: " + resp.Status + ": " + strings.TrimSpace(string(msg)))
}
