/*
 * Copyright (c) Huawei Technologies Co., Ltd. 2026-2027. All rights reserved.
 */

package sdk

import (
	devbridge "github.com/huaweicloud/devspace-devbridge/sdk"
	"huawei.com/devbridge/internal/auth"
	"huawei.com/devbridge/internal/config"
)

// ServerAddr WebSocket 网关地址（host:port），供 ldflags 注入。
var ServerAddr = "gateway.devbridge-s2.hwtunnel.com:443"

// ServerHost WebSocket 网关 SNI host，供 ldflags 注入。
var ServerHost = "devbridge-s2.hwtunnel.com"

// resolveGatewayAddr 解析 WebSocket 网关地址，优先级：
// 配置文件 gateway-addr > ldflags 注入的 ServerAddr。
func resolveGatewayAddr() string {
	if v := config.LoadGatewayAddr(); v != "" {
		return v
	}
	return ServerAddr
}

// resolveGatewayHost 解析 WebSocket 网关 SNI host，优先级：
// 配置文件 gateway-host > ldflags 注入的 ServerHost。
func resolveGatewayHost() string {
	if v := config.LoadGatewayHost(); v != "" {
		return v
	}
	return ServerHost
}

// NewClient 从 CLI 的认证体系创建 SDK 客户端。
//
// 读取顺序：override API Key → 环境变量 → keyring/config 存储。
// API base URL 和网关地址从 CLI 配置和 ldflags 注入值获取。
func NewClient() (*devbridge.Client, error) {
	cfg := devbridge.Config{
		APIBaseURL:  config.DefaultServerDomain + "/open-api-inner/v1/relay-controller",
		GatewayAddr: resolveGatewayAddr(),
		GatewayHost: resolveGatewayHost(),
	}

	if cred := auth.ReadValidAPIKey(); cred != nil && cred.APIKey != "" {
		cfg.APIKey = cred.APIKey
	}

	return devbridge.NewClient(cfg)
}
