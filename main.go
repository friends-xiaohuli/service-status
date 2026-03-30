package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base32"
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
	Insecure bool   `json:"insecure"` // true = 跳过 TLS 证书校验（自签名/过期证书）
}

type ServiceGroupConfig struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
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
	Name         string         `json:"name"`
	Subtitle     string         `json:"subtitle,omitempty"`
	Type         string         `json:"type"`
	Group        string         `json:"group,omitempty"`
	Status       string         `json:"status"`
	ResponseTime int64          `json:"response_time"`
	Message      string         `json:"message"`
	CheckedAt    string         `json:"checked_at"`
	Uptime       float64        `json:"uptime"`
	CheckCount   int            `json:"check_count"`
	History      []HistoryEntry `json:"history"`
}

// ========== 可用率 & 历史追踪 ==========

const (
	configFileName          = "config.json"
	webDirName              = "web"
	historyDBFileName       = "status-history.db"
	refreshInterval         = 2 * time.Minute
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
)

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
		Port:          raw.Port,
		HTTPSPort:     raw.HTTPSPort,
		TLSCert:       raw.TLSCert,
		TLSKey:        raw.TLSKey,
		ServiceGroups: raw.ServiceGroups,
	}

	appendService := func(groupName string, svc ServiceConfig) {
		svc.Group = strings.TrimSpace(svc.Group)
		if svc.Group == "" {
			svc.Group = strings.TrimSpace(groupName)
		}
		cfg.Services = append(cfg.Services, svc)
	}

	for _, svc := range raw.Services {
		appendService("", svc)
	}
	for _, group := range raw.ServiceGroups {
		for _, svc := range group.Services {
			appendService(group.Name, svc)
		}
	}

	return cfg
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

func checkHTTP(cfg ServiceConfig) ServiceStatus {
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout == 0 {
		timeout = 5 * time.Second
	}
	transport := http.DefaultTransport
	if cfg.Insecure {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	start := time.Now()
	resp, err := client.Get(cfg.URL)
	elapsed := time.Since(start).Milliseconds()

	s := ServiceStatus{
		Name:         cfg.Name,
		Type:         cfg.Type,
		ResponseTime: elapsed,
		CheckedAt:    time.Now().Format("15:04:05"),
	}
	if err != nil {
		s.Status = "offline"
		s.Message = classifyHTTPError(cfg.URL, err)
		return s
	}
	defer resp.Body.Close()
	code := resp.StatusCode
	bodyPreview, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	serverHeader := strings.ToLower(resp.Header.Get("Server"))
	bodyText := strings.ToLower(string(bodyPreview))
	s.Message = classifyHTTPResponse(cfg.URL, code, serverHeader, bodyText)
	switch {
	case code >= 200 && code < 400:
		s.Status = "online"
	case isVercelDeploymentMissing(code, serverHeader, bodyText):
		s.Status = "offline"
	case isGitHubPagesMissingSite(cfg.URL, bodyText):
		s.Status = "offline"
	case code >= 400 && code < 500:
		s.Status = "unknown" // 4xx：服务可达但端点异常
	default:
		s.Status = "offline" // 5xx
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
			s.Message = fmt.Sprintf("MySQL %s", version)
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
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout == 0 {
		timeout = 5 * time.Second
	}

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

	endpoint := fmt.Sprintf("http://%s:%d/plugin/napcat-plugin-builtin/mem/dynamic/info.json", host, port)

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		s.Status = "offline"
		s.Message = "请求创建失败"
		return s
	}
	if token := strings.TrimSpace(cfg.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	transport := http.DefaultTransport
	if cfg.Insecure {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	client := &http.Client{Timeout: timeout, Transport: transport}
	start := time.Now()
	resp, err := client.Do(req)
	s.ResponseTime = time.Since(start).Milliseconds()
	if err != nil {
		s.Status = "offline"
		s.Message = "NapCat 请求失败"
		return s
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.Status = "offline"
		s.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return s
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		s.Status = "offline"
		s.Message = "NapCat 响应解析失败"
		return s
	}

	configVal, ok := result["config"].(map[string]any)
	if !ok {
		s.Status = "unknown"
		s.Message = "缺少 config 字段"
		return s
	}

	prefix, _ := configVal["prefix"].(string)
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		s.Status = "unknown"
		s.Message = "缺少 config.prefix"
		return s
	}

	if prefix == "#napcat" {
		s.Status = "online"
		s.Message = "prefix=#napcat"
		return s
	}

	s.Status = "unknown"
	s.Message = fmt.Sprintf("prefix=%s", prefix)
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
	index int
	key   string
	cfg   ServiceConfig
	entry HistoryEntry
	state ServiceStatus
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

	services := getServicesSnapshot()
	results := make([]ServiceStatus, len(services))
	observations := make([]serviceObservation, len(services))
	var wg sync.WaitGroup
	for i, svc := range services {
		wg.Add(1)
		go func(idx int, s ServiceConfig) {
			defer wg.Done()
			r := checkService(s)
			now := time.Now()
			observations[idx] = serviceObservation{
				index: idx,
				key:   serviceKey(s),
				cfg:   s,
				entry: HistoryEntry{
					Status:       r.Status,
					ResponseTime: r.ResponseTime,
					Time:         formatHistoryTime(now),
					Ts:           now.Unix(),
				},
				state: r,
			}
		}(i, svc)
	}
	wg.Wait()

	for _, observation := range observations {
		h := getHistory(observation.key)
		h.record(observation.entry)
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
		r := observation.state
		r.Group = observation.cfg.Group
		r.Subtitle = observation.cfg.Subtitle
		memHistory, memUptime, memCount := getHistory(observation.key).snapshot()
		r.Uptime = memUptime
		r.CheckCount = memCount
		if history, ok := historiesByKey[observation.key]; ok && len(history) > 0 {
			r.History = history
		} else {
			r.History = memHistory
		}
		results[observation.index] = r
	}

	statusLock.Lock()
	lastStatus = results
	lastRefreshed = time.Now().Unix()
	statusLock.Unlock()
	log.Printf("[刷新] 已检测 %d 个服务", len(results))
}

func apiStatusHandler(w http.ResponseWriter, r *http.Request) {
	statusLock.RLock()
	resp := APIResponse{
		RefreshedAt: lastRefreshed,
		Interval:    int(refreshInterval / time.Second),
		Services:    lastStatus,
	}
	statusLock.RUnlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(resp)
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

	// 每 2 分钟自动刷新
	go func() {
		ticker := time.NewTicker(refreshInterval)
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
