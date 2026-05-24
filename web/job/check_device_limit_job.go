package job

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/websocket"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

const (
	deviceLimitActiveTTL = 3 * time.Minute
	deviceLimitGrace     = 3 * time.Minute
)

type deviceLimitInfo struct {
	Limit    int
	Port     int
	Tag      string
	Protocol model.Protocol
	Settings string
}

type deviceIPState struct {
	FirstSeen time.Time
	LastSeen  time.Time
}

// CheckDeviceLimitJob tracks active client IPs and temporarily invalidates
// users that exceed the inbound-level device limit.
type CheckDeviceLimitJob struct {
	runLock          sync.Mutex
	inboundService   service.InboundService
	xrayService      *service.XrayService
	xrayAPI          xray.XrayAPI
	lastPosition     int64
	activeClientIPs  map[string]map[string]time.Time
	activeInboundIPs map[int]map[string]deviceIPState
	bannedClients    map[string]bool
	bannedInboundIPs map[int]map[string]int
	violationStarted map[string]time.Time
}

func NewCheckDeviceLimitJob(xrayService *service.XrayService) *CheckDeviceLimitJob {
	return &CheckDeviceLimitJob{
		xrayService:      xrayService,
		activeClientIPs:  make(map[string]map[string]time.Time),
		activeInboundIPs: make(map[int]map[string]deviceIPState),
		bannedClients:    make(map[string]bool),
		bannedInboundIPs: make(map[int]map[string]int),
		violationStarted: make(map[string]time.Time),
	}
}

func (j *CheckDeviceLimitJob) Run() {
	if !j.runLock.TryLock() {
		return
	}
	defer j.runLock.Unlock()

	if j.xrayService == nil || !j.xrayService.IsXrayRunning() {
		return
	}

	j.cleanupExpiredIPs()
	j.parseAccessLog()
	j.checkInboundDeviceLimits()
	j.broadcastActiveInboundIPs()
}

func (j *CheckDeviceLimitJob) cleanupExpiredIPs() {
	now := time.Now()
	for email, ips := range j.activeClientIPs {
		for ip, lastSeen := range ips {
			if now.Sub(lastSeen) > deviceLimitActiveTTL {
				delete(ips, ip)
			}
		}
		if len(ips) == 0 {
			delete(j.activeClientIPs, email)
		}
	}
	for inboundID, ips := range j.activeInboundIPs {
		for ip, state := range ips {
			if now.Sub(state.LastSeen) > deviceLimitActiveTTL {
				delete(ips, ip)
				j.unbanInboundIP(inboundID, ip)
			}
		}
		if len(ips) == 0 {
			delete(j.activeInboundIPs, inboundID)
		}
	}
}

func (j *CheckDeviceLimitJob) parseAccessLog() {
	logPath, err := xray.GetAccessLogPath()
	if err != nil || logPath == "" || logPath == "none" {
		return
	}

	file, err := os.Open(logPath)
	if err != nil {
		return
	}
	defer file.Close()

	if stat, err := file.Stat(); err == nil && stat.Size() < j.lastPosition {
		j.lastPosition = 0
	}
	if _, err := file.Seek(j.lastPosition, 0); err != nil {
		j.lastPosition = 0
		return
	}

	tagToInboundID, emailToInboundID := j.deviceLimitLookupMaps()
	emailRegex := regexp.MustCompile(`email: ([^ ]+)`)
	ipRegex := regexp.MustCompile(`(?:from\s+)?(?:tcp:|udp:)?\[?([0-9a-fA-F\.:]+)\]?:\d+\s+accepted`)

	now := time.Now()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		emailMatch := emailRegex.FindStringSubmatch(line)
		ipMatch := ipRegex.FindStringSubmatch(line)
		if len(ipMatch) < 2 {
			continue
		}

		email := ""
		if len(emailMatch) >= 2 {
			email = emailMatch[1]
		}
		ip := ipMatch[1]
		if ip == "127.0.0.1" || ip == "::1" {
			continue
		}

		if email != "" {
			if _, ok := j.activeClientIPs[email]; !ok {
				j.activeClientIPs[email] = make(map[string]time.Time)
			}
			j.activeClientIPs[email][ip] = now
		}

		inboundID := j.inboundIDFromAccessLine(line, email, tagToInboundID, emailToInboundID)
		if inboundID > 0 {
			if _, ok := j.activeInboundIPs[inboundID]; !ok {
				j.activeInboundIPs[inboundID] = make(map[string]deviceIPState)
			}
			state, exists := j.activeInboundIPs[inboundID][ip]
			if !exists {
				state.FirstSeen = now
			}
			state.LastSeen = now
			j.activeInboundIPs[inboundID][ip] = state
		}
	}

	if pos, err := file.Seek(0, io.SeekCurrent); err == nil {
		j.lastPosition = pos
	}
}

func (j *CheckDeviceLimitJob) deviceLimitLookupMaps() (map[string]int, map[string]int) {
	tagToInboundID := make(map[string]int)
	emailToInboundID := make(map[string]int)

	var inbounds []*model.Inbound
	if err := database.GetDB().Where("enable = ? AND device_limit > ?", true, 0).Find(&inbounds).Error; err != nil {
		return tagToInboundID, emailToInboundID
	}
	for _, inbound := range inbounds {
		if inbound == nil {
			continue
		}
		tagToInboundID[inbound.Tag] = inbound.Id
		var settings map[string][]model.Client
		if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
			continue
		}
		for _, client := range settings["clients"] {
			if client.Email != "" {
				emailToInboundID[client.Email] = inbound.Id
			}
		}
	}
	return tagToInboundID, emailToInboundID
}

func (j *CheckDeviceLimitJob) inboundIDFromAccessLine(line, email string, tagToInboundID map[string]int, emailToInboundID map[string]int) int {
	for tag, inboundID := range tagToInboundID {
		if tag != "" && strings.Contains(line, tag) {
			return inboundID
		}
	}
	if email != "" {
		return emailToInboundID[email]
	}
	return 0
}

func (j *CheckDeviceLimitJob) checkInboundDeviceLimits() {
	db := database.GetDB()
	var inbounds []model.Inbound
	if err := db.Where("enable = ? AND device_limit > ?", true, 0).Find(&inbounds).Error; err != nil || len(inbounds) == 0 {
		return
	}

	infoByID := make(map[int]deviceLimitInfo, len(inbounds))
	for _, inbound := range inbounds {
		infoByID[inbound.Id] = deviceLimitInfo{
			Limit:    inbound.DeviceLimit,
			Port:     inbound.Port,
			Tag:      inbound.Tag,
			Protocol: inbound.Protocol,
			Settings: inbound.Settings,
		}
	}

	for inboundID, ips := range j.activeInboundIPs {
		info, ok := infoByID[inboundID]
		if !ok || info.Limit <= 0 {
			continue
		}

		active := make([]struct {
			IP    string
			State deviceIPState
		}, 0, len(ips))
		for ip, state := range ips {
			active = append(active, struct {
				IP    string
				State deviceIPState
			}{IP: ip, State: state})
		}
		sort.Slice(active, func(i, k int) bool {
			return active[i].State.FirstSeen.Before(active[k].State.FirstSeen)
		})

		allowed := make(map[string]bool, info.Limit)
		for index, entry := range active {
			if index < info.Limit {
				allowed[entry.IP] = true
				continue
			}
			if !j.isInboundIPBanned(inboundID, entry.IP) {
				j.banInboundIP(inboundID, entry.IP, &info, len(active))
			}
		}

		for ip := range j.bannedInboundIPs[inboundID] {
			if allowed[ip] || ips[ip].LastSeen.IsZero() {
				j.unbanInboundIP(inboundID, ip)
			}
		}
	}
}

func (j *CheckDeviceLimitJob) broadcastActiveInboundIPs() {
	if !websocket.HasClients() {
		return
	}

	payload := make(map[int][]map[string]any, len(j.activeInboundIPs))
	for inboundID, ips := range j.activeInboundIPs {
		if len(ips) == 0 {
			continue
		}

		items := make([]struct {
			IP    string
			State deviceIPState
		}, 0, len(ips))
		for ip, state := range ips {
			items = append(items, struct {
				IP    string
				State deviceIPState
			}{IP: ip, State: state})
		}
		sort.Slice(items, func(i, k int) bool {
			return items[i].State.FirstSeen.Before(items[k].State.FirstSeen)
		})

		rows := make([]map[string]any, 0, len(items))
		for _, item := range items {
			rows = append(rows, map[string]any{
				"ip":        item.IP,
				"firstSeen": item.State.FirstSeen.UnixMilli(),
				"lastSeen":  item.State.LastSeen.UnixMilli(),
				"banned":    j.isInboundIPBanned(inboundID, item.IP),
			})
		}
		payload[inboundID] = rows
	}

	websocket.BroadcastTraffic(map[string]any{
		"deviceLimitIPs": payload,
	})
}

func (j *CheckDeviceLimitJob) isInboundIPBanned(inboundID int, ip string) bool {
	if j.bannedInboundIPs[inboundID] == nil {
		return false
	}
	_, ok := j.bannedInboundIPs[inboundID][ip]
	return ok
}

func (j *CheckDeviceLimitJob) banInboundIP(inboundID int, ip string, info *deviceLimitInfo, activeIPCount int) {
	if ip == "" || info == nil || info.Port <= 0 {
		return
	}
	if j.bannedInboundIPs[inboundID] == nil {
		j.bannedInboundIPs[inboundID] = make(map[string]int)
	}
	if err := setInboundIPDropRule(ip, info.Port, true); err != nil {
		logger.Warningf("[DEVICE_LIMIT] Failed to block IP %s on inbound %s port %d: %v", ip, info.Tag, info.Port, err)
		return
	}
	logger.Warningf("[DEVICE_LIMIT] Blocking new device IP %s on inbound %s port %d: active=%d limit=%d", ip, info.Tag, info.Port, activeIPCount, info.Limit)
	j.bannedInboundIPs[inboundID][ip] = info.Port
}

func (j *CheckDeviceLimitJob) unbanInboundIP(inboundID int, ip string) {
	if ip == "" || j.bannedInboundIPs[inboundID] == nil {
		return
	}
	port, ok := j.bannedInboundIPs[inboundID][ip]
	if !ok {
		return
	}
	if port > 0 {
		_ = setInboundIPDropRule(ip, port, false)
	}
	delete(j.bannedInboundIPs[inboundID], ip)
	if len(j.bannedInboundIPs[inboundID]) == 0 {
		delete(j.bannedInboundIPs, inboundID)
	}
}

func setInboundIPDropRule(ip string, port int, ban bool) error {
	if ip == "" || port <= 0 {
		return nil
	}
	binary := "iptables"
	if strings.Contains(ip, ":") {
		binary = "ip6tables"
	}
	portArg := strconv.Itoa(port)
	var firstErr error
	for _, proto := range []string{"tcp", "udp"} {
		ruleArgs := []string{"INPUT", "-p", proto, "-s", ip, "--dport", portArg, "-j", "DROP"}
		if ban {
			checkArgs := append([]string{"-w", "-C"}, ruleArgs...)
			if err := exec.Command(binary, checkArgs...).Run(); err == nil {
				continue
			}
			insertArgs := append([]string{"-w", "-I"}, ruleArgs...)
			if err := exec.Command(binary, insertArgs...).Run(); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		deleteArgs := append([]string{"-w", "-D"}, ruleArgs...)
		for {
			if err := exec.Command(binary, deleteArgs...).Run(); err != nil {
				break
			}
		}
	}
	return firstErr
}

func (j *CheckDeviceLimitJob) writeIPLimitLog(label, ip string) {
	logIpFile, err := os.OpenFile(xray.GetIPLimitLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Warningf("[DEVICE_LIMIT] Failed to write fail2ban log: %v", err)
		return
	}
	defer logIpFile.Close()
	_, _ = fmt.Fprintf(logIpFile, "%s [LIMIT_IP] Email = %s || Disconnecting OLD IP = %s || Timestamp = %d\n",
		time.Now().Format("2006/01/02 15:04:05"), label, ip, time.Now().Unix())
}

func (j *CheckDeviceLimitJob) checkAllClientsLimit() {
	db := database.GetDB()
	var inbounds []*model.Inbound
	if err := db.Where("enable = ?", true).Find(&inbounds).Error; err != nil || len(inbounds) == 0 {
		return
	}

	inboundInfo := make(map[int]deviceLimitInfo, len(inbounds))
	for _, inbound := range inbounds {
		inboundInfo[inbound.Id] = deviceLimitInfo{
			Limit:    inbound.DeviceLimit,
			Port:     inbound.Port,
			Tag:      inbound.Tag,
			Protocol: inbound.Protocol,
			Settings: inbound.Settings,
		}
	}

	apiPort := resolveDeviceLimitAPIPort()
	if err := j.xrayAPI.Init(apiPort); err != nil {
		logger.Warningf("[DEVICE_LIMIT] Failed to init Xray API: %v", err)
		return
	}
	defer j.xrayAPI.Close()

	for email, ips := range j.activeClientIPs {
		info, ok := j.lookupLimitInfo(email, inboundInfo)
		if !ok {
			continue
		}

		activeIPCount := len(ips)
		isBanned := j.bannedClients[email]
		if info.Limit <= 0 {
			delete(j.violationStarted, email)
			if isBanned {
				j.unbanUser(email, activeIPCount, info)
			}
			continue
		}

		if activeIPCount > info.Limit && !isBanned {
			startedAt, exists := j.violationStarted[email]
			if !exists {
				j.violationStarted[email] = time.Now()
				logger.Infof("[DEVICE_LIMIT] Client %s exceeded device limit (%d > %d), entering grace period", email, activeIPCount, info.Limit)
				continue
			}
			if time.Since(startedAt) < deviceLimitGrace {
				continue
			}

			delete(j.violationStarted, email)
			j.banUser(email, activeIPCount, info)
		}

		if activeIPCount <= info.Limit {
			delete(j.violationStarted, email)
			if isBanned {
				j.unbanUser(email, activeIPCount, info)
			}
		}
	}

	for email, isBanned := range j.bannedClients {
		if !isBanned {
			continue
		}
		if _, online := j.activeClientIPs[email]; online {
			continue
		}
		info, ok := j.lookupLimitInfo(email, inboundInfo)
		if ok {
			j.unbanUser(email, 0, info)
		}
	}
}

func (j *CheckDeviceLimitJob) lookupLimitInfo(email string, inboundInfo map[int]deviceLimitInfo) (*deviceLimitInfo, bool) {
	traffic, err := j.inboundService.GetClientTrafficByEmail(email)
	if err != nil || traffic == nil {
		return nil, false
	}
	info, ok := inboundInfo[traffic.InboundId]
	if !ok {
		return nil, false
	}
	return &info, true
}

func (j *CheckDeviceLimitJob) banUser(email string, activeIPCount int, info *deviceLimitInfo) {
	traffic, client, err := j.inboundService.GetClientByEmail(email)
	if err != nil || client == nil || !client.Enable || (traffic != nil && !traffic.Enable) {
		return
	}
	if !supportsDeviceLimitProtocol(info.Protocol) {
		logger.Warningf("[DEVICE_LIMIT] Protocol %s is not supported for device-limit blocking", info.Protocol)
		return
	}

	clientMap, err := clientToAPIUser(client, info.Protocol, info.Settings, true)
	if err != nil {
		logger.Warningf("[DEVICE_LIMIT] Failed to build temporary blocked client for %s: %v", email, err)
		return
	}

	logger.Infof("[DEVICE_LIMIT] Blocking client %s: limit=%d active_ips=%d", email, info.Limit, activeIPCount)
	if err := j.xrayAPI.RemoveUser(info.Tag, email); err != nil {
		logger.Warningf("[DEVICE_LIMIT] Failed to remove client %s before blocking: %v", email, err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := j.xrayAPI.AddUser(string(info.Protocol), info.Tag, clientMap); err != nil {
		logger.Warningf("[DEVICE_LIMIT] Failed to add temporary blocked client %s: %v", email, err)
		return
	}
	j.bannedClients[email] = true
}

func (j *CheckDeviceLimitJob) unbanUser(email string, activeIPCount int, info *deviceLimitInfo) {
	traffic, client, err := j.inboundService.GetClientByEmail(email)
	if err != nil || client == nil {
		return
	}
	if !supportsDeviceLimitProtocol(info.Protocol) {
		delete(j.bannedClients, email)
		return
	}

	logger.Infof("[DEVICE_LIMIT] Restoring client %s: limit=%d active_ips=%d", email, info.Limit, activeIPCount)
	if err := j.xrayAPI.RemoveUser(info.Tag, email); err != nil {
		logger.Warningf("[DEVICE_LIMIT] Failed to remove temporary blocked client %s: %v", email, err)
	}
	delete(j.bannedClients, email)

	if !client.Enable || (traffic != nil && !traffic.Enable) {
		return
	}

	clientMap, err := clientToAPIUser(client, info.Protocol, info.Settings, false)
	if err != nil {
		logger.Warningf("[DEVICE_LIMIT] Failed to rebuild client %s: %v", email, err)
		return
	}
	time.Sleep(100 * time.Millisecond)
	if err := j.xrayAPI.AddUser(string(info.Protocol), info.Tag, clientMap); err != nil {
		logger.Warningf("[DEVICE_LIMIT] Failed to restore client %s: %v", email, err)
	}
}

func clientToAPIUser(client *model.Client, protocol model.Protocol, inboundSettings string, blocked bool) (map[string]any, error) {
	apiClient := *client
	if blocked {
		if apiClient.ID != "" {
			apiClient.ID = uuid.NewString()
		}
		if apiClient.Password != "" {
			apiClient.Password = uuid.NewString()
		}
		if apiClient.Auth != "" {
			apiClient.Auth = uuid.NewString()
		}
	}

	clientJSON, err := json.Marshal(apiClient)
	if err != nil {
		return nil, err
	}
	var clientMap map[string]any
	if err := json.Unmarshal(clientJSON, &clientMap); err != nil {
		return nil, err
	}

	if protocol == model.Shadowsocks {
		var settings map[string]any
		if err := json.Unmarshal([]byte(inboundSettings), &settings); err == nil {
			if method, ok := settings["method"].(string); ok && method != "" {
				clientMap["cipher"] = method
			}
		}
	}

	return clientMap, nil
}

func supportsDeviceLimitProtocol(protocol model.Protocol) bool {
	switch protocol {
	case model.VMESS, model.VLESS, model.Trojan, model.Shadowsocks, model.Hysteria, model.Hysteria2:
		return true
	default:
		return false
	}
}

func resolveDeviceLimitAPIPort() int {
	if port, err := getAPIPortFromConfigPath(xray.GetConfigPath()); err == nil {
		return port
	}
	return defaultXrayAPIPort
}
