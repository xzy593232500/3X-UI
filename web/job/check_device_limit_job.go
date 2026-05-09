package job

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

const (
	deviceLimitActiveTTL = 3 * time.Minute
	deviceLimitGrace     = 3 * time.Minute
)

type deviceLimitInfo struct {
	Limit    int
	Tag      string
	Protocol model.Protocol
	Settings string
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
	bannedClients    map[string]bool
	violationStarted map[string]time.Time
}

func NewCheckDeviceLimitJob(xrayService *service.XrayService) *CheckDeviceLimitJob {
	return &CheckDeviceLimitJob{
		xrayService:      xrayService,
		activeClientIPs:  make(map[string]map[string]time.Time),
		bannedClients:    make(map[string]bool),
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
	j.checkAllClientsLimit()
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

	emailRegex := regexp.MustCompile(`email: ([^ ]+)`)
	ipRegex := regexp.MustCompile(`from (?:tcp:|udp:)?\[?([0-9a-fA-F\.:]+)\]?:\d+ accepted`)

	now := time.Now()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		emailMatch := emailRegex.FindStringSubmatch(line)
		ipMatch := ipRegex.FindStringSubmatch(line)
		if len(emailMatch) < 2 || len(ipMatch) < 2 {
			continue
		}

		email := emailMatch[1]
		ip := ipMatch[1]
		if ip == "127.0.0.1" || ip == "::1" {
			continue
		}

		if _, ok := j.activeClientIPs[email]; !ok {
			j.activeClientIPs[email] = make(map[string]time.Time)
		}
		j.activeClientIPs[email][ip] = now
	}

	if pos, err := file.Seek(0, os.SEEK_CUR); err == nil {
		j.lastPosition = pos
	}
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
