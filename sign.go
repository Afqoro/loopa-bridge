package main

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// httpClient is the shared HTTP client for REST API calls
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

// newSignedRequest builds an HTTP request with the standard loopa headers + Bearer auth
func newSignedRequest(method, urlStr string, cfg *Config, body interface{}) (*http.Request, error) {
	req, err := http.NewRequest(method, urlStr, nil)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AccessToken)
	req.Header.Set("Origin", "https://www.loopa.im")
	req.Header.Set("Referer", "https://www.loopa.im/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36")
	return req, nil
}

// generateNonce produces a random 20-char base36 string (same as loopa.im JS generateNonce)
func generateNonce() string {
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 20)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(36))
		b[i] = chars[n.Int64()]
	}
	return string(b)
}

// generateSignature replicates loopa.im's generateSignature from mm0bwkdo2.js
// Algorithm: SHA1(sorted key=value pairs concatenated WITHOUT separator)
// Keys: key (signKey), nonce, timestamp, web_app_key
func generateSignature(timestamp int64, webAppKey, nonce, signKey string) string {
	params := map[string]string{
		"key":         signKey,
		"nonce":       nonce,
		"timestamp":   fmt.Sprintf("%d", timestamp),
		"web_app_key": webAppKey,
	}

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(fmt.Sprintf("%s=%s", k, params[k]))
	}

	h := sha1.Sum([]byte(sb.String()))
	return hex.EncodeToString(h[:])
}

// SignParams holds the signed query parameters for a loopa REST API request
type SignParams struct {
	Timestamp int64
	WebAppKey string
	Nonce     string
	Sign      string
}

// NewSignParams generates a fresh signed parameter set for a REST API call
func NewSignParams(cfg *Config) *SignParams {
	ts := time.Now().Unix()
	nonce := generateNonce()
	sign := generateSignature(ts, cfg.ClientName, nonce, cfg.SignKey)
	return &SignParams{
		Timestamp: ts,
		WebAppKey: cfg.ClientName,
		Nonce:     nonce,
		Sign:      sign,
	}
}

// AppendToURL appends sign params as query string to a base URL
func (s *SignParams) AppendToURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	q := u.Query()
	q.Set("timestamp", fmt.Sprintf("%d", s.Timestamp))
	q.Set("web_app_key", s.WebAppKey)
	q.Set("nonce", s.Nonce)
	q.Set("sign", s.Sign)
	u.RawQuery = q.Encode()
	return u.String()
}

// LoopaPermission holds credit/usage info from /api/auth/permission
type LoopaPermission struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		FreeCredits int `json:"free_credits"`
		Credits     int `json:"credits"`
		Images      int `json:"images"`
		Musics      int `json:"musics"`
		VIP         int `json:"vip"`
	} `json:"data"`
}

// CheckCredit queries loopa.im /api/auth/permission for remaining credits
func CheckCredit(cfg *Config) (*LoopaPermission, error) {
	sign := NewSignParams(cfg)
	urlStr := sign.AppendToURL(cfg.LocalAPIBase + "/loopa-proxy-api/api/auth/permission")

	req, err := newSignedRequest("POST", urlStr, cfg, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("credit check: %w", err)
	}
	defer resp.Body.Close()

	var perm LoopaPermission
	if err := json.NewDecoder(resp.Body).Decode(&perm); err != nil {
		return nil, fmt.Errorf("credit decode: %w", err)
	}
	return &perm, nil
}
