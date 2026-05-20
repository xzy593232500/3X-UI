package service

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/util/json_util"
	"github.com/mhsanaei/3x-ui/v2/xray"

	"gorm.io/gorm"
)

const (
	defaultRelayPortMin = 30000
	defaultRelayPortMax = 50000
	relayModeDirect     = "direct"
)

type relayGrantRow struct {
	NodeID      int    `gorm:"column:node_id"`
	NodeName    string `gorm:"column:node_name"`
	Link        string `gorm:"column:link"`
	RelayPort   int    `gorm:"column:relay_port"`
	SubjectKey  string `gorm:"column:subject_key"`
	SubjectName string `gorm:"column:subject_name"`
	SubjectType string `gorm:"column:subject_type"`
}

type relayRuntimeNode struct {
	ID      int
	Name    string
	Link    string
	Port    int
	Clients map[string]relayRuntimeClient
}

type relayRuntimeClient struct {
	ID    string
	Email string
}

func SubscriptionRelayEnabled() bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("XUI_UPSTREAM_RELAY_MODE")))
	return mode != relayModeDirect && mode != "false" && mode != "0" && mode != "off"
}

func SubscriptionRelayPublicHost(requestHost string) string {
	if host := normalizeRelayHost(os.Getenv("XUI_RELAY_PUBLIC_HOST")); host != "" {
		return host
	}
	if host := publicHostFromBaseURL(os.Getenv("XUI_PUBLIC_SUB_BASE_URL")); host != "" {
		return host
	}
	return normalizeRelayHost(requestHost)
}

func (s *SubscriptionMarketService) ApplyRelayXrayConfig(config *xray.Config) error {
	if config == nil || !SubscriptionRelayEnabled() {
		return nil
	}
	if err := s.EnsureRelayPorts(); err != nil {
		return err
	}
	nodes, err := s.activeRelayRuntimeNodes()
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return nil
	}

	outbounds, err := rawOutboundConfigs(config.OutboundConfigs)
	if err != nil {
		return err
	}
	routing, rules, err := rawRoutingRules(config.RouterConfig)
	if err != nil {
		return err
	}

	for _, node := range nodes {
		outbound, ok := buildRelayVMessOutbound(node)
		if !ok {
			continue
		}
		inbound, ok := buildRelayVMessInbound(node)
		if !ok {
			continue
		}
		config.InboundConfigs = append(config.InboundConfigs, inbound)
		outbounds = append(outbounds, outbound)
		rules = prependAfterAPIRule(rules, map[string]any{
			"type":        "field",
			"inboundTag":  []string{relayInboundTag(node.ID)},
			"outboundTag": relayOutboundTag(node.ID),
		})
	}

	outboundBytes, err := json.MarshalIndent(outbounds, "", "  ")
	if err != nil {
		return err
	}
	config.OutboundConfigs = outboundBytes

	routing["rules"] = rules
	routingBytes, err := json.MarshalIndent(routing, "", "  ")
	if err != nil {
		return err
	}
	config.RouterConfig = routingBytes
	return nil
}

func (s *SubscriptionMarketService) DisableExpiredCustomers() (int64, error) {
	result := database.GetDB().
		Table("customer_subscriptions").
		Where("enable = ? AND expiry_time > 0 AND expiry_time <= ?", true, time.Now().UnixMilli()).
		Update("enable", false)
	return result.RowsAffected, result.Error
}

func (s *SubscriptionMarketService) EnsureRelayPorts() error {
	if !SubscriptionRelayEnabled() {
		return nil
	}
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		return s.ensureRelayPorts(tx)
	})
}

func (s *SubscriptionMarketService) ensureRelayPorts(tx *gorm.DB) error {
	var rows []struct {
		ID        int `gorm:"column:id"`
		RelayPort int `gorm:"column:relay_port"`
	}
	if err := tx.Table("upstream_nodes").
		Select("id, relay_port").
		Where("protocol = ? AND link <> ''", "vmess").
		Order("id asc").
		Scan(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if relayPortFromValue(row.RelayPort) > 0 {
			continue
		}
		port, err := s.allocateRelayPort(tx, row.ID)
		if err != nil {
			return err
		}
		if err := tx.Table("upstream_nodes").
			Where("id = ?", row.ID).
			Update("relay_port", port).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *SubscriptionMarketService) allocateRelayPort(tx *gorm.DB, excludeNodeID int) (int, error) {
	minPort, maxPort := relayPortRange()
	used := make(map[int]bool)
	var nodePorts []int
	nodeQuery := tx.Table("upstream_nodes").Where("relay_port > 0")
	if excludeNodeID > 0 {
		nodeQuery = nodeQuery.Where("id <> ?", excludeNodeID)
	}
	if err := nodeQuery.Pluck("relay_port", &nodePorts).Error; err != nil {
		return 0, err
	}
	for _, port := range nodePorts {
		if port >= minPort && port <= maxPort {
			used[port] = true
		}
	}

	var inboundPorts []int
	if err := tx.Table("inbounds").Where("port > 0").Pluck("port", &inboundPorts).Error; err != nil {
		return 0, err
	}
	for _, port := range inboundPorts {
		if port >= minPort && port <= maxPort {
			used[port] = true
		}
	}

	capacity := maxPort - minPort + 1
	if capacity <= len(used) {
		return 0, fmt.Errorf("no available upstream relay ports in %d-%d", minPort, maxPort)
	}
	for attempts := 0; attempts < 512; attempts++ {
		port, err := randomRelayPort(minPort, maxPort)
		if err != nil {
			return 0, err
		}
		if !used[port] {
			return port, nil
		}
	}
	for port := minPort; port <= maxPort; port++ {
		if !used[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available upstream relay ports in %d-%d", minPort, maxPort)
}

func (s *SubscriptionMarketService) activeRelayRuntimeNodes() ([]relayRuntimeNode, error) {
	rows, err := s.activeRelayGrantRows()
	if err != nil {
		return nil, err
	}
	byNode := make(map[int]*relayRuntimeNode)
	for _, row := range rows {
		if row.NodeID <= 0 || relayPortFromValue(row.RelayPort) <= 0 || strings.TrimSpace(row.Link) == "" {
			continue
		}
		node := byNode[row.NodeID]
		if node == nil {
			node = &relayRuntimeNode{
				ID:      row.NodeID,
				Name:    row.NodeName,
				Link:    row.Link,
				Port:    row.RelayPort,
				Clients: make(map[string]relayRuntimeClient),
			}
			byNode[row.NodeID] = node
		}
		clientID := relayClientUUID(row.SubjectType, row.SubjectKey, row.NodeID)
		if clientID == "" {
			continue
		}
		email := relayClientEmail(row.SubjectType, row.SubjectKey, row.NodeID)
		node.Clients[email] = relayRuntimeClient{ID: clientID, Email: email}
	}

	ids := make([]int, 0, len(byNode))
	for id := range byNode {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	nodes := make([]relayRuntimeNode, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, *byNode[id])
	}
	return nodes, nil
}

func (s *SubscriptionMarketService) activeRelayGrantRows() ([]relayGrantRow, error) {
	db := database.GetDB()
	now := time.Now().UnixMilli()
	var rows []relayGrantRow

	customerQuery := db.Table("customer_subscription_nodes").
		Select(`
			upstream_nodes.id AS node_id,
			upstream_nodes.name AS node_name,
			upstream_nodes.link,
			upstream_nodes.relay_port,
			customer_subscriptions.token AS subject_key,
			customer_subscriptions.name AS subject_name,
			'customer' AS subject_type`).
		Joins("JOIN customer_subscriptions ON customer_subscriptions.id = customer_subscription_nodes.customer_id").
		Joins("JOIN upstream_nodes ON upstream_nodes.id = customer_subscription_nodes.node_id").
		Joins("JOIN upstream_subscriptions ON upstream_subscriptions.id = upstream_nodes.upstream_id").
		Where("customer_subscriptions.enable = ?", true).
		Where("(customer_subscriptions.expiry_time = 0 OR customer_subscriptions.expiry_time > ?)", now).
		Where("upstream_nodes.enable = ? AND upstream_subscriptions.enable = ?", true, true).
		Where("upstream_nodes.protocol = ? AND upstream_nodes.link <> ''", "vmess")
	if err := customerQuery.Scan(&rows).Error; err != nil {
		return nil, err
	}

	var inboundRows []relayGrantRow
	inboundQuery := `
		SELECT DISTINCT
			upstream_nodes.id AS node_id,
			upstream_nodes.name AS node_name,
			upstream_nodes.link AS link,
			upstream_nodes.relay_port AS relay_port,
			JSON_EXTRACT(client.value, '$.subId') AS subject_key,
			COALESCE(JSON_EXTRACT(client.value, '$.email'), JSON_EXTRACT(client.value, '$.subId')) AS subject_name,
			'inbound' AS subject_type
		FROM inbounds
		JOIN inbound_subscription_nodes ON inbound_subscription_nodes.inbound_id = inbounds.id
		JOIN upstream_nodes ON upstream_nodes.id = inbound_subscription_nodes.node_id
		JOIN upstream_subscriptions ON upstream_subscriptions.id = upstream_nodes.upstream_id
		JOIN JSON_EACH(JSON_EXTRACT(inbounds.settings, '$.clients')) AS client
		LEFT JOIN client_traffics ON client_traffics.inbound_id = inbounds.id
			AND client_traffics.email = JSON_EXTRACT(client.value, '$.email')
		WHERE inbounds.enable = ?
			AND upstream_nodes.enable = ?
			AND upstream_subscriptions.enable = ?
			AND upstream_nodes.protocol = ?
			AND upstream_nodes.link <> ''
			AND COALESCE(JSON_EXTRACT(client.value, '$.enable'), 1) = 1
			AND COALESCE(JSON_EXTRACT(client.value, '$.subId'), '') <> ''
			AND (client_traffics.enable IS NULL OR client_traffics.enable = 1)
			AND (client_traffics.expiry_time IS NULL OR client_traffics.expiry_time = 0 OR client_traffics.expiry_time > ?)
	`
	if err := db.Raw(inboundQuery, true, true, true, "vmess", now).Scan(&inboundRows).Error; err != nil {
		return nil, err
	}
	rows = append(rows, inboundRows...)
	return rows, nil
}

func buildRelayVMessInbound(node relayRuntimeNode) (xray.InboundConfig, bool) {
	port := relayPortFromValue(node.Port)
	if port <= 0 || len(node.Clients) == 0 {
		return xray.InboundConfig{}, false
	}
	emails := make([]string, 0, len(node.Clients))
	for email := range node.Clients {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	clients := make([]any, 0, len(emails))
	for _, email := range emails {
		client := node.Clients[email]
		clients = append(clients, map[string]any{
			"id":       client.ID,
			"email":    client.Email,
			"alterId":  0,
			"security": "auto",
		})
	}
	settings := mustJSON(map[string]any{
		"clients":                   clients,
		"disableInsecureEncryption": false,
	})
	stream := mustJSON(map[string]any{
		"network":  "tcp",
		"security": "none",
		"tcpSettings": map[string]any{
			"acceptProxyProtocol": false,
			"header": map[string]any{
				"type": "none",
			},
		},
	})
	sniffing := mustJSON(map[string]any{
		"enabled":      true,
		"destOverride": []string{"http", "tls", "quic", "fakedns"},
	})
	return xray.InboundConfig{
		Listen:         json_util.RawMessage(`"0.0.0.0"`),
		Port:           port,
		Protocol:       "vmess",
		Settings:       settings,
		StreamSettings: stream,
		Tag:            relayInboundTag(node.ID),
		Sniffing:       sniffing,
	}, true
}

func buildRelayVMessOutbound(node relayRuntimeNode) (map[string]any, bool) {
	vmess, err := parseVMessShare(node.Link)
	if err != nil {
		return nil, false
	}
	address := vmessString(vmess, "add")
	port := vmessInt(vmess, "port")
	uuid := vmessString(vmess, "id")
	if address == "" || port <= 0 || uuid == "" {
		return nil, false
	}
	security := vmessString(vmess, "scy")
	if security == "" {
		security = "auto"
	}
	user := map[string]any{
		"id":       uuid,
		"alterId":  vmessInt(vmess, "aid"),
		"security": security,
	}
	outbound := map[string]any{
		"tag":      relayOutboundTag(node.ID),
		"protocol": "vmess",
		"settings": map[string]any{
			"vnext": []any{
				map[string]any{
					"address": address,
					"port":    port,
					"users":   []any{user},
				},
			},
		},
		"streamSettings": buildVMessOutboundStream(vmess),
	}
	return outbound, true
}

func buildVMessOutboundStream(vmess map[string]any) map[string]any {
	network := strings.ToLower(vmessString(vmess, "net"))
	if network == "" {
		network = "tcp"
	}
	if network == "h2" {
		network = "http"
	}
	security := strings.ToLower(vmessString(vmess, "tls"))
	if security == "" {
		security = "none"
	}
	stream := map[string]any{
		"network":  network,
		"security": security,
	}
	switch network {
	case "ws":
		ws := map[string]any{}
		if path := vmessString(vmess, "path"); path != "" {
			ws["path"] = path
		}
		if host := vmessString(vmess, "host"); host != "" {
			ws["headers"] = map[string]any{"Host": firstCSV(host)}
		}
		stream["wsSettings"] = ws
	case "grpc":
		grpc := map[string]any{}
		if serviceName := vmessString(vmess, "path"); serviceName != "" {
			grpc["serviceName"] = serviceName
		}
		if authority := vmessString(vmess, "host"); authority != "" {
			grpc["authority"] = firstCSV(authority)
		}
		stream["grpcSettings"] = grpc
	case "http":
		httpSettings := map[string]any{}
		if path := vmessString(vmess, "path"); path != "" {
			httpSettings["path"] = path
		}
		if host := vmessString(vmess, "host"); host != "" {
			httpSettings["host"] = splitCSV(host)
		}
		stream["httpSettings"] = httpSettings
	default:
		headerType := strings.ToLower(vmessString(vmess, "type"))
		header := map[string]any{"type": "none"}
		if headerType == "http" {
			request := map[string]any{}
			if path := vmessString(vmess, "path"); path != "" {
				request["path"] = []string{path}
			}
			if host := vmessString(vmess, "host"); host != "" {
				request["headers"] = map[string]any{"Host": splitCSV(host)}
			}
			header = map[string]any{"type": "http", "request": request}
		}
		stream["tcpSettings"] = map[string]any{"header": header}
	}
	if security == "tls" {
		tlsSettings := map[string]any{}
		serverName := vmessString(vmess, "sni")
		if serverName == "" {
			serverName = firstCSV(vmessString(vmess, "host"))
		}
		if serverName != "" {
			tlsSettings["serverName"] = serverName
		}
		if alpn := splitCSV(vmessString(vmess, "alpn")); len(alpn) > 0 {
			tlsSettings["alpn"] = alpn
		}
		if fp := vmessString(vmess, "fp"); fp != "" {
			tlsSettings["fingerprint"] = fp
		}
		stream["tlsSettings"] = tlsSettings
	}
	return stream
}

func buildRelayedUpstreamNodeContent(rows []upstreamNodeContentRow, relayHost string, clientIDForNode func(int) string) ([]string, []map[string]any) {
	relayHost = normalizeRelayHost(relayHost)
	if relayHost == "" || clientIDForNode == nil {
		return nil, nil
	}
	links := make([]string, 0, len(rows))
	clashProxies := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row.ID <= 0 || strings.ToLower(row.Protocol) != "vmess" || strings.TrimSpace(row.Link) == "" {
			continue
		}
		clientID := clientIDForNode(row.ID)
		if clientID == "" {
			continue
		}
		port := relayPortFromValue(row.RelayPort)
		if port <= 0 {
			continue
		}
		name := strings.TrimSpace(row.Name)
		if name == "" {
			name = fmt.Sprintf("Relay %d", row.ID)
		}
		obj := map[string]any{
			"v":    "2",
			"ps":   name,
			"add":  relayHost,
			"port": port,
			"id":   clientID,
			"aid":  "0",
			"scy":  "auto",
			"net":  "tcp",
			"type": "none",
			"host": "",
			"path": "",
			"tls":  "none",
		}
		links = append(links, buildVMessRelayLink(obj))
		clashProxies = append(clashProxies, map[string]any{
			"name":    name,
			"type":    "vmess",
			"server":  relayHost,
			"port":    port,
			"uuid":    clientID,
			"alterId": 0,
			"cipher":  "auto",
			"tls":     false,
			"network": "tcp",
		})
	}
	return links, clashProxies
}

func relayCustomerClientID(token string, nodeID int) string {
	return relayClientUUID("customer", token, nodeID)
}

func relayInboundClientID(subID string, nodeID int) string {
	return relayClientUUID("inbound", subID, nodeID)
}

func relayClientUUID(subjectType, subjectKey string, nodeID int) string {
	subjectKey = strings.TrimSpace(subjectKey)
	if subjectKey == "" || nodeID <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("3x-ui-relay:%s:%s:%d", subjectType, subjectKey, nodeID)))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func relayClientEmail(subjectType, subjectKey string, nodeID int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", subjectType, subjectKey, nodeID)))
	return fmt.Sprintf("relay-%s-%d-%x", subjectType, nodeID, sum[:4])
}

func relayPortFromValue(port int) int {
	if port < 1 || port > 65535 {
		return 0
	}
	return port
}

func relayPortRange() (int, int) {
	minPort, hasMin := relayEnvInt("XUI_RELAY_PORT_MIN")
	maxPort, hasMax := relayEnvInt("XUI_RELAY_PORT_MAX")
	if !hasMin && !hasMax {
		minPort = defaultRelayPortMin
		if base, ok := relayEnvInt("XUI_RELAY_PORT_BASE"); ok {
			minPort = base
		}
		maxPort = minPort + (defaultRelayPortMax - defaultRelayPortMin)
	} else {
		if !hasMin {
			minPort = defaultRelayPortMin
		}
		if !hasMax {
			maxPort = defaultRelayPortMax
		}
	}
	if minPort < 1 {
		minPort = 1
	}
	if maxPort > 65535 {
		maxPort = 65535
	}
	if maxPort < minPort {
		maxPort = minPort
	}
	return minPort, maxPort
}

func relayEnvInt(name string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return value, true
}

func randomRelayPort(minPort, maxPort int) (int, error) {
	if maxPort < minPort {
		return 0, fmt.Errorf("invalid relay port range %d-%d", minPort, maxPort)
	}
	span := big.NewInt(int64(maxPort - minPort + 1))
	offset, err := cryptorand.Int(cryptorand.Reader, span)
	if err != nil {
		return 0, err
	}
	return minPort + int(offset.Int64()), nil
}

func relayInboundTag(nodeID int) string {
	return fmt.Sprintf("xui-upstream-relay-in-%d", nodeID)
}

func relayOutboundTag(nodeID int) string {
	return fmt.Sprintf("xui-upstream-relay-out-%d", nodeID)
}

func parseVMessShare(link string) (map[string]any, error) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(link)), "vmess://") {
		return nil, fmt.Errorf("not a vmess link")
	}
	payload := strings.TrimSpace(link[len("vmess://"):])
	decoded, ok := decodeBase64Any(payload)
	if !ok {
		return nil, fmt.Errorf("invalid vmess payload")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(decoded), &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func buildVMessRelayLink(obj map[string]any) string {
	encoded, _ := json.Marshal(obj)
	return "vmess://" + base64.StdEncoding.EncodeToString(encoded)
}

func vmessString(obj map[string]any, key string) string {
	if obj == nil {
		return ""
	}
	switch value := obj[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		return strconv.FormatInt(int64(value), 10)
	case int:
		return strconv.Itoa(value)
	default:
		return ""
	}
}

func vmessInt(obj map[string]any, key string) int {
	switch value := obj[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(value))
		return parsed
	default:
		return 0
	}
}

func rawOutboundConfigs(raw json_util.RawMessage) ([]any, error) {
	var outbounds []any
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return outbounds, nil
	}
	if err := json.Unmarshal(raw, &outbounds); err != nil {
		return nil, err
	}
	return outbounds, nil
}

func rawRoutingRules(raw json_util.RawMessage) (map[string]any, []any, error) {
	routing := map[string]any{}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(raw, &routing); err != nil {
			return nil, nil, err
		}
	}
	var rules []any
	if existing, ok := routing["rules"].([]any); ok {
		rules = existing
	}
	return routing, rules, nil
}

func mustJSON(value any) json_util.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func normalizeRelayHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if strings.Contains(host, "://") {
		return publicHostFromBaseURL(host)
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	return host
}

func publicHostFromBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return normalizeRelayHost(raw)
	}
	return normalizeRelayHost(parsed.Host)
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func firstCSV(value string) string {
	parts := splitCSV(value)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}
