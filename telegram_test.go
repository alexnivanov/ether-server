package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// rsaJWKS отдаёт JWKS с одним публичным RSA-ключом (kid) — как это делает
// endpoint Telegram, но локально, чтобы проверять Verify без сети.
func rsaJWKS(pub *rsa.PublicKey, kid string) string {
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	return fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":%q,"n":%q,"e":%q}]}`, kid, n, e)
}

// POST /auth/telegram: корректно подписанный ID-token → сессия (и она реально
// резолвится); чужая подпись, чужой aud и протухший токен → 401 bad_auth.
func TestAuthTelegram(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const kid = "k1"
	body := rsaJWKS(&key.PublicKey, kid)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer jwks.Close()

	store, err := OpenStore(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	const clientID = "8705267895"
	mux := http.NewServeMux()
	registerREST(mux, store, NewTelegramAuth(clientID, jwks.URL))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sign := func(k *rsa.PrivateKey, aud string, exp time.Time) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, tgClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    tgIssuer,
				Subject:   "777",
				Audience:  jwt.ClaimStrings{aud},
				ExpiresAt: jwt.NewNumericDate(exp),
			},
			PreferredUsername: "alex",
		})
		tok.Header["kid"] = kid
		s, err := tok.SignedString(k)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return s
	}
	post := func(idToken string) (int, map[string]any) {
		resp, m := restPost(t, srv.URL+"/auth/telegram", map[string]string{"id_token": idToken})
		return resp.StatusCode, m
	}

	// валидный токен → сессия
	code, m := post(sign(key, clientID, time.Now().Add(time.Hour)))
	if code != http.StatusOK {
		t.Fatalf("auth(valid) = %d %v, want 200", code, m)
	}
	token, _ := m["token"].(string)
	if token == "" {
		t.Fatalf("auth(valid): пустой token, %v", m)
	}
	if u, _ := m["user"].(map[string]any); u == nil || u["nick"] != "alex" {
		t.Fatalf("auth(valid): user = %v, want nick=alex", m["user"])
	}
	if u, err := store.UserBySession(token); err != nil || u == nil || u.TgID != 777 {
		t.Fatalf("сессия из auth не резолвится: u=%v err=%v", u, err)
	}

	// чужая подпись (другой ключ, тот же kid) → 401
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if code, m = post(sign(other, clientID, time.Now().Add(time.Hour))); code != http.StatusUnauthorized || m["code"] != "bad_auth" {
		t.Fatalf("auth(bad sig) = %d %v, want 401 bad_auth", code, m)
	}
	// чужой aud → 401
	if code, m = post(sign(key, "999", time.Now().Add(time.Hour))); code != http.StatusUnauthorized || m["code"] != "bad_auth" {
		t.Fatalf("auth(bad aud) = %d %v, want 401 bad_auth", code, m)
	}
	// протухший → 401
	if code, m = post(sign(key, clientID, time.Now().Add(-time.Hour))); code != http.StatusUnauthorized || m["code"] != "bad_auth" {
		t.Fatalf("auth(expired) = %d %v, want 401 bad_auth", code, m)
	}
}
