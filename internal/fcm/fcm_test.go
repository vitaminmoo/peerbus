package fcm

import (
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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vitaminmoo/peerbus/internal/transport"
)

// fakeGoogle checks the service account's assertion and records pushes.
func fakeGoogle(t *testing.T, pub *rsa.PublicKey) (*httptest.Server, func() []map[string]any) {
	var mu sync.Mutex
	var sent []map[string]any
	exchanges := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		parts := strings.Split(r.Form.Get("assertion"), ".")
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || len(parts) != 3 {
			http.Error(w, "bad grant", 400)
			return
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
			http.Error(w, "bad signature", 400)
			return
		}
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var c map[string]any
		json.Unmarshal(claims, &c)
		if c["iss"] != "pusher@proj.iam.gserviceaccount.com" || c["scope"] != scope {
			http.Error(w, "bad claims", 400)
			return
		}
		mu.Lock()
		exchanges++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	})
	mux.HandleFunc("POST /v1/projects/proj/messages:send", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", 401)
			return
		}
		var body struct {
			Message map[string]any `json:"message"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Message["token"] == "gone" {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`))
			return
		}
		mu.Lock()
		sent = append(sent, body.Message)
		mu.Unlock()
		w.Write([]byte(`{"name":"projects/proj/messages/1"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		if exchanges != 1 {
			t.Errorf("%d token exchanges, want 1 (cached)", exchanges)
		}
		return sent
	}
}

func TestPush(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	srv, sent := fakeGoogle(t, &key.PublicKey)
	sa, _ := json.Marshal(serviceAccount{
		ProjectID:   "proj",
		ClientEmail: "pusher@proj.iam.gserviceaccount.com",
		PrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		TokenURI:    srv.URL + "/token",
	})
	path := filepath.Join(t.TempDir(), "sa.json")
	os.WriteFile(path, sa, 0o600)

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Endpoint = srv.URL
	ctx := context.Background()
	for range 2 {
		if err := c.Push(ctx, "phone-token", "sealed"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Push(ctx, "gone", "sealed"); !errors.Is(err, transport.ErrUnregistered) {
		t.Fatalf("unregistered token: %v", err)
	}
	got := sent()
	if len(got) != 2 {
		t.Fatalf("sent %d", len(got))
	}
	m := got[0]
	if m["token"] != "phone-token" || m["data"].(map[string]any)["p"] != "sealed" || m["android"].(map[string]any)["priority"] != "HIGH" {
		t.Fatalf("message: %v", m)
	}
}
