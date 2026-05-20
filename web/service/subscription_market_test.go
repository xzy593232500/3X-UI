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
