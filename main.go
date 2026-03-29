package main

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ========== 配置结构 ==========

type ServiceConfig struct {
	Name         string `json:"name"`
	Subtitle     string `json:"subtitle,omitempty"`
	Type         string `json:"type"` // http | tcp | ping | mysql | napcat_qq | minecraft_bedrock | minecraft_java
	Group        string `json:"group,omitempty"`
	URL          string `json:"url"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Timeout      int    `json:"timeout"`
	UserID       string `json:"user_id,omitempty"`
	Token        string `json:"token,omitempty"`
	Insecure     bool   `json:"insecure"` // true = 跳过 TLS 证书校验（自签名/过期证书）
	RconHost     string `json:"rcon_host"`
	RconPort     int    `json:"rcon_port"`
	RconPassword string `json:"rcon_password"`
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
	PlayerOnline int            `json:"player_online"`
	PlayerMax    int            `json:"player_max"`
	HasPlayers   bool           `json:"has_players"`
	Uptime       float64        `json:"uptime"`
	CheckCount   int            `json:"check_count"`
	History      []HistoryEntry `json:"history"`
}

// ========== 可用率 & 历史追踪 ==========

const historySize = 21600 // 30天 × 24H × 30次/H（2分钟间隔）

const (
	configPath          = "config.json"
	refreshInterval     = 2 * time.Minute
	configWatchInterval = 2 * time.Second
)

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
	if len(h.entries) > historySize {
		h.entries = h.entries[1:]
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
)

func getHistory(name string) *serviceHistory {
	historyLock.Lock()
	defer historyLock.Unlock()
	if h, ok := historyMap[name]; ok {
		return h
	}
	h := &serviceHistory{}
	historyMap[name] = h
	return h
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
		log.Printf("[配置] 已重新加载，服务数量 %d ", len(nextCfg.Services))
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
		s.Message = "连接失败"
		return s
	}
	defer resp.Body.Close()
	code := resp.StatusCode
	s.Message = fmt.Sprintf("HTTP %d", code)
	switch {
	case code >= 200 && code < 400:
		s.Status = "online"
	case code >= 400 && code < 500:
		s.Status = "unknown" // 4xx：服务可达但端点异常
	default:
		s.Status = "offline" // 5xx
	}
	return s
}

func checkTCP(cfg ServiceConfig) ServiceStatus {
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout == 0 {
		timeout = 5 * time.Second
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
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

	addr := fmt.Sprintf("%s:%d", cfg.Host, port)
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

// ========== RCON ==========

func rconSend(conn net.Conn, reqID int32, typ int32, payload string) error {
	body := []byte(payload)
	length := int32(4 + 4 + len(body) + 2)
	buf := make([]byte, 0, 4+length)
	buf = appendInt32LE(buf, length)
	buf = appendInt32LE(buf, reqID)
	buf = appendInt32LE(buf, typ)
	buf = append(buf, body...)
	buf = append(buf, 0x00, 0x00)
	_, err := conn.Write(buf)
	return err
}

func rconRecv(conn net.Conn) (length, reqID, typ int32, payload string, err error) {
	header := make([]byte, 12)
	if _, err = readFull(conn, header); err != nil {
		return
	}
	length = readInt32LE(header[0:4])
	reqID = readInt32LE(header[4:8])
	typ = readInt32LE(header[8:12])
	remaining := int(length) - 8
	if remaining < 2 || remaining > 4096 {
		err = fmt.Errorf("RCON 包长度异常: %d", remaining)
		return
	}
	data := make([]byte, remaining)
	if _, err = readFull(conn, data); err != nil {
		return
	}
	end := len(data)
	for end > 0 && data[end-1] == 0 {
		end--
	}
	payload = string(data[:end])
	return
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

func appendInt32LE(b []byte, v int32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func readInt32LE(b []byte) int32 {
	return int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16 | int32(b[3])<<24
}

func queryRCON(cfg ServiceConfig, timeout time.Duration) (online, max int, err error) {
	host := cfg.RconHost
	if host == "" {
		host = cfg.Host
	}
	port := cfg.RconPort
	if port == 0 {
		port = 25575
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), timeout)
	if err != nil {
		return 0, 0, fmt.Errorf("RCON 连接失败: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	if err = rconSend(conn, 1, 3, cfg.RconPassword); err != nil {
		return 0, 0, err
	}
	_, reqID, _, _, err := rconRecv(conn)
	if err != nil {
		return 0, 0, err
	}
	if reqID == -1 {
		return 0, 0, fmt.Errorf("RCON 密码错误")
	}
	if err = rconSend(conn, 2, 2, "list"); err != nil {
		return 0, 0, err
	}
	_, _, _, resp, err := rconRecv(conn)
	if err != nil {
		return 0, 0, err
	}
	log.Printf("[RCON %s] list → %q", cfg.Name, resp)

	var n int
	if n, err = fmt.Sscanf(resp, "There are %d of a max of %d players online", &online, &max); n == 2 {
		return online, max, nil
	}
	if idx := strings.Index(resp, "("); idx >= 0 {
		fmt.Sscanf(resp[idx:], "(%d/%d)", &online, &max)
		if max > 0 {
			return online, max, nil
		}
	}
	return 0, 0, fmt.Errorf("无法解析 list 响应: %s", resp)
}

// ========== Minecraft Bedrock ==========

func checkMinecraftBedrock(cfg ServiceConfig) ServiceStatus {
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout == 0 {
		timeout = 5 * time.Second
	}

	s := ServiceStatus{
		Name:      cfg.Name,
		Type:      cfg.Type,
		CheckedAt: time.Now().Format("15:04:05"),
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		s.Status = "offline"
		s.Message = "连接失败"
		return s
	}
	defer conn.Close()

	magic := []byte{0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe,
		0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78}
	ts := make([]byte, 8)
	binary.LittleEndian.PutUint64(ts, uint64(time.Now().UnixMilli()))
	guid := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}

	buildPing := func(id byte) []byte {
		p := append([]byte{id}, ts...)
		p = append(p, magic...)
		p = append(p, guid...)
		return p
	}

	buf := make([]byte, 2048)
	var n int
	start := time.Now()

	for _, pingID := range []byte{0x01, 0x02} {
		conn.SetDeadline(time.Now().Add(timeout))
		if _, err = conn.Write(buildPing(pingID)); err != nil {
			continue
		}
		n, err = conn.Read(buf)
		if err == nil && n > 33 {
			break
		}
	}

	elapsed := time.Since(start).Milliseconds()
	s.ResponseTime = elapsed

	if err != nil || n == 0 {
		s.Status = "offline"
		s.Message = "无响应"
		return s
	}

	if buf[0] != 0x1c {
		s.Status = "unknown"
		s.Message = fmt.Sprintf("响应异常(0x%02x)", buf[0])
		return s
	}

	s.Status = "online"
	s.Message = "在线"

	if cfg.RconPassword != "" {
		if online, max, rerr := queryRCON(cfg, timeout); rerr == nil {
			s.PlayerOnline = online
			s.PlayerMax = max
			s.HasPlayers = true
		} else {
			log.Printf("[RCON %s] %v", cfg.Name, rerr)
		}
	}

	const motdOffset = 1 + 8 + 8 + 16
	if n >= motdOffset+2 {
		strLen := int(binary.BigEndian.Uint16(buf[motdOffset : motdOffset+2]))
		end := motdOffset + 2 + strLen
		if end > n {
			end = n
		}
		if end > motdOffset+2 {
			parts := strings.Split(string(buf[motdOffset+2:end]), ";")
			if len(parts) >= 6 && !s.HasPlayers {
				if online, err2 := strconv.Atoi(strings.TrimSpace(parts[4])); err2 == nil {
					s.PlayerOnline = online
				}
				if max, err2 := strconv.Atoi(strings.TrimSpace(parts[5])); err2 == nil {
					s.PlayerMax = max
				}
				s.HasPlayers = true
			}
			if len(parts) >= 2 {
				s.Message = strings.TrimSpace(parts[1])
			}
		}
	}
	return s
}

// ========== Minecraft Java ==========

func checkMinecraftJava(cfg ServiceConfig) ServiceStatus {
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout == 0 {
		timeout = 5 * time.Second
	}
	s := ServiceStatus{
		Name:      cfg.Name,
		Type:      cfg.Type,
		CheckedAt: time.Now().Format("15:04:05"),
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	elapsed := time.Since(start).Milliseconds()
	s.ResponseTime = elapsed
	if err != nil {
		s.Status = "offline"
		s.Message = "连接失败"
		return s
	}
	conn.Close()
	s.Status = "online"
	s.Message = "端口开放"
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
	case "minecraft_bedrock":
		return checkMinecraftBedrock(cfg)
	case "minecraft_java":
		return checkMinecraftJava(cfg)
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

func refreshAllStatus() {
	refreshLock.Lock()
	defer refreshLock.Unlock()

	services := getServicesSnapshot()
	results := make([]ServiceStatus, len(services))
	var wg sync.WaitGroup
	for i, svc := range services {
		wg.Add(1)
		go func(idx int, s ServiceConfig) {
			defer wg.Done()
			r := checkService(s)
			r.Group = s.Group
			r.Subtitle = s.Subtitle
			h := getHistory(s.Name)
			now := time.Now()
			h.record(HistoryEntry{
				Status:       r.Status,
				ResponseTime: r.ResponseTime,
				Time:         now.Format("01-02 15:04"),
				Ts:           now.Unix(),
			})
			r.History, r.Uptime, r.CheckCount = h.snapshot()
			results[idx] = r
		}(i, svc)
	}
	wg.Wait()

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
		Interval:    120,
		Services:    lastStatus,
	}
	statusLock.RUnlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(resp)
}

func main() {
	cfg, err := loadConfigFile(configPath)
	if err != nil {
		log.Fatalf("加载 config.json 失败: %v", err)
	}
	setConfig(cfg)

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
	http.Handle("/", http.FileServer(http.Dir("web")))

	addr := ":" + getConfig().Port
	log.Printf("服务状态面板已启动 → http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
