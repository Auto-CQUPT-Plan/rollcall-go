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
	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/notify"
)

const (
	lmsBase    = "http://lms.tc.cqupt.edu.cn"
	idsBase    = "https://ids.cqupt.edu.cn"
	userAgent  = "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/86.0.4240.198 Safari/537.36"
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

type StudentRollcall struct {
	Status string `json:"status"`
}

type StudentRollcallsData struct {
	IsNumber     bool              `json:"is_number"`
	NumberCode   int               `json:"number_code"`
	RollcallList []StudentRollcall `json:"student_rollcalls"`
}

type Client struct {
	http *http.Client
	mu   sync.Mutex
	log  *slog.Logger
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
				return http.ErrUseLastResponse
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
	c.log.Info("正在登录 IDS")

	// Clear cookies for fresh login
	jar, _ := cookiejar.New(nil)
	c.http.Jar = jar

	// Step 1: Follow up to 2 redirects from /login to get callback URL
	// (matches Python: for _ in range(2): resp = send(req); req = resp.next_request)
	callbackURL, err := c.getCallbackURL(ctx)
	if err != nil {
		return fmt.Errorf("get callback url: %w", err)
	}

	// Step 2: GET IDS login page to extract salt and execution token
	// Python uses: params={"service": str(callback_url)} which httpx appends as query param
	idsLoginURL := idsBase + "/authserver/login"
	salt, execution, err := c.getLoginPageParams(ctx, idsLoginURL+"?service="+url.QueryEscape(callbackURL))
	if err != nil {
		return fmt.Errorf("get login params: %w", err)
	}

	// Step 3: POST login credentials
	// Python POSTs to: ids/authserver/login?service=callback_url
	postURL := idsLoginURL + "?service=" + url.QueryEscape(callbackURL)
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

	resp, err := c.doRequest(ctx, "POST", postURL, "application/x-www-form-urlencoded", strings.NewReader(formData.Encode()))
	if err != nil {
		return fmt.Errorf("submit login: %w", err)
	}

	// Step 4: Handle session kick ("踢出会话") or get redirect URL
	var redirectURL string
	if resp.StatusCode == 302 {
		redirectURL = resp.Header.Get("Location")
		resp.Body.Close()
	} else if resp.StatusCode == 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodyStr := string(body)
		if strings.Contains(bodyStr, "踢出会话") || strings.Contains(bodyStr, "kickout") {
			c.log.Info("检测到会话冲突，正在继续...")
			doc, err := goquery.NewDocumentFromReader(strings.NewReader(bodyStr))
			if err == nil {
				if exec2, exists := doc.Find("input[name=execution]").Attr("value"); exists {
					formData2 := url.Values{
						"execution": {exec2},
						"_eventId":  {"continue"},
					}
					resp2, err := c.doRequest(ctx, "POST", postURL, "application/x-www-form-urlencoded", strings.NewReader(formData2.Encode()))
					if err != nil {
						return fmt.Errorf("continue after kick: %w", err)
					}
					if resp2.StatusCode == 302 {
						redirectURL = resp2.Header.Get("Location")
					}
					resp2.Body.Close()
				}
			}
		}
	} else {
		resp.Body.Close()
	}

	// Step 5: Follow redirect with full redirect support (matching Python follow_redirects=True)
	if redirectURL != "" {
		if err := c.followAllRedirects(ctx, redirectURL); err != nil {
			return fmt.Errorf("follow login redirect: %w", err)
		}
	}

	// Verify session cookie on LMS domain
	u, _ := url.Parse(lmsBase)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == "session" {
			c.saveCookies()
			c.log.Info("IDS 登录成功")
			notify.Send("🔑 IDS 登录成功")
			return nil
		}
	}

	return fmt.Errorf("login failed: session cookie not found")
}

// followAllRedirects follows ALL redirect types (301/302/303/307/308) until a non-redirect response.
// This matches Python's follow_redirects=True behavior.
func (c *Client) followAllRedirects(ctx context.Context, startURL string) error {
	currentURL := startURL
	for i := 0; i < 20; i++ { // max 20 redirects to prevent infinite loops
		resp, err := c.doRequest(ctx, "GET", currentURL, "", nil)
		if err != nil {
			return err
		}
		resp.Body.Close()

		if !isRedirect(resp.StatusCode) {
			return nil
		}

		loc := resp.Header.Get("Location")
		if loc == "" {
			return nil
		}

		// Resolve relative URLs
		base, _ := url.Parse(currentURL)
		ref, _ := url.Parse(loc)
		currentURL = base.ResolveReference(ref).String()
	}
	return nil
}

func isRedirect(statusCode int) bool {
	return statusCode == 301 || statusCode == 302 || statusCode == 303 ||
		statusCode == 307 || statusCode == 308
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
			c.log.Info("会话已过期，正在重新登录")
			if err := c.login(ctx); err != nil {
				return nil, fmt.Errorf("re-login: %w", err)
			}
			return c.getRollcalls(ctx, false)
		}
		// Match Python: return empty list instead of error on second failure
		c.log.Warn("重新登录后会话仍然无效")
		return []Rollcall{}, nil
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
		c.log.Error("签到请求失败", "error", err)
		return CheckinResult{false, err.Error()}
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CheckinResult{false, "decode error"}
	}

	status, _ := result["status"].(string)
	if resp.StatusCode == 200 && status == "on_call" {
		c.log.Info("签到成功", "rollcall_id", rollcallID, "type", type_)
		return CheckinResult{true, ""}
	}

	errCode, _ := result["error_code"].(string)
	msg, _ := result["message"].(string)
	errDetail := errCode
	if errDetail == "" {
		errDetail = msg
	}
	c.log.Warn("签到失败", "rollcall_id", rollcallID, "type", type_, "error", errDetail)
	return CheckinResult{false, errDetail}
}

// GetStudentRollcalls fetches student rollcalls details for a rollcall. Re-logins on 302/401.
func (c *Client) GetStudentRollcalls(ctx context.Context, rollcallID int) (*StudentRollcallsData, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getStudentRollcalls(ctx, rollcallID, true)
}

func (c *Client) getStudentRollcalls(ctx context.Context, rollcallID int, canRetry bool) (*StudentRollcallsData, error) {
	apiURL := fmt.Sprintf("%s/api/rollcall/%d/student_rollcalls", lmsBase, rollcallID)
	resp, err := c.doRequest(ctx, "GET", apiURL, "", nil)
	if err != nil {
		return nil, fmt.Errorf("get student rollcalls: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 302 || resp.StatusCode == 401 {
		if canRetry {
			c.log.Info("会话已过期，正在重新登录")
			if err := c.login(ctx); err != nil {
				return nil, fmt.Errorf("re-login: %w", err)
			}
			return c.getStudentRollcalls(ctx, rollcallID, false)
		}
		// Match Python: return nil instead of error on second failure
		c.log.Warn("重新登录后会话仍然无效")
		return nil, nil
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var result StudentRollcallsData
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode student rollcalls: %w", err)
	}

	return &result, nil
}

// getCallbackURL follows up to 2 redirect hops from /login, matching Python's behavior.
func (c *Client) getCallbackURL(ctx context.Context) (string, error) {
	currentURL := lmsBase + "/login"

	for i := 0; i < 2; i++ {
		resp, err := c.doRequest(ctx, "GET", currentURL, "", nil)
		if err != nil {
			return "", err
		}
		resp.Body.Close()

		if !isRedirect(resp.StatusCode) {
			break
		}

		loc := resp.Header.Get("Location")
		if loc == "" {
			break
		}

		// Resolve relative URLs
		base, _ := url.Parse(currentURL)
		ref, _ := url.Parse(loc)
		currentURL = base.ResolveReference(ref).String()
	}

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
	c.log.Info("已加载保存的 Cookie", "数量", len(httpCookies))
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
		c.log.Warn("Cookie 序列化失败", "error", err)
		return
	}
	if err := os.WriteFile(config.CookiesPath(), data, 0o644); err != nil {
		c.log.Warn("Cookie 保存失败", "error", err)
	}
}
