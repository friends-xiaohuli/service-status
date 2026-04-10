package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ========== 配置结构 ==========

type ServiceConfig struct {
	Name     string `json:"name"`
	Subtitle string `json:"subtitle,omitempty"`
	Type     string `json:"type"` // http | tcp | ping | mysql | napcat_qq
	Group    string `json:"group,omitempty"`
	URL      string `json:"url"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Timeout  int    `json:"timeout"`
	UserID   string `json:"user_id,omitempty"`
	Token    string `json:"token,omitempty"`
	Insecure bool   `json:"insecure"`          // true = 跳过 TLS 证书校验（自签名/过期证书）
	Display  *bool  `json:"display,omitempty"` // nil/true = 展示，false = 隐藏
	Enable   *bool  `json:"enable,omitempty"`  // nil/true = 启用检测，false = 停用且不记录
}

type ServiceGroupConfig struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Display     *bool           `json:"display,omitempty"` // nil/true = 展示，false = 隐藏
	Enable      *bool           `json:"enable,omitempty"`  // nil/true = 启用组内检测，false = 停用且不记录
	Services    []ServiceConfig `json:"services"`
}

type Config struct {
	Port          string               `json:"port"`
	HTTPSPort     string               `json:"https_port"`
	TLSCert       string               `json:"tls_cert"`
	TLSKey        string               `json:"tls_key"`
	Services      []ServiceConfig      `json:"services,omitempty"`
	ServiceGroups []ServiceGroupConfig `json:"service_groups,omitempty"`
}

// ========== 状态结构 ==========

type ServiceStatus struct {
	Key          string         `json:"key,omitempty"`
	Name         string         `json:"name"`
	Subtitle     string         `json:"subtitle,omitempty"`
	Type         string         `json:"type"`
	Group        string         `json:"group,omitempty"`
	Status       string         `json:"status"`
	ResponseTime int64          `json:"response_time"`
	Message      string         `json:"message"`
	Meta         string         `json:"meta,omitempty"`
	CheckedAt    string         `json:"checked_at"`
	Uptime       float64        `json:"uptime"`
	CheckCount   int            `json:"check_count"`
	Visible      bool           `json:"visible"`
	Uptime24BP   int            `json:"uptime_24_bp,omitempty"`
	Uptime7dBP   int            `json:"uptime_7d_bp,omitempty"`
	Uptime30dBP  int            `json:"uptime_30d_bp,omitempty"`
	History      []HistoryEntry `json:"history,omitempty"`
}

// ========== 可用率 & 历史追踪 ==========

const (
	configFileName          = "config.json"
	webDirName              = "web"
	historyDBFileName       = "status-history.db"
	apiRefreshInterval      = 1 * time.Minute
	schedulerTickInterval   = 1 * time.Minute
	retryRefreshInterval    = 2 * time.Minute
	stableRefreshInterval   = 5 * time.Minute
	configWatchInterval     = 2 * time.Second
	memoryHistoryRetention  = 24 * time.Hour
	persistentHistoryWindow = 30 * 24 * time.Hour
)

var east8Location = time.FixedZone("UTC+8", 8*60*60)

type HistoryEntry struct {
	Status       string `json:"status"`
	ResponseTime int64  `json:"response_time"`
	Time         string `json:"time"`
	Ts           int64  `json:"ts"` // Unix 时间戳（秒）
}

type serviceHistory struct {
	mu      sync.Mutex
	entries []HistoryEntry
}

func (h *serviceHistory) record(e HistoryEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, e)
	h.pruneLocked(e.Ts - int64(memoryHistoryRetention/time.Second))
}

func (h *serviceHistory) replace(entries []HistoryEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append([]HistoryEntry(nil), entries...)
	h.pruneLocked(time.Now().Add(-memoryHistoryRetention).Unix())
}

func (h *serviceHistory) pruneLocked(cutoff int64) {
	firstValid := 0
	for firstValid < len(h.entries) && h.entries[firstValid].Ts < cutoff {
		firstValid++
	}
	if firstValid > 0 {
		h.entries = append([]HistoryEntry(nil), h.entries[firstValid:]...)
	}
}

func (h *serviceHistory) snapshot() (entries []HistoryEntry, uptime float64, total int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	total = len(h.entries)
	if total == 0 {
		return nil, 0, 0
	}
	entries = make([]HistoryEntry, total)
	copy(entries, h.entries)
	success := 0
	for _, e := range h.entries {
		if e.Status == "online" {
			success++
		}
	}
	uptime = float64(success) / float64(total) * 100
	return
}

type APIResponse struct {
	RefreshedAt int64           `json:"refreshed_at"` // Unix 时间戳（秒）
	Interval    int             `json:"interval"`     // 刷新间隔（秒）
	Services    []ServiceStatus `json:"services"`
}

type compactSummaryResponse struct {
	V int     `json:"v"`
	T int64   `json:"t"`
	I int     `json:"i"`
	S [][]any `json:"s,omitempty"`
}

type compactHistoryResponse struct {
	V int     `json:"v"`
	T int64   `json:"t"`
	R string  `json:"r"`
	S int     `json:"s"`
	H [][]any `json:"h,omitempty"`
}

var (
	config        Config
	configLock    sync.RWMutex
	refreshLock   sync.Mutex
	statusLock    sync.RWMutex
	lastStatus    []ServiceStatus
	lastRefreshed int64
	historyMap    = map[string]*serviceHistory{}
	historyLock   sync.Mutex
	historyDB     *historyStore
	probeState    = map[string]probeRuntimeState{}
	probeStateMu  sync.Mutex
	napCatAuthMu  sync.Mutex
	napCatTokens  = map[string]string{}
)

type probeRuntimeState struct {
	LastChecked int64
	LastStatus  string
}

func getHistory(key string) *serviceHistory {
	historyLock.Lock()
	defer historyLock.Unlock()
	if h, ok := historyMap[key]; ok {
		return h
	}
	h := &serviceHistory{}
	historyMap[key] = h
	return h
}

func nextProbeInterval(status string) time.Duration {
	if status == "online" {
		return stableRefreshInterval
	}
	return retryRefreshInterval
}

func shouldCheckServiceNow(key string, now time.Time) bool {
	probeStateMu.Lock()
	defer probeStateMu.Unlock()

	state, ok := probeState[key]
	if !ok || state.LastChecked <= 0 {
		return true
	}
	lastChecked := time.Unix(state.LastChecked, 0)
	return now.Sub(lastChecked) >= nextProbeInterval(state.LastStatus)
}

func updateProbeState(key string, checkedAt time.Time, status string) {
	probeStateMu.Lock()
	probeState[key] = probeRuntimeState{
		LastChecked: checkedAt.Unix(),
		LastStatus:  status,
	}
	probeStateMu.Unlock()
}

func pruneProbeState(validKeys []string) {
	allowed := make(map[string]struct{}, len(validKeys))
	for _, key := range validKeys {
		allowed[key] = struct{}{}
	}

	probeStateMu.Lock()
	for key := range probeState {
		if _, ok := allowed[key]; !ok {
			delete(probeState, key)
		}
	}
	probeStateMu.Unlock()
}

func getLastStatusMap() map[string]ServiceStatus {
	statusLock.RLock()
	defer statusLock.RUnlock()

	result := make(map[string]ServiceStatus, len(lastStatus))
	for _, svc := range lastStatus {
		result[svc.Key] = svc
	}
	return result
}

func resolveRuntimePath(name string) string {
	if name == "" {
		return ""
	}
	if _, err := os.Stat(name); err == nil {
		return name
	}
	exePath, err := os.Executable()
	if err != nil {
		return name
	}
	exeDir := filepath.Dir(exePath)
	fallback := filepath.Join(exeDir, name)
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	return fallback
}

func resolveWritablePath(name string) string {
	if name == "" {
		return ""
	}
	if filepath.IsAbs(name) {
		return name
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, name)
	}
	exePath, err := os.Executable()
	if err != nil {
		return name
	}
	return filepath.Join(filepath.Dir(exePath), name)
}

func serviceKey(cfg ServiceConfig) string {
	keyMaterial := legacyServiceKey(cfg)
	sum := sha256.Sum256([]byte(keyMaterial))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:10])
	return strings.ToLower(encoded)
}

func legacyServiceKey(cfg ServiceConfig) string {
	parts := []string{
		strings.TrimSpace(cfg.Group),
		strings.TrimSpace(cfg.Name),
		strings.TrimSpace(cfg.Type),
		strings.TrimSpace(cfg.URL),
		strings.TrimSpace(cfg.Host),
		strconv.Itoa(cfg.Port),
		strings.TrimSpace(cfg.UserID),
	}
	return strings.Join(parts, "\x1f")
}

func serviceKeyCandidates(cfg ServiceConfig) []string {
	canonical := serviceKey(cfg)
	legacy := legacyServiceKey(cfg)
	if canonical == legacy {
		return []string{canonical}
	}
	return []string{canonical, legacy}
}

func east8Time(t time.Time) time.Time {
	return t.In(east8Location)
}

func formatHistoryTime(t time.Time) string {
	return east8Time(t).Format("01-02 15:04")
}

func formatStorageDisplayTime(t time.Time) string {
	t = east8Time(t)
	return fmt.Sprintf("%s:%02d", t.Format("2006-01-02 15:04:05"), t.Nanosecond()/1e7)
}

func formatNapCatGeneratedAtDisplay(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return east8Time(t).Format("01-02 15:04")
		}
	}

	return raw
}

func parseNapCatData(payload map[string]any) map[string]any {
	value, ok := payload["data"]
	if !ok {
		return nil
	}
	result, _ := value.(map[string]any)
	return result
}

func parseNapCatUptimeFormatted(payload map[string]any) string {
	data := parseNapCatData(payload)
	if data == nil {
		return ""
	}
	text, _ := data["uptimeFormatted"].(string)
	return strings.TrimSpace(text)
}

func parseNapCatCode(payload map[string]any) (int64, bool) {
	value, ok := payload["code"]
	if !ok {
		return 0, false
	}
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		parsed, err := v.Int64()
		if err == nil {
			return parsed, true
		}
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func parseNapCatMessage(payload map[string]any) string {
	value, ok := payload["message"]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func isNapCatUnauthorized(payload map[string]any) bool {
	code, ok := parseNapCatCode(payload)
	if !ok || code != -1 {
		return false
	}
	return strings.EqualFold(parseNapCatMessage(payload), "Unauthorized")
}

type napCatSessionEnvelope struct {
	Data struct {
		CreatedTime int64  `json:"CreatedTime"`
		HashEncoded string `json:"HashEncoded"`
	} `json:"Data"`
	Hmac string `json:"Hmac"`
}

func napCatSessionCacheKey(cfg ServiceConfig, rawToken string) string {
	host := strings.TrimSpace(cfg.Host)
	port := cfg.Port
	if port == 0 {
		port = 6099
	}
	return fmt.Sprintf("%s:%d|%s", host, port, rawToken)
}

func getCachedNapCatToken(key string) string {
	napCatAuthMu.Lock()
	defer napCatAuthMu.Unlock()
	return napCatTokens[key]
}

func setCachedNapCatToken(key, token string) {
	napCatAuthMu.Lock()
	defer napCatAuthMu.Unlock()
	if strings.TrimSpace(token) == "" {
		delete(napCatTokens, key)
		return
	}
	napCatTokens[key] = token
}

func isNapCatSessionToken(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}

	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return false
	}

	var envelope napCatSessionEnvelope
	if err := json.Unmarshal(decoded, &envelope); err != nil {
		return false
	}
	return envelope.Data.CreatedTime > 0 &&
		strings.TrimSpace(envelope.Data.HashEncoded) != "" &&
		strings.TrimSpace(envelope.Hmac) != ""
}

func decodeNapCatSessionToken(token string) (napCatSessionEnvelope, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return napCatSessionEnvelope{}, false
	}
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return napCatSessionEnvelope{}, false
	}
	var envelope napCatSessionEnvelope
	if err := json.Unmarshal(decoded, &envelope); err != nil {
		return napCatSessionEnvelope{}, false
	}
	if envelope.Data.CreatedTime <= 0 || strings.TrimSpace(envelope.Data.HashEncoded) == "" || strings.TrimSpace(envelope.Hmac) == "" {
		return napCatSessionEnvelope{}, false
	}
	return envelope, true
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", sum[:])
}

func napCatLoginHash(token string) string {
	return sha256Hex(strings.TrimSpace(token) + ".napcat")
}

func isHexSHA256(text string) bool {
	text = strings.TrimSpace(text)
	if len(text) != 64 {
		return false
	}
	for _, ch := range text {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') && (ch < 'A' || ch > 'F') {
			return false
		}
	}
	return true
}

func extractNapCatTokenValue(value any) string {
	switch v := value.(type) {
	case string:
		v = strings.TrimSpace(v)
		if isNapCatSessionToken(v) {
			return v
		}
	case map[string]any:
		for _, key := range []string{"token", "Token", "access_token", "accessToken", "authorization", "Authorization"} {
			if token := extractNapCatTokenValue(v[key]); token != "" {
				return token
			}
		}
		for _, nested := range v {
			if token := extractNapCatTokenValue(nested); token != "" {
				return token
			}
		}
	case []any:
		for _, nested := range v {
			if token := extractNapCatTokenValue(nested); token != "" {
				return token
			}
		}
	}
	return ""
}

func sanitizeNapCatPreview(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 240 {
		text = text[:240] + "..."
	}
	return text
}

func extractNapCatTokenFromCookies(cookies []*http.Cookie) string {
	for _, cookie := range cookies {
		if cookie == nil {
			continue
		}
		if token := extractNapCatTokenValue(cookie.Value); token != "" {
			return token
		}
	}
	return ""
}

func extractNapCatTokenFromHeaders(header http.Header) string {
	for _, key := range []string{"Authorization", "X-Token", "Token", "Access-Token"} {
		for _, value := range header.Values(key) {
			value = strings.TrimSpace(value)
			value = strings.TrimPrefix(value, "Bearer ")
			value = strings.TrimSpace(value)
			if token := extractNapCatTokenValue(value); token != "" {
				return token
			}
		}
	}
	return ""
}

func extractNapCatTokenFromResponse(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	if token := extractNapCatTokenFromHeaders(resp.Header); token != "" {
		return token
	}
	return extractNapCatTokenFromCookies(resp.Cookies())
}

func napCatBaseURL(cfg ServiceConfig) string {
	host := strings.TrimSpace(cfg.Host)
	port := cfg.Port
	if port == 0 {
		port = 6099
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

func buildNapCatRequest(method, rawURL string, body io.Reader, token string) (*http.Request, error) {
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("User-Agent", "service-status/1.0")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	return req, nil
}

func napCatAuthCheck(client *http.Client, cfg ServiceConfig, baseURL, token string) (bool, int, string) {
	path := "/api/auth/check"
	req, err := buildNapCatRequest(http.MethodPost, baseURL+path, nil, token)
	if err != nil {
		return false, 0, "请求创建失败"
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, 0, "NapCat 请求失败"
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256))
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, resp.StatusCode, ""
	}
	return false, resp.StatusCode, fmt.Sprintf("HTTP %d", resp.StatusCode)
}

func exchangeNapCatBrowserToken(client *http.Client, cfg ServiceConfig, initialToken string) (string, string, error) {
	baseURL := napCatBaseURL(cfg)
	type tokenCandidate struct {
		Value string
		Label string
	}
	hashCandidates := make([]tokenCandidate, 0, 4)
	seenCandidates := map[string]struct{}{}
	appendCandidate := func(value, label string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seenCandidates[value]; ok {
			return
		}
		seenCandidates[value] = struct{}{}
		hashCandidates = append(hashCandidates, tokenCandidate{Value: value, Label: label})
	}
	if isHexSHA256(initialToken) {
		appendCandidate(initialToken, "prehashed")
	} else {
		appendCandidate(napCatLoginHash(initialToken), "sha256(token+.napcat)")
	}
	if envelope, ok := decodeNapCatSessionToken(initialToken); ok {
		hashCandidates = hashCandidates[:0]
		seenCandidates = map[string]struct{}{}
		appendCandidate(envelope.Data.HashEncoded, "session.HashEncoded")
	}

	type loginAttempt struct {
		Path  string
		Body  map[string]string
		Label string
	}

	attempts := make([]loginAttempt, 0, len(hashCandidates))
	for _, candidate := range hashCandidates {
		attempts = append(attempts, loginAttempt{
			Path:  "/api/auth/login",
			Body:  map[string]string{"hash": candidate.Value},
			Label: "/api/auth/login hash=" + candidate.Label,
		})
	}
	if len(attempts) == 0 {
		return "", "", errors.New("没有可用于 /api/auth/login 的 hash 候选值")
	}

	lastMessage := "auth/login 交换失败"
	failures := make([]string, 0, 6)
	seenFailures := map[string]struct{}{}
	appendFailure := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if _, ok := seenFailures[text]; ok {
			return
		}
		seenFailures[text] = struct{}{}
		if len(failures) < 6 {
			failures = append(failures, text)
		}
		lastMessage = text
	}
	for _, attempt := range attempts {
		payload, err := json.Marshal(attempt.Body)
		if err != nil {
			appendFailure(attempt.Label + " JSON 序列化失败")
			continue
		}
		req, err := buildNapCatRequest(http.MethodPost, baseURL+attempt.Path, bytes.NewReader(payload), "")
		if err != nil {
			appendFailure(attempt.Label + " 请求创建失败")
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			appendFailure(attempt.Label + " NapCat 登录请求失败")
			continue
		}

		if token := extractNapCatTokenFromResponse(resp); token != "" {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return token, attempt.Path, nil
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if readErr != nil {
			appendFailure(attempt.Label + " NapCat 登录响应读取失败")
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			appendFailure(fmt.Sprintf("%s HTTP %d: %s", attempt.Label, resp.StatusCode, sanitizeNapCatPreview(string(body))))
			continue
		}

		trimmed := strings.TrimSpace(string(body))
		if isNapCatSessionToken(trimmed) {
			return trimmed, attempt.Path, nil
		}

		var parsed any
		if err := json.Unmarshal(body, &parsed); err == nil {
			if token := extractNapCatTokenValue(parsed); token != "" {
				return token, attempt.Path, nil
			}
		}
		appendFailure(fmt.Sprintf("%s 2xx 但未返回会话 token: %s", attempt.Label, sanitizeNapCatPreview(trimmed)))
	}

	if len(failures) > 0 {
		return "", "", errors.New(strings.Join(failures, " | "))
	}
	return "", "", errors.New(lastMessage)
}

func resolveNapCatAuthToken(client *http.Client, cfg ServiceConfig) (string, string, error) {
	rawToken := strings.TrimSpace(cfg.Token)
	if rawToken == "" {
		return "", "", errors.New("缺少 NapCat token")
	}
	if isNapCatSessionToken(rawToken) {
		return rawToken, "session", nil
	}

	cacheKey := napCatSessionCacheKey(cfg, rawToken)
	if cached := strings.TrimSpace(getCachedNapCatToken(cacheKey)); cached != "" {
		if ok, _, _ := napCatAuthCheck(client, cfg, napCatBaseURL(cfg), cached); ok {
			return cached, "cached-login", nil
		}
		setCachedNapCatToken(cacheKey, "")
	}

	token, _, err := exchangeNapCatBrowserToken(client, cfg, rawToken)
	if err != nil {
		return "", "", err
	}
	setCachedNapCatToken(cacheKey, token)
	return token, "login", nil
}

func retryNapCatAuthToken(client *http.Client, cfg ServiceConfig, tokenSource string) (string, string, error) {
	rawToken := strings.TrimSpace(cfg.Token)
	cacheKey := napCatSessionCacheKey(cfg, rawToken)

	if tokenSource == "cached-login" {
		setCachedNapCatToken(cacheKey, "")
		return resolveNapCatAuthToken(client, cfg)
	}

	if isNapCatSessionToken(rawToken) {
		token, _, err := exchangeNapCatBrowserToken(client, cfg, rawToken)
		if err != nil {
			return "", "", err
		}
		setCachedNapCatToken(cacheKey, token)
		return token, "session-refresh", nil
	}

	token, _, err := exchangeNapCatBrowserToken(client, cfg, rawToken)
	if err != nil {
		return "", "", err
	}
	setCachedNapCatToken(cacheKey, token)
	return token, "login-retry", nil
}

func fetchNapCatStatus(client *http.Client, cfg ServiceConfig, authToken string) (*http.Response, int64, error) {
	host := strings.TrimSpace(cfg.Host)
	port := cfg.Port
	if port == 0 {
		port = 6099
	}
	statusEndpoint := fmt.Sprintf("http://%s:%d/api/Plugin/ext/napcat-plugin-builtin/status", host, port)

	req, err := buildNapCatRequest(http.MethodGet, statusEndpoint, nil, authToken)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Referer", fmt.Sprintf("http://%s:%d/plugin/napcat-plugin-builtin/page/dashboard", host, port))

	start := time.Now()
	resp, err := client.Do(req)
	return resp, time.Since(start).Milliseconds(), err
}

func loadConfigFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var raw Config
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, err
	}

	cfg := normalizeConfig(raw)
	if cfg.Port == "" {
		cfg.Port = "8080"
	}
	return cfg, nil
}

func normalizeConfig(raw Config) Config {
	cfg := Config{
		Port:      raw.Port,
		HTTPSPort: raw.HTTPSPort,
		TLSCert:   raw.TLSCert,
		TLSKey:    raw.TLSKey,
	}

	appendService := func(groupName string, groupDisplay *bool, svc ServiceConfig) {
		if !isEnabled(svc.Enable) {
			return
		}
		svc.Group = strings.TrimSpace(svc.Group)
		if svc.Group == "" {
			svc.Group = strings.TrimSpace(groupName)
		}
		visible := isDisplayEnabled(groupDisplay) && isDisplayEnabled(svc.Display)
		svc.Display = boolPtr(visible)
		cfg.Services = append(cfg.Services, svc)
	}

	for _, svc := range raw.Services {
		appendService("", nil, svc)
	}
	for _, group := range raw.ServiceGroups {
		if !isEnabled(group.Enable) {
			continue
		}

		groupVisible := isDisplayEnabled(group.Display)
		visibleGroup := ServiceGroupConfig{
			Name:        group.Name,
			Description: group.Description,
			Display:     boolPtr(groupVisible),
			Enable:      group.Enable,
		}
		for _, svc := range group.Services {
			if !isEnabled(svc.Enable) {
				continue
			}
			appendService(group.Name, group.Display, svc)
			if groupVisible && isDisplayEnabled(svc.Display) {
				visibleGroup.Services = append(visibleGroup.Services, svc)
			}
		}
		if groupVisible && len(visibleGroup.Services) > 0 {
			cfg.ServiceGroups = append(cfg.ServiceGroups, visibleGroup)
		}
	}

	return cfg
}

func isDisplayEnabled(display *bool) bool {
	return display == nil || *display
}

func isEnabled(enable *bool) bool {
	return enable == nil || *enable
}

func boolPtr(v bool) *bool {
	return &v
}

func setConfig(cfg Config) {
	configLock.Lock()
	config = cfg
	configLock.Unlock()
}

func getConfig() Config {
	configLock.RLock()
	defer configLock.RUnlock()
	return config
}

func getServicesSnapshot() []ServiceConfig {
	configLock.RLock()
	defer configLock.RUnlock()
	services := make([]ServiceConfig, len(config.Services))
	copy(services, config.Services)
	return services
}

func getServiceMap() map[string]ServiceConfig {
	services := getServicesSnapshot()
	result := make(map[string]ServiceConfig, len(services))
	for _, svc := range services {
		result[serviceKey(svc)] = svc
	}
	return result
}

func watchConfig(path string) {
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("[配置] 监控 config.json 失败: %v", err)
		return
	}
	lastModTime := info.ModTime()

	ticker := time.NewTicker(configWatchInterval)
	defer ticker.Stop()

	for range ticker.C {
		info, err := os.Stat(path)
		if err != nil {
			log.Printf("[配置] 读取文件时间失败: %v", err)
			continue
		}
		if !info.ModTime().After(lastModTime) {
			continue
		}
		lastModTime = info.ModTime()

		nextCfg, err := loadConfigFile(path)
		if err != nil {
			log.Printf("[配置] 重新加载失败: %v", err)
			continue
		}

		prevCfg := getConfig()
		setConfig(nextCfg)
		restoreRecentHistory(nextCfg.Services)
		log.Printf("[配置] 已重新加载，当前服务数量： %d ", len(nextCfg.Services))
		if prevCfg.Port != nextCfg.Port || prevCfg.HTTPSPort != nextCfg.HTTPSPort || prevCfg.TLSCert != nextCfg.TLSCert || prevCfg.TLSKey != nextCfg.TLSKey {
			log.Printf("[配置] 监听端口或 TLS 配置已变更，需重启程序后生效")
		}

		refreshAllStatus()
	}
}

// ========== 检测函数 ==========

func httpTransportForConfig(cfg ServiceConfig) http.RoundTripper {
	if cfg.Insecure {
		return &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return http.DefaultTransport
}

func httpClientForConfig(cfg ServiceConfig) *http.Client {
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout == 0 {
		timeout = 5 * time.Second
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: httpTransportForConfig(cfg),
	}
}

func executeHTTPProbe(client *http.Client, rawURL string, method string, useRange bool) (*http.Response, int64, error) {
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "service-status/1.0")
	if useRange {
		req.Header.Set("Range", "bytes=0-511")
	}

	start := time.Now()
	resp, err := client.Do(req)
	return resp, time.Since(start).Milliseconds(), err
}

func shouldFallbackToHTTPGet(rawURL string, code int, serverHeader string) bool {
	if code == http.StatusMethodNotAllowed || code == http.StatusNotImplemented {
		return true
	}
	if code != http.StatusNotFound {
		return false
	}
	return isGitHubPagesHost(rawURL) || strings.Contains(serverHeader, "vercel")
}

func checkHTTP(cfg ServiceConfig) ServiceStatus {
	client := httpClientForConfig(cfg)
	checkedAt := time.Now().Format("15:04:05")
	s := ServiceStatus{
		Name:      cfg.Name,
		Type:      cfg.Type,
		CheckedAt: checkedAt,
	}

	resp, elapsed, err := executeHTTPProbe(client, cfg.URL, http.MethodHead, false)
	s.ResponseTime = elapsed
	if err != nil {
		s.Status = "offline"
		s.Message = classifyHTTPError(cfg.URL, err)
		return s
	}

	code := resp.StatusCode
	serverHeader := strings.ToLower(resp.Header.Get("Server"))
	_ = resp.Body.Close()

	bodyText := ""
	if shouldFallbackToHTTPGet(cfg.URL, code, serverHeader) {
		resp, elapsed, err = executeHTTPProbe(client, cfg.URL, http.MethodGet, true)
		s.ResponseTime = elapsed
		if err != nil {
			s.Status = "offline"
			s.Message = classifyHTTPError(cfg.URL, err)
			return s
		}
		defer resp.Body.Close()
		code = resp.StatusCode
		serverHeader = strings.ToLower(resp.Header.Get("Server"))
		bodyPreview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		bodyText = strings.ToLower(string(bodyPreview))
	}

	s.Message = classifyHTTPResponse(cfg.URL, code, serverHeader, bodyText)
	switch {
	case code >= 200 && code < 400:
		s.Status = "online"
	case isVercelDeploymentMissing(code, serverHeader, bodyText):
		s.Status = "offline"
	case isGitHubPagesMissingSite(cfg.URL, bodyText):
		s.Status = "offline"
	case code >= 400 && code < 500:
		s.Status = "unknown"
	default:
		s.Status = "offline"
	}
	return s
}

func classifyHTTPError(rawURL string, err error) string {
	if strings.TrimSpace(rawURL) == "" {
		return "缺少 URL"
	}

	var urlErr *neturl.Error
	if errors.As(err, &urlErr) {
		if urlErr.Timeout() {
			return "请求超时"
		}
		err = urlErr.Err
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS 解析失败"
	}

	var unknownAuthorityErr x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthorityErr) {
		return "TLS 证书不受信任"
	}

	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return "TLS 主机名不匹配"
	}

	var certInvalidErr x509.CertificateInvalidError
	if errors.As(err, &certInvalidErr) {
		return "TLS 证书无效"
	}

	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "连接被拒绝"
	case errors.Is(err, syscall.ECONNRESET):
		return "连接被重置"
	case errors.Is(err, syscall.ECONNABORTED):
		return "连接被中止"
	case errors.Is(err, syscall.ETIMEDOUT):
		return "连接超时"
	}

	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "unsupported protocol scheme"):
		return "URL 缺少协议"
	case strings.Contains(text, "no such host"):
		return "DNS 解析失败"
	case strings.Contains(text, "certificate signed by unknown authority"):
		return "TLS 证书不受信任"
	case strings.Contains(text, "certificate is valid for"), strings.Contains(text, "not ") && strings.Contains(text, "requested server name"):
		return "TLS 主机名不匹配"
	case strings.Contains(text, "first record does not look like a tls handshake"), strings.Contains(text, "tls: handshake failure"):
		return "TLS 握手失败"
	case strings.Contains(text, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(text, "connection reset"):
		return "连接被重置"
	case strings.Contains(text, "timeout"), strings.Contains(text, "deadline exceeded"):
		return "请求超时"
	case strings.Contains(text, "no host in request url"), strings.Contains(text, "missing protocol scheme"):
		return "URL 配置错误"
	case strings.Contains(text, "eof"):
		return "连接意外关闭"
	}

	return "连接失败"
}

func classifyHTTPResponse(rawURL string, code int, serverHeader, bodyText string) string {
	if isVercelDeploymentMissing(code, serverHeader, bodyText) {
		return "Vercel 不存在"
	}
	if code == http.StatusNotFound {
		if isGitHubPagesMissingSite(rawURL, bodyText) {
			return "GitHub Pages 不存在"
		}
		if isGitHubPagesHost(rawURL) {
			return "GitHub Pages 404"
		}
		if strings.Contains(serverHeader, "vercel") || strings.Contains(bodyText, "deployment_not_found") || strings.Contains(bodyText, "404: not_found") {
			return "Vercel 404"
		}
		return "HTTP 404"
	}
	return fmt.Sprintf("HTTP %d", code)
}

func isVercelDeploymentMissing(code int, serverHeader, bodyText string) bool {
	if code != http.StatusNotFound {
		return false
	}
	if !strings.Contains(serverHeader, "vercel") && !strings.Contains(bodyText, "deployment_not_found") && !strings.Contains(bodyText, "404: not_found") {
		return false
	}
	return strings.Contains(bodyText, "deployment_not_found")
}

func isGitHubPagesMissingSite(rawURL, bodyText string) bool {
	_ = rawURL
	return strings.Contains(bodyText, "there isn't a github pages site here")
}

func isGitHubPagesHost(rawURL string) bool {
	u, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil || u.URL == nil {
		return false
	}
	host := strings.ToLower(u.URL.Hostname())
	return strings.HasSuffix(host, ".github.io") || host == "github.io"
}

func checkTCP(cfg ServiceConfig) ServiceStatus {
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout == 0 {
		timeout = 5 * time.Second
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	elapsed := time.Since(start).Milliseconds()

	s := ServiceStatus{
		Name:         cfg.Name,
		Type:         cfg.Type,
		ResponseTime: elapsed,
		CheckedAt:    time.Now().Format("15:04:05"),
	}
	if err != nil {
		s.Status = "offline"
		s.Message = "端口不可达"
		return s
	}
	conn.Close()
	s.Status = "online"
	s.Message = "端口连通"
	return s
}

func checkMySQL(cfg ServiceConfig) ServiceStatus {
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout == 0 {
		timeout = 5 * time.Second
	}
	port := cfg.Port
	if port == 0 {
		port = 3306
	}

	s := ServiceStatus{
		Name:      cfg.Name,
		Type:      cfg.Type,
		CheckedAt: time.Now().Format("15:04:05"),
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		s.ResponseTime = time.Since(start).Milliseconds()
		s.Status = "offline"
		s.Message = "MySQL 连接失败"
		return s
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	header := make([]byte, 4)
	if _, err = readFull(conn, header); err != nil {
		s.ResponseTime = time.Since(start).Milliseconds()
		s.Status = "offline"
		s.Message = "MySQL 握手超时"
		return s
	}

	payloadLen := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	if payloadLen <= 0 || payloadLen > 1<<20 {
		s.ResponseTime = time.Since(start).Milliseconds()
		s.Status = "unknown"
		s.Message = "MySQL 握手包异常"
		return s
	}

	payload := make([]byte, payloadLen)
	if _, err = readFull(conn, payload); err != nil {
		s.ResponseTime = time.Since(start).Milliseconds()
		s.Status = "offline"
		s.Message = "MySQL 握手读取失败"
		return s
	}

	s.ResponseTime = time.Since(start).Milliseconds()
	if len(payload) == 0 {
		s.Status = "unknown"
		s.Message = "MySQL 空响应"
		return s
	}
	if payload[0] == 0xff {
		s.Status = "unknown"
		s.Message = "MySQL 返回错误包"
		return s
	}
	if payload[0] == 0x0a {
		versionEnd := 1
		for versionEnd < len(payload) && payload[versionEnd] != 0x00 {
			versionEnd++
		}
		version := strings.TrimSpace(string(payload[1:versionEnd]))
		s.Status = "online"
		if version != "" {
			s.Message = fmt.Sprintf("MySQL version %s", version)
		} else {
			s.Message = "MySQL 握手成功"
		}
		return s
	}

	s.Status = "unknown"
	s.Message = fmt.Sprintf("非标准 MySQL 握手(0x%02x)", payload[0])
	return s
}

func checkNapCatQQ(cfg ServiceConfig) ServiceStatus {
	s := ServiceStatus{
		Name:      cfg.Name,
		Type:      cfg.Type,
		CheckedAt: time.Now().Format("15:04:05"),
	}

	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		s.Status = "unknown"
		s.Message = "缺少 NapCat host"
		return s
	}
	port := cfg.Port
	if port == 0 {
		port = 6099
	}

	client := httpClientForConfig(cfg)
	authToken, tokenSource, err := resolveNapCatAuthToken(client, cfg)
	if err != nil {
		s.Status = "unknown"
		s.Message = err.Error()
		return s
	}

	if ok, statusCode, msg := napCatAuthCheck(client, cfg, napCatBaseURL(cfg), authToken); !ok {
		cacheKey := napCatSessionCacheKey(cfg, strings.TrimSpace(cfg.Token))
		if tokenSource == "cached-login" {
			setCachedNapCatToken(cacheKey, "")
			if refreshed, refreshedSource, refreshErr := resolveNapCatAuthToken(client, cfg); refreshErr == nil {
				authToken = refreshed
				tokenSource = refreshedSource
				ok, statusCode, msg = napCatAuthCheck(client, cfg, napCatBaseURL(cfg), authToken)
			}
		}
		if !ok && tokenSource == "session" {
			if refreshed, _, refreshErr := exchangeNapCatBrowserToken(client, cfg, strings.TrimSpace(cfg.Token)); refreshErr == nil {
				authToken = refreshed
				tokenSource = "session-refresh"
				ok, statusCode, msg = napCatAuthCheck(client, cfg, napCatBaseURL(cfg), authToken)
			}
		}
		if !ok {
			if statusCode >= 500 || statusCode == 0 {
				s.Status = "offline"
			} else {
				s.Status = "unknown"
			}
			if strings.TrimSpace(msg) == "" {
				msg = fmt.Sprintf("HTTP %d", statusCode)
			}
			s.Message = "WebUI 鉴权失败: " + msg
			return s
		}
	}

	needRetry := false
	for attempt := 0; attempt < 2; attempt++ {
		resp, elapsed, err := fetchNapCatStatus(client, cfg, authToken)
		s.ResponseTime = elapsed
		if err != nil {
			s.Status = "offline"
			s.Message = "NapCat 请求失败"
			return s
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				s.Status = "unknown"
			} else {
				s.Status = "offline"
			}
			s.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
			return s
		}

		var payload map[string]any
		if err := json.NewDecoder(io.LimitReader(resp.Body, 512)).Decode(&payload); err != nil {
			_ = resp.Body.Close()
			s.Status = "offline"
			s.Message = "NapCat 响应解析失败"
			return s
		}
		_ = resp.Body.Close()

		if isNapCatUnauthorized(payload) {
			if attempt == 0 {
				refreshedToken, refreshedSource, refreshErr := retryNapCatAuthToken(client, cfg, tokenSource)
				if refreshErr == nil {
					authToken = refreshedToken
					tokenSource = refreshedSource
					needRetry = true
					continue
				}
			}
			s.Status = "unknown"
			s.Message = fmt.Sprintf("HTTP %d · code = -1", resp.StatusCode)
			return s
		}

		uptimeText := strings.TrimSpace(parseNapCatUptimeFormatted(payload))
		if code, ok := parseNapCatCode(payload); ok {
			switch code {
			case -1:
				if needRetry || attempt > 0 {
					s.Status = "unknown"
					s.Message = fmt.Sprintf("HTTP %d · code = -1", resp.StatusCode)
					return s
				}
				s.Status = "unknown"
				s.Message = fmt.Sprintf("HTTP %d · code = -1", resp.StatusCode)
				return s
			case 0:
				s.Status = "online"
				if uptimeText != "" {
					s.Message = fmt.Sprintf("HTTP %d · %s", resp.StatusCode, uptimeText)
				} else {
					s.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
				}
				return s
			default:
				s.Status = "unknown"
				s.Message = fmt.Sprintf("HTTP %d · code = %d", resp.StatusCode, code)
				return s
			}
		}

		s.Status = "online"
		if uptimeText != "" {
			s.Message = fmt.Sprintf("HTTP %d · %s", resp.StatusCode, uptimeText)
		} else {
			s.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return s
	}

	s.Status = "unknown"
	s.Message = "HTTP 200 · code = -1"
	return s
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// ========== PING ==========

func checkPing(cfg ServiceConfig) ServiceStatus {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 5
	}
	s := ServiceStatus{
		Name:      cfg.Name,
		Type:      cfg.Type,
		CheckedAt: time.Now().Format("15:04:05"),
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// -n 1：发1个包  -w：超时毫秒
		cmd = exec.Command("ping", "-n", "1", "-w", fmt.Sprintf("%d", timeout*1000), cfg.Host)
	} else {
		// -c 1：发1个包  -W：超时秒
		cmd = exec.Command("ping", "-c", "1", "-W", fmt.Sprintf("%d", timeout), cfg.Host)
	}

	start := time.Now()
	out, err := cmd.Output()
	elapsed := time.Since(start).Milliseconds()
	s.ResponseTime = elapsed

	if err != nil {
		s.Status = "offline"
		s.Message = "无法到达"
		return s
	}

	output := string(out)
	// 从输出里提取 RTT（Windows: "时间=Xms" / "time=Xms"，Linux: "time=X ms"）
	re := regexp.MustCompile(`[Tt]ime[=<](\d+)`)
	if m := re.FindStringSubmatch(output); len(m) > 1 {
		if ms, err2 := strconv.ParseInt(m[1], 10, 64); err2 == nil {
			s.ResponseTime = ms
		}
	}

	// Windows 丢包判断："100%" 丢包 = 失败
	if strings.Contains(output, "100%") ||
		strings.Contains(output, "unreachable") ||
		strings.Contains(output, "无法访问") {
		s.Status = "offline"
		s.Message = "丢包 100%"
		return s
	}

	s.Status = "online"
	s.Message = fmt.Sprintf("RTT %d ms", s.ResponseTime)
	return s
}

func checkService(cfg ServiceConfig) ServiceStatus {
	switch cfg.Type {
	case "http":
		return checkHTTP(cfg)
	case "tcp":
		return checkTCP(cfg)
	case "mysql":
		return checkMySQL(cfg)
	case "napcat_qq":
		return checkNapCatQQ(cfg)
	case "ping":
		return checkPing(cfg)
	default:
		return ServiceStatus{
			Name:      cfg.Name,
			Type:      cfg.Type,
			Status:    "unknown",
			Message:   "未知检测类型",
			CheckedAt: time.Now().Format("15:04:05"),
		}
	}
}

// ========== 刷新 & HTTP ==========

type serviceObservation struct {
	index   int
	key     string
	cfg     ServiceConfig
	checked bool
	entry   HistoryEntry
	state   ServiceStatus
}

func loadPersistedHistories(services []ServiceConfig, since time.Time) (map[string][]HistoryEntry, error) {
	result := make(map[string][]HistoryEntry)
	if historyDB == nil || len(services) == 0 {
		return result, nil
	}

	keys := make([]string, 0, len(services)*2)
	aliasToCanonical := make(map[string]string, len(services)*2)
	for _, svc := range services {
		canonical := serviceKey(svc)
		for _, key := range serviceKeyCandidates(svc) {
			keys = append(keys, key)
			aliasToCanonical[key] = canonical
		}
	}

	entriesByKey, err := historyDB.LoadWindow(keys, since)
	if err != nil {
		return nil, err
	}

	for aliasKey, entries := range entriesByKey {
		canonical := aliasToCanonical[aliasKey]
		result[canonical] = append(result[canonical], entries...)
	}

	for canonical, entries := range result {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Ts < entries[j].Ts
		})
		result[canonical] = entries
	}

	return result, nil
}

func restoreRecentHistory(services []ServiceConfig) {
	if historyDB == nil || len(services) == 0 {
		return
	}

	entriesByKey, err := loadPersistedHistories(services, time.Now().Add(-memoryHistoryRetention))
	if err != nil {
		log.Printf("[存储] 恢复最近历史失败: %v", err)
		return
	}

	totalEntries := 0
	for _, svc := range services {
		key := serviceKey(svc)
		entries := entriesByKey[key]
		totalEntries += len(entries)
		getHistory(key).replace(entries)
	}
	if totalEntries > 0 {
		log.Printf("[存储] 已恢复最近 24h 历史，共 %d 条", totalEntries)
	}
}

func refreshAllStatus() {
	refreshLock.Lock()
	defer refreshLock.Unlock()

	now := time.Now()
	services := getServicesSnapshot()
	serviceKeys := make([]string, 0, len(services))
	for _, svc := range services {
		serviceKeys = append(serviceKeys, serviceKey(svc))
	}
	pruneProbeState(serviceKeys)

	prevStatus := getLastStatusMap()
	results := make([]ServiceStatus, len(services))
	observations := make([]serviceObservation, len(services))
	var wg sync.WaitGroup
	for i, svc := range services {
		key := serviceKey(svc)
		if !shouldCheckServiceNow(key, now) {
			if cached, ok := prevStatus[key]; ok {
				cached.Group = svc.Group
				cached.Subtitle = svc.Subtitle
				cached.Visible = isDisplayEnabled(svc.Display)
				results[i] = cached
				continue
			}
		}

		wg.Add(1)
		go func(idx int, key string, s ServiceConfig) {
			defer wg.Done()
			r := checkService(s)
			checkedAt := time.Now()
			observations[idx] = serviceObservation{
				index:   idx,
				key:     key,
				cfg:     s,
				checked: true,
				entry: HistoryEntry{
					Status:       r.Status,
					ResponseTime: r.ResponseTime,
					Time:         formatHistoryTime(checkedAt),
					Ts:           checkedAt.Unix(),
				},
				state: r,
			}
		}(i, key, svc)
	}
	wg.Wait()

	checkedCount := 0
	for _, observation := range observations {
		if !observation.checked {
			continue
		}
		checkedCount++
		h := getHistory(observation.key)
		h.record(observation.entry)
		updateProbeState(observation.key, time.Unix(observation.entry.Ts, 0), observation.entry.Status)
		if historyDB != nil {
			if err := historyDB.Insert(observation.key, observation.cfg, observation.entry); err != nil {
				log.Printf("[存储] 写入历史失败(%s): %v", observation.cfg.Name, err)
			}
		}
	}

	if historyDB != nil {
		if err := historyDB.MaybeCleanup(time.Now()); err != nil {
			log.Printf("[存储] 清理过期历史失败: %v", err)
		}
	}

	historiesByKey := map[string][]HistoryEntry{}
	if historyDB != nil && len(observations) > 0 {
		serviceMap := make(map[string]ServiceConfig, len(observations))
		for _, observation := range observations {
			if !observation.checked {
				continue
			}
			serviceMap[observation.key] = observation.cfg
		}
		serviceList := make([]ServiceConfig, 0, len(serviceMap))
		for _, svc := range serviceMap {
			serviceList = append(serviceList, svc)
		}
		dbHistories, err := loadPersistedHistories(serviceList, time.Now().Add(-persistentHistoryWindow))
		if err != nil {
			log.Printf("[存储] 读取 30 天历史失败，将退回内存缓存: %v", err)
		} else {
			historiesByKey = dbHistories
		}
	}

	for _, observation := range observations {
		if !observation.checked {
			continue
		}
		r := observation.state
		r.Key = observation.key
		r.Group = observation.cfg.Group
		r.Subtitle = observation.cfg.Subtitle
		r.Visible = isDisplayEnabled(observation.cfg.Display)
		memHistory, memUptime, memCount := getHistory(observation.key).snapshot()
		r.Uptime = memUptime
		r.CheckCount = memCount
		statsHistory := memHistory
		if history, ok := historiesByKey[observation.key]; ok && len(history) > 0 {
			statsHistory = history
		}
		nowUnix := time.Now().Unix()
		r.Uptime24BP = uptimeBasisPointsSince(statsHistory, nowUnix-int64(24*time.Hour/time.Second))
		r.Uptime7dBP = uptimeBasisPointsSince(statsHistory, nowUnix-int64(7*24*time.Hour/time.Second))
		r.Uptime30dBP = uptimeBasisPointsSince(statsHistory, nowUnix-int64(30*24*time.Hour/time.Second))
		results[observation.index] = r
	}

	statusLock.Lock()
	lastStatus = results
	if checkedCount > 0 || lastRefreshed == 0 {
		lastRefreshed = time.Now().Unix()
	}
	statusLock.Unlock()
	log.Printf("[刷新] 本轮实际检测 %d / %d 个服务", checkedCount, len(results))
}

func uptimeBasisPointsSince(entries []HistoryEntry, since int64) int {
	total := 0
	success := 0
	for _, entry := range entries {
		if entry.Ts < since {
			continue
		}
		total++
		if entry.Status == "online" {
			success++
		}
	}
	if total == 0 {
		return -1
	}
	return success * 10000 / total
}

func parseSince(query string) int64 {
	since, err := strconv.ParseInt(strings.TrimSpace(query), 10, 64)
	if err != nil || since < 0 {
		return 0
	}
	return since
}

func parseRangeWindow(query string) (string, time.Duration) {
	switch strings.TrimSpace(strings.ToLower(query)) {
	case "7d":
		return "7d", 7 * 24 * time.Hour
	case "30d":
		return "30d", 30 * 24 * time.Hour
	default:
		return "24h", 24 * time.Hour
	}
}

func parseSlots(query string, fallback int) int {
	slots, err := strconv.Atoi(strings.TrimSpace(query))
	if err != nil || slots <= 0 {
		return fallback
	}
	if slots < 10 {
		return 10
	}
	if slots > 120 {
		return 120
	}
	return slots
}

func parseKeys(r *http.Request) []string {
	var keys []string
	for _, raw := range r.URL.Query()["keys"] {
		for _, part := range strings.Split(raw, ",") {
			key := strings.TrimSpace(part)
			if key == "" {
				continue
			}
			keys = append(keys, key)
		}
	}
	if len(keys) > 64 {
		keys = keys[:64]
	}
	return dedupeStrings(keys)
}

func encodeStatus(status string) int {
	switch status {
	case "online":
		return 0
	case "offline":
		return 1
	default:
		return 2
	}
}

func encodeType(kind string) int {
	switch kind {
	case "http":
		return 0
	case "tcp":
		return 1
	case "ping":
		return 2
	case "mysql":
		return 3
	case "napcat_qq":
		return 4
	default:
		return 9
	}
}

func snapshotStatuses(includeHidden bool) ([]ServiceStatus, int64) {
	statusLock.RLock()
	defer statusLock.RUnlock()

	result := make([]ServiceStatus, 0, len(lastStatus))
	for _, svc := range lastStatus {
		if !includeHidden && !svc.Visible {
			continue
		}
		copySvc := svc
		copySvc.History = nil
		result = append(result, copySvc)
	}
	return result, lastRefreshed
}

func statusETag(refreshedAt int64) string {
	return fmt.Sprintf("W/\"status-%d\"", refreshedAt)
}

func clientHasFreshStatus(r *http.Request, refreshedAt int64) bool {
	if refreshedAt <= 0 {
		return false
	}
	if since := parseSince(r.URL.Query().Get("since")); since >= refreshedAt {
		return true
	}
	if match := strings.TrimSpace(r.Header.Get("If-None-Match")); match != "" && match == statusETag(refreshedAt) {
		return true
	}
	return false
}

func sampleHistoryEntries(entries []HistoryEntry, since int64, slots int) []HistoryEntry {
	filtered := make([]HistoryEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Ts >= since {
			filtered = append(filtered, entry)
		}
	}
	if len(filtered) <= slots {
		return filtered
	}

	step := float64(len(filtered)) / float64(slots)
	sampled := make([]HistoryEntry, 0, slots)
	for i := 0; i < slots; i++ {
		index := int(float64(i) * step)
		if index >= len(filtered) {
			index = len(filtered) - 1
		}
		sampled = append(sampled, filtered[index])
	}
	return sampled
}

func loadHistoryWindowForServices(services []ServiceConfig, since time.Time) (map[string][]HistoryEntry, error) {
	result := make(map[string][]HistoryEntry, len(services))
	if len(services) == 0 {
		return result, nil
	}

	if historyDB != nil {
		entriesByKey, err := loadPersistedHistories(services, since)
		if err != nil {
			return nil, err
		}
		for _, svc := range services {
			key := serviceKey(svc)
			if entries, ok := entriesByKey[key]; ok && len(entries) > 0 {
				result[key] = entries
				continue
			}
			memEntries, _, _ := getHistory(key).snapshot()
			result[key] = sampleHistoryEntries(memEntries, since.Unix(), len(memEntries))
		}
		return result, nil
	}

	for _, svc := range services {
		key := serviceKey(svc)
		memEntries, _, _ := getHistory(key).snapshot()
		result[key] = sampleHistoryEntries(memEntries, since.Unix(), len(memEntries))
	}
	return result, nil
}

const allowedStatusAPIOrigin = "https://static.0201799.xyz"

func requestScheme(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if len(parts) > 0 {
			proto := strings.ToLower(strings.TrimSpace(parts[0]))
			if proto != "" {
				return proto
			}
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func currentRequestOrigin(r *http.Request) string {
	return requestScheme(r) + "://" + r.Host
}

func isLoopbackOrigin(origin string) bool {
	if strings.TrimSpace(origin) == "" {
		return false
	}
	u, err := neturl.Parse(origin)
	if err != nil {
		return false
	}
	host := strings.TrimSpace(strings.ToLower(u.Hostname()))
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func allowedStatusOriginValue(r *http.Request) string {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	switch {
	case origin == "":
		return ""
	case strings.EqualFold(origin, "null"):
		return "null"
	case strings.EqualFold(origin, allowedStatusAPIOrigin):
		return allowedStatusAPIOrigin
	case strings.EqualFold(origin, currentRequestOrigin(r)):
		return origin
	case isLoopbackOrigin(origin):
		return origin
	default:
		return ""
	}
}

func isAllowedStatusOrigin(r *http.Request) bool {
	return allowedStatusOriginValue(r) != "" || strings.TrimSpace(r.Header.Get("Origin")) == ""
}

func applyStatusCORS(headers http.Header, r *http.Request) {
	if allowedOrigin := allowedStatusOriginValue(r); allowedOrigin != "" {
		headers.Set("Access-Control-Allow-Origin", allowedOrigin)
	}
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any, cacheControl string, etag string) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "json encode failed", http.StatusInternalServerError)
		return
	}

	headers := w.Header()
	headers.Set("Content-Type", "application/json; charset=utf-8")
	applyStatusCORS(headers, r)
	headers.Set("Cache-Control", cacheControl)
	headers.Set("Vary", "Origin, Accept-Encoding, If-None-Match")
	if etag != "" {
		headers.Set("ETag", etag)
	}

	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && len(body) >= 256 {
		headers.Set("Content-Encoding", "gzip")
		w.WriteHeader(status)
		gz := gzip.NewWriter(w)
		defer gz.Close()
		_, _ = gz.Write(body)
		return
	}

	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func serveStatusSummary(w http.ResponseWriter, r *http.Request) {
	includeHidden := r.URL.Query().Get("all") == "1"
	statuses, refreshedAt := snapshotStatuses(includeHidden)
	etag := statusETag(refreshedAt)
	if clientHasFreshStatus(r, refreshedAt) {
		headers := w.Header()
		applyStatusCORS(headers, r)
		headers.Set("Cache-Control", "public, max-age=15, stale-while-revalidate=45")
		headers.Set("ETag", etag)
		headers.Set("Vary", "Origin, Accept-Encoding, If-None-Match")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	rows := make([][]any, 0, len(statuses))
	for _, svc := range statuses {
		rows = append(rows, []any{
			svc.Key,
			svc.Name,
			svc.Subtitle,
			svc.Group,
			encodeType(svc.Type),
			encodeStatus(svc.Status),
			svc.ResponseTime,
			svc.Message,
			svc.Uptime24BP,
			svc.Uptime7dBP,
			svc.Uptime30dBP,
			svc.Meta,
		})
	}

	writeJSON(w, r, http.StatusOK, compactSummaryResponse{
		V: 2,
		T: refreshedAt,
		I: int(apiRefreshInterval / time.Second),
		S: rows,
	}, "public, max-age=15, stale-while-revalidate=45", etag)
}

func serveStatusHistory(w http.ResponseWriter, r *http.Request) {
	keys := parseKeys(r)
	rangeLabel, window := parseRangeWindow(r.URL.Query().Get("range"))
	slots := parseSlots(r.URL.Query().Get("slots"), 40)
	_, refreshedAt := snapshotStatuses(true)
	if len(keys) == 0 {
		writeJSON(w, r, http.StatusOK, compactHistoryResponse{
			V: 2,
			T: refreshedAt,
			R: rangeLabel,
			S: slots,
			H: [][]any{},
		}, "public, max-age=15, stale-while-revalidate=45", statusETag(refreshedAt))
		return
	}
	serviceMap := getServiceMap()
	services := make([]ServiceConfig, 0, len(keys))
	for _, key := range keys {
		svc, ok := serviceMap[key]
		if !ok {
			continue
		}
		services = append(services, svc)
	}

	since := time.Now().Add(-window)
	entriesByKey, err := loadHistoryWindowForServices(services, since)
	if err != nil {
		http.Error(w, "load history failed", http.StatusInternalServerError)
		return
	}

	rows := make([][]any, 0, len(services))
	for _, svc := range services {
		key := serviceKey(svc)
		sampled := sampleHistoryEntries(entriesByKey[key], since.Unix(), slots)
		points := make([][]any, 0, len(sampled))
		for _, entry := range sampled {
			points = append(points, []any{entry.Ts, encodeStatus(entry.Status), entry.ResponseTime})
		}
		rows = append(rows, []any{key, points})
	}

	writeJSON(w, r, http.StatusOK, compactHistoryResponse{
		V: 2,
		T: refreshedAt,
		R: rangeLabel,
		S: slots,
		H: rows,
	}, "public, max-age=15, stale-while-revalidate=45", statusETag(refreshedAt))
}

func serveStatusHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": "请求正常",
	}, "public, max-age=15, stale-while-revalidate=45", "")
}

func apiStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !isAllowedStatusOrigin(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}

	if r.Method == http.MethodOptions {
		headers := w.Header()
		applyStatusCORS(headers, r)
		headers.Set("Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers")
		headers.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		headers.Set("Access-Control-Allow-Headers", "Content-Type, If-None-Match")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("view"))) {
	case "summary":
		serveStatusSummary(w, r)
	case "history":
		serveStatusHistory(w, r)
	default:
		serveStatusHealth(w, r)
	}
}

func main() {
	configPath := resolveRuntimePath(configFileName)
	webPath := resolveRuntimePath(webDirName)
	historyDBPath := resolveWritablePath(historyDBFileName)

	cfg, err := loadConfigFile(configPath)
	if err != nil {
		log.Fatalf("加载 config.json 失败: %v", err)
	}
	setConfig(cfg)

	historyDB, err = openHistoryStore(historyDBPath)
	if err != nil {
		log.Printf("[存储] SQLite 未启用，将仅使用内存缓存: %v", err)
	} else {
		defer historyDB.Close()
		restoreRecentHistory(cfg.Services)
		log.Printf("[存储] SQLite 历史库已启用: %s", historyDBPath)
	}

	refreshAllStatus()
	go watchConfig(configPath)

	// 每分钟调度一次，单个服务根据上一状态决定 2 分钟或 5 分钟是否到点检测
	go func() {
		ticker := time.NewTicker(schedulerTickInterval)
		defer ticker.Stop()
		for range ticker.C {
			refreshAllStatus()
		}
	}()

	http.HandleFunc("/api/status", apiStatusHandler)
	http.Handle("/", http.FileServer(http.Dir(webPath)))

	addr := ":" + getConfig().Port
	log.Printf("服务状态面板已启动 → http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
