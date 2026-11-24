// Package auth resolves Google accounts (gx registry + Lightpanda metadata)
// and builds the net/http cookie jar used for gemini.google.com requests,
// preferring live cookies from the Lightpanda CDP endpoint and falling back
// to the synced Playwright cookie file.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Account is one selectable Google account.
type Account struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	AuthUser int    `json:"authuser"`
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "~"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// AccountsFile / CookiesFile / RegistryFile expose the resolved paths so the
// CLI can print them in error messages.
func AccountsFile() string {
	return filepath.Join(homeDir(), ".cache", "lightpanda", "google-accounts.json")
}
func CookiesFile() string {
	return filepath.Join(homeDir(), ".cache", "lightpanda", "google-cookies.json")
}
func RegistryFile() string {
	return filepath.Join(homeDir(), ".config", "gx", "accounts.json")
}

// ResolveAccounts merges the gx registry (field "agy" = email) with the
// Lightpanda account metadata (field "authuser"), sorted by authuser.
// Mirrors tools/bin/gemini-chat resolve_accounts.
func ResolveAccounts() ([]Account, error) {
	registryPath := envOr("GX_ACCOUNTS_FILE", RegistryFile())
	metaPath := envOr("LIGHTPANDA_GOOGLE_ACCOUNTS", AccountsFile())

	regRaw, err := os.ReadFile(registryPath)
	if err != nil {
		return nil, fmt.Errorf("gemini-chat: gx registry がありません: %s\n  Chromium :9334 で Google ログイン後 'task lightpanda:sync' を実行してください", registryPath)
	}
	var registry map[string]map[string]any
	if err := json.Unmarshal(regRaw, &registry); err != nil {
		return nil, fmt.Errorf("gemini-chat: gx registry を読めません: %v", err)
	}

	metaRaw, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("gemini-chat: Google アカウント metadata がありません: %s\n  Chromium :9334 で Google ログイン後 'task lightpanda:sync' を実行してください", metaPath)
	}
	var meta struct {
		Accounts []struct {
			Email    string `json:"email"`
			AuthUser int    `json:"authuser"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return nil, fmt.Errorf("gemini-chat: Google アカウント metadata を読めません: %v", err)
	}

	byEmail := map[string]int{}
	for _, a := range meta.Accounts {
		if a.Email != "" {
			byEmail[strings.ToLower(a.Email)] = a.AuthUser
		}
	}

	// Python iterates registry items in JSON insertion order; Go maps lose it,
	// so sort keys to get deterministic behavior.
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)

	var accounts []Account
	for _, name := range names {
		ent := registry[name]
		agy, _ := ent["agy"].(string)
		email := strings.ToLower(agy)
		if agy == "" {
			continue
		}
		if au, ok := byEmail[email]; ok {
			accounts = append(accounts, Account{Name: name, Email: agy, AuthUser: au})
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].AuthUser < accounts[j].AuthUser })
	if len(accounts) == 0 {
		return nil, fmt.Errorf("gemini-chat: metadata と registry を突き合わせられるアカウントがありません")
	}
	return accounts, nil
}

// PickAccount selects an account by name or email, or round-robins by
// authuser using state["last_authuser"]. Mirrors pick_account.
func PickAccount(accounts []Account, explicit string, lastAuthUser string) (Account, error) {
	if explicit != "" {
		for _, acct := range accounts {
			if acct.Name == explicit || strings.EqualFold(acct.Email, explicit) {
				return acct, nil
			}
		}
		names := make([]string, len(accounts))
		for i, a := range accounts {
			names[i] = a.Name
		}
		return Account{}, fmt.Errorf("gemini-chat: unknown account '%s'; available: %s", explicit, strings.Join(names, ", "))
	}
	for i, acct := range accounts {
		if fmt.Sprint(acct.AuthUser) == lastAuthUser {
			return accounts[(i+1)%len(accounts)], nil
		}
	}
	return accounts[0], nil
}

// cdpCookie is the subset of a CDP Storage.getCookies cookie we need.
type cdpCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
}

// jarFromCookies ports jar_from_cookies: builds a cookiejar.Jar and returns
// it along with __Secure-1PSID / __Secure-1PSIDTS values.
func jarFromCookies(cookies []cdpCookie) (*cookiejar.Jar, string, string) {
	jar, _ := cookiejar.New(nil)
	psid, psidts := "", ""
	now := time.Now()
	// cookiejar keys cookies by the request URL host, so group cookies by
	// their domain and register each group under a representative host.
	byDomain := map[string][]*http.Cookie{}
	var domainOrder []string
	for _, c := range cookies {
		if c.Name == "" || c.Value == "" {
			continue
		}
		domain := c.Domain
		if domain == "" {
			domain = ".google.com"
		}
		if !strings.Contains(domain, "google.com") {
			continue
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		var expires time.Time
		if c.Expires > 0 {
			expires = time.Unix(int64(c.Expires), 0)
			if expires.Before(now) {
				continue
			}
		}
		ck := &http.Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Path:     path,
			Domain:   domain,
			Secure:   c.Secure,
			Expires:  expires,
			HttpOnly: c.HTTPOnly,
		}
		if _, seen := byDomain[domain]; !seen {
			domainOrder = append(domainOrder, domain)
		}
		byDomain[domain] = append(byDomain[domain], ck)
		if c.Name == "__Secure-1PSID" {
			psid = c.Value
		} else if c.Name == "__Secure-1PSIDTS" {
			psidts = c.Value
		}
	}
	for _, domain := range domainOrder {
		host := strings.TrimPrefix(domain, ".")
		jar.SetCookies(&url.URL{Scheme: "https", Host: host}, byDomain[domain])
	}
	return jar, psid, psidts
}
func fetchCDPCookies(cdpURL string) []cdpCookie {
	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()

	type result struct {
		cookies []cdpCookie
		ok      bool
	}
	done := make(chan result, 1)
	go func() {
		cookies, ok := cdpCookiesSync(ctx, cdpURL)
		done <- result{cookies, ok}
	}()
	select {
	case r := <-done:
		if r.ok {
			return r.cookies
		}
	case <-ctx.Done():
	}
	return nil
}

func cdpCookiesSync(ctx context.Context, cdpURL string) ([]cdpCookie, bool) {
	httpClient := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cdpURL, "/")+"/json/version", nil)
	if err != nil {
		return nil, false
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var version struct {
		WSURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&version); err != nil || version.WSURL == "" {
		return nil, false
	}

	conn, _, err := websocket.Dial(ctx, version.WSURL, nil)
	if err != nil {
		return nil, false
	}
	defer conn.CloseNow()

	msg, err := json.Marshal(map[string]any{
		"id":     1,
		"method": "Storage.getCookies",
		"params": map[string]any{},
	})
	if err != nil {
		return nil, false
	}
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		return nil, false
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return nil, false
		}
		var reply struct {
			ID     int `json:"id"`
			Error  any `json:"error"`
			Result struct {
				Cookies []cdpCookie `json:"cookies"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &reply); err != nil {
			continue
		}
		if reply.ID != 1 {
			continue
		}
		if reply.Error != nil {
			return nil, false
		}
		var out []cdpCookie
		for _, c := range reply.Result.Cookies {
			if strings.Contains(c.Domain, "google.com") {
				out = append(out, c)
			}
		}
		return out, true
	}
}

// fileCookies parses the Playwright cookie file: either a JSON array of
// cookie objects or {"cookies": [...]}.
func fileCookies(path string) ([]cdpCookie, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []cdpCookie
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Cookies []cdpCookie `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		return wrapped.Cookies, nil
	}
	return nil, fmt.Errorf("unrecognized cookie file format")
}

// LoadCookieJar returns an http.Client carrying the Google cookie jar plus
// the __Secure-1PSID / __Secure-1PSIDTS values. CDP is tried first; the
// synced file is the fallback. Cookie values are never printed.
func LoadCookieJar() (*http.Client, string, string, error) {
	cdp := envOr("GEMINI_CHAT_CDP", "http://127.0.0.1:9334")
	cookiesPath := envOr("LIGHTPANDA_GOOGLE_COOKIES", CookiesFile())

	if cookies := fetchCDPCookies(cdp); len(cookies) > 0 {
		jar, psid, psidts := jarFromCookies(cookies)
		if psid != "" {
			return &http.Client{Jar: jar, Timeout: 5 * time.Minute}, psid, psidts, nil
		}
	}

	fileCookies, err := fileCookies(cookiesPath)
	if err != nil {
		return nil, "", "", fmt.Errorf("gemini-chat: cookie がありません (CDP %s 無応答 / file %s 無し)\n  Chromium :9334 で Google ログイン後 'task lightpanda:sync' を実行してください", cdp, cookiesPath)
	}
	jar, psid, psidts := jarFromCookies(fileCookies)
	if psid == "" {
		return nil, "", "", fmt.Errorf("gemini-chat: __Secure-1PSID がありません: %s\n  Google ログイン後に 'task lightpanda:sync' を実行してください", cookiesPath)
	}
	return &http.Client{Jar: jar, Timeout: 5 * time.Minute}, psid, psidts, nil
}
