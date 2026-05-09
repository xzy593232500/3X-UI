package service

import (
	"encoding/json"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/util/json_util"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

func TestApplyInboundSocksProxyAddsOutboundAndRouting(t *testing.T) {
	config := &xray.Config{
		OutboundConfigs: json_util.RawMessage(`[{"tag":"direct","protocol":"freedom","settings":{}}]`),
		RouterConfig: json_util.RawMessage(`{
			"rules": [
				{"type":"field","inboundTag":["api"],"outboundTag":"api"},
				{"type":"field","outboundTag":"direct"}
			]
		}`),
	}
	inbound := &model.Inbound{
		Id:                 42,
		Tag:                "inbound-443",
		SocksProxyEnabled:  true,
		SocksProxyHost:     "proxy.example.com",
		SocksProxyPort:     1080,
		SocksProxyUsername: "user1",
		SocksProxyPassword: "pass1",
	}

	if err := applyInboundSocksProxy(config, inbound); err != nil {
		t.Fatalf("applyInboundSocksProxy() error = %v", err)
	}

	var outbounds []map[string]any
	if err := json.Unmarshal(config.OutboundConfigs, &outbounds); err != nil {
		t.Fatalf("unmarshal outbounds: %v", err)
	}
	if len(outbounds) != 2 {
		t.Fatalf("len(outbounds) = %d, want 2", len(outbounds))
	}
	gotOutbound := outbounds[1]
	if gotOutbound["tag"] != "xui-socks-inbound-42" || gotOutbound["protocol"] != "socks" {
		t.Fatalf("unexpected socks outbound: %#v", gotOutbound)
	}
	settings := gotOutbound["settings"].(map[string]any)
	server := settings["servers"].([]any)[0].(map[string]any)
	if server["address"] != "proxy.example.com" || int(server["port"].(float64)) != 1080 {
		t.Fatalf("unexpected socks server: %#v", server)
	}
	user := server["users"].([]any)[0].(map[string]any)
	if user["user"] != "user1" || user["pass"] != "pass1" {
		t.Fatalf("unexpected socks user: %#v", user)
	}

	var routing map[string]any
	if err := json.Unmarshal(config.RouterConfig, &routing); err != nil {
		t.Fatalf("unmarshal routing: %v", err)
	}
	rules := routing["rules"].([]any)
	if len(rules) != 3 {
		t.Fatalf("len(rules) = %d, want 3", len(rules))
	}
	gotRule := rules[1].(map[string]any)
	if gotRule["outboundTag"] != "xui-socks-inbound-42" {
		t.Fatalf("socks rule outboundTag = %v", gotRule["outboundTag"])
	}
	inboundTags := gotRule["inboundTag"].([]any)
	if len(inboundTags) != 1 || inboundTags[0] != "inbound-443" {
		t.Fatalf("socks rule inboundTag = %#v", inboundTags)
	}
}
