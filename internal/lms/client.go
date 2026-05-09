package lms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/config"
	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/crypto"
)

const (
	lmsBase    = "http://lms.tc.cqupt.edu.cn"
	idsBase    = "https://ids.cqupt.edu.cn"
	userAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	apiVersion = "1.1.0"
)

type Rollcall struct {
	RollcallID   int    `json:"rollcall_id"`
	Source       string `json:"source"`
	Status       string `json:"status"`
	CourseTitle  string `json:"course_title"`
	RollcallTime string `json:"rollcall_time"`
}

type CheckinResult struct {
	Success   bool
	ErrorCode string
}

type Client struct {
	http  *http.Client
	mu    sync.Mutex
	log   *slog.Logger
}

// persistedCookie is used for JSON serialization of cookies.
type persistedCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

func NewClient() *Client {
	jar, _ := cookiejar.New(nil)
	c := &Client{
		http: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // manual redirect handling
			},
		},
		log: slog.With("component", "lms"),
	}
	c.loadCookies()
	return c
}

func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

// Login performs the full IDS login flow and saves cookies on success.
func (c *Client) Login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.login(ctx)
}

func (c *Client) login(ctx context.Context) error {
	c.log.Info("Starting IDS login")

	// Clear cookies
	jar, _ := cookiejar.New(nil)
	c.http.Jar = jar

	// Step 1: GET /login to obtain callback URL
	callbackURL, err := c.getCallbackURL(ctx)
	if err != nil {
		return fmt.Errorf("get callback url: %w", err)
	}

	// Step 2: GET IDS login page to extract salt and execution token
	loginURL := fmt.Sprintf("%s/authserver/login?service=%s", idsBase, url.QueryEscape(callbackURL))
	salt, execution, err := c.getLoginPageParams(ctx, loginURL)
	if err != nil {
		return fmt.Errorf("get login params: %w", err)
	}

	// Step 3: POST login credentials
	encPwd := crypto.EncryptPassword(config.Cfg.Password, salt)
	formData := url.Values{
		"username":  {config.Cfg.Username},
		"password":  {encPwd},
		"captcha":   {""},
		"_eventId":  {"submit"},
		"cllt":      {"userNameLogin"},
		"dllt":      {"generalLogin"},
		"lt":        {""},
		"execution": {execution},
	}

	resp, err := c.doRequest(ctx, "POST", loginURL, "application/x-www-form-urlencoded", strings.NewReader(formData.Encode()))
	if err != nil {
		return fmt.Errorf("submit login: %w", err)
	}
	defer resp.Body.Close()

	// Step 4: Handle session kick ("踢出会话")
	if resp.StatusCode == 200 {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)
		if strings.Contains(bodyStr, "踢出会话") || strings.Contains(bodyStr, "kickout") {
			c.log.Info("Session kick detected, continuing...")
			doc, err := goquery.NewDocumentFromReader(strings.NewReader(bodyStr))
			if err == nil {
				exec2, exists := doc.Find("input[name=execution]").Attr("value")
				if exists {
					formData2 := url.Values{
						"execution": {exec2},
						"_eventId":  {"continue"},
					}
					resp2, err := c.doRequest(ctx, "POST", loginURL, "application/x-www-form-urlencoded", strings.NewReader(formData2.Encode()))
					if err != nil {
						return fmt.Errorf("continue after kick: %w", err)
					}
					resp.Body.Close()
					resp = resp2
				}
			}
		}
	}

	// Step 5: Follow redirects to complete login
	for resp.StatusCode == 302 {
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			break
		}
		resp, err = c.doRequest(ctx, "GET", loc, "", nil)
		if err != nil {
			return fmt.Errorf("follow redirect: %w", err)
		}
	}
	resp.Body.Close()

	// Verify session
	u, _ := url.Parse(lmsBase)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == "session" {
			c.saveCookies()
			c.log.Info("Login successful")
			return nil
		}
	}

	return fmt.Errorf("login failed: session cookie not found")
}

// GetRollcalls fetches active rollcalls from LMS. Re-logins on 302/401.
func (c *Client) GetRollcalls(ctx context.Context) ([]Rollcall, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getRollcalls(ctx, true)
}

func (c *Client) getRollcalls(ctx context.Context, canRetry bool) ([]Rollcall, error) {
	apiURL := fmt.Sprintf("%s/api/radar/rollcalls?api_version=%s", lmsBase, apiVersion)
	resp, err := c.doRequest(ctx, "GET", apiURL, "", nil)
	if err != nil {
		return nil, fmt.Errorf("get rollcalls: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 302 || resp.StatusCode == 401 {
		if canRetry {
			c.log.Info("Session expired, re-logging in")
			if err := c.login(ctx); err != nil {
				return nil, fmt.Errorf("re-login: %w", err)
			}
			return c.getRollcalls(ctx, false)
		}
		return nil, fmt.Errorf("session expired after re-login")
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var result struct {
		Rollcalls []Rollcall `json:"rollcalls"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode rollcalls: %w", err)
	}

	return result.Rollcalls, nil
}

// DoCheckin submits a check-in for a rollcall.
// type_ is "qr", "number", or "radar".
// payload is the check-in data (will have deviceId added).
func (c *Client) DoCheckin(ctx context.Context, rollcallID int, type_ string, payload map[string]interface{}) CheckinResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	var endpoint string
	switch type_ {
	case "qr":
		endpoint = fmt.Sprintf("%s/api/rollcall/%d/answer_qr_rollcall", lmsBase, rollcallID)
	case "number":
		endpoint = fmt.Sprintf("%s/api/rollcall/%d/answer_number_rollcall", lmsBase, rollcallID)
	case "radar":
		endpoint = fmt.Sprintf("%s/api/rollcall/%d/answer", lmsBase, rollcallID)
	default:
		return CheckinResult{false, "unknown type"}
	}

	payload["deviceId"] = config.ClientID
	body, _ := json.Marshal(payload)

	resp, err := c.doRequest(ctx, "PUT", endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		c.log.Error("Checkin request failed", "error", err)
		return CheckinResult{false, err.Error()}
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CheckinResult{false, "decode error"}
	}

	status, _ := result["status"].(string)
	if resp.StatusCode == 200 && status == "on_call" {
		c.log.Info("Checkin successful", "rollcall_id", rollcallID, "type", type_)
		return CheckinResult{true, ""}
	}

	errCode, _ := result["error_code"].(string)
	msg, _ := result["message"].(string)
	errDetail := errCode
	if errDetail == "" {
		errDetail = msg
	}
	c.log.Warn("Checkin failed", "rollcall_id", rollcallID, "type", type_, "error", errDetail)
	return CheckinResult{false, errDetail}
}

// getCallbackURL initiates the login redirect chain to get the CAS callback URL.
func (c *Client) getCallbackURL(ctx context.Context) (string, error) {
	resp, err := c.doRequest(ctx, "GET", lmsBase+"/login", "", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Follow redirects to collect the final callback URL
	currentURL := lmsBase + "/login"
	for resp.StatusCode == 302 {
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			break
		}
		// Resolve relative URLs
		base, _ := url.Parse(currentURL)
		ref, _ := url.Parse(loc)
		resolved := base.ResolveReference(ref).String()
		currentURL = resolved

		resp, err = c.doRequest(ctx, "GET", resolved, "", nil)
		if err != nil {
			return "", err
		}
	}
	resp.Body.Close()

	// The callback URL is the final URL we arrived at
	return currentURL, nil
}

// getLoginPageParams extracts the salt and execution token from the IDS login page.
func (c *Client) getLoginPageParams(ctx context.Context, loginURL string) (salt, execution string, err error) {
	resp, err := c.doRequest(ctx, "GET", loginURL, "", nil)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("parse login page: %w", err)
	}

	salt, _ = doc.Find("#pwdEncryptSalt").Attr("value")
	execution, _ = doc.Find("input[name=execution]").Attr("value")

	if execution == "" {
		return "", "", fmt.Errorf("execution token not found on login page")
	}

	return salt, execution, nil
}

func (c *Client) doRequest(ctx context.Context, method, rawURL, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.http.Do(req)
}

func (c *Client) loadCookies() {
	data, err := os.ReadFile(config.CookiesPath())
	if err != nil {
		return
	}
	var cookies []persistedCookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		return
	}

	u, _ := url.Parse(lmsBase)
	var httpCookies []*http.Cookie
	for _, pc := range cookies {
		httpCookies = append(httpCookies, &http.Cookie{
			Name:   pc.Name,
			Value:  pc.Value,
			Domain: pc.Domain,
			Path:   pc.Path,
		})
	}
	c.http.Jar.SetCookies(u, httpCookies)
	c.log.Info("Loaded saved cookies", "count", len(httpCookies))
}

func (c *Client) saveCookies() {
	u, _ := url.Parse(lmsBase)
	cookies := c.http.Jar.Cookies(u)

	var persisted []persistedCookie
	for _, ck := range cookies {
		persisted = append(persisted, persistedCookie{
			Name:   ck.Name,
			Value:  ck.Value,
			Domain: ck.Domain,
			Path:   ck.Path,
		})
	}

	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		c.log.Warn("Failed to marshal cookies", "error", err)
		return
	}
	if err := os.WriteFile(config.CookiesPath(), data, 0o644); err != nil {
		c.log.Warn("Failed to save cookies", "error", err)
	}
}
