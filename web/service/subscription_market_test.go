package service

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func TestParseClashNodesBuildsDirectTLSVLESSLink(t *testing.T) {
	nodes := parseClashNodes(`
proxies:
  - name: HK TLS
    type: vless
    server: edge.example.com
    port: 443
    uuid: 11111111-1111-1111-1111-111111111111
    tls: true
    servername: edge.example.com
    network: tcp
    client-fingerprint: chrome
    alpn:
      - h2
      - http/1.1
`)
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].Link == "" {
		t.Fatal("expected Clash VLESS node to also have a share link")
	}
	parsed, err := url.Parse(nodes[0].Link)
	if err != nil {
		t.Fatalf("parse share link: %v", err)
	}
	if parsed.Scheme != "vless" || parsed.Host != "edge.example.com:443" {
		t.Fatalf("unexpected VLESS link: %s", nodes[0].Link)
	}
	query := parsed.Query()
	if query.Get("security") != "tls" || query.Get("type") != "tcp" || query.Get("sni") != "edge.example.com" {
		t.Fatalf("missing TLS params in %s", nodes[0].Link)
	}
	if query.Get("fp") != "chrome" || query.Get("alpn") != "h2,http/1.1" {
		t.Fatalf("missing client TLS details in %s", nodes[0].Link)
	}
}

func TestParseClashNodesBuildsDirectTLSVMessLink(t *testing.T) {
	nodes := parseClashNodes(`
proxies:
  - name: VMess TLS
    type: vmess
    server: vmess.example.com
    port: 443
    uuid: 22222222-2222-2222-2222-222222222222
    alterId: 0
    cipher: auto
    tls: true
    servername: vmess.example.com
    network: tcp
`)
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if !strings.HasPrefix(nodes[0].Link, "vmess://") {
		t.Fatalf("expected vmess share link, got %q", nodes[0].Link)
	}
	payload := strings.TrimPrefix(nodes[0].Link, "vmess://")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode vmess payload: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(decoded, &obj); err != nil {
		t.Fatalf("unmarshal vmess payload: %v", err)
	}
	if obj["add"] != "vmess.example.com" || obj["tls"] != "tls" || obj["net"] != "tcp" {
		t.Fatalf("unexpected vmess payload: %+v", obj)
	}
}

func TestParseClashNodesBuildsHysteria2Link(t *testing.T) {
	nodes := parseClashNodes(`
proxies:
  - name: HY2 Node
    type: hysteria2
    server: hy2.example.com
    port: 8443
    password: secret-pass
    sni: hy2.example.com
    skip-cert-verify: true
    obfs: salamander
    obfs-password: obfs-pass
    alpn:
      - h3
`)
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].Protocol != "hysteria2" {
		t.Fatalf("expected hysteria2 protocol, got %q", nodes[0].Protocol)
	}
	parsed, err := url.Parse(nodes[0].Link)
	if err != nil {
		t.Fatalf("parse share link: %v", err)
	}
	if parsed.Scheme != "hysteria2" || parsed.Host != "hy2.example.com:8443" {
		t.Fatalf("unexpected hysteria2 link: %s", nodes[0].Link)
	}
	if password := parsed.User.Username(); password != "secret-pass" {
		t.Fatalf("unexpected hysteria2 password: %q", password)
	}
	query := parsed.Query()
	if query.Get("security") != "tls" || query.Get("sni") != "hy2.example.com" || query.Get("insecure") != "1" {
		t.Fatalf("missing hysteria2 TLS params in %s", nodes[0].Link)
	}
	if query.Get("obfs") != "salamander" || query.Get("obfs-password") != "obfs-pass" || query.Get("alpn") != "h3" {
		t.Fatalf("missing hysteria2 options in %s", nodes[0].Link)
	}
}

func TestParseURINodesSkipsSubscriptionInfoVMessNodes(t *testing.T) {
	info := map[string]any{
		"v":    "2",
		"ps":   "剩余流量：101.17GB",
		"add":  "example.com",
		"port": "80",
		"id":   "513faacc-68f8-3577-8f53-b10dc068c8ea",
		"aid":  "0",
		"scy":  "auto",
		"net":  "tcp",
		"type": "none",
		"tls":  "",
	}
	valid := map[string]any{
		"v":    "2",
		"ps":   "HK Node",
		"add":  "node.example.com",
		"port": "443",
		"id":   "22222222-2222-2222-2222-222222222222",
		"aid":  "0",
		"scy":  "auto",
		"net":  "tcp",
		"type": "none",
		"tls":  "tls",
	}
	infoJSON, _ := json.Marshal(info)
	validJSON, _ := json.Marshal(valid)
	nodes := parseURINodes("vmess://" + base64.StdEncoding.EncodeToString(infoJSON) + "\n" +
		"vmess://" + base64.StdEncoding.EncodeToString(validJSON))
	if len(nodes) != 1 {
		t.Fatalf("expected only valid node, got %d", len(nodes))
	}
	if nodes[0].Name != "HK Node" {
		t.Fatalf("unexpected node name: %q", nodes[0].Name)
	}
}
