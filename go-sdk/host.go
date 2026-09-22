/*
 * Copyright (c) Huawei Technologies Co., Ltd. 2026-2027. All rights reserved.
 */

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/microsoft/dev-tunnels-ssh/src/go/ssh"
	"github.com/microsoft/dev-tunnels-ssh/src/go/tcp"
)

// HostConfig is the configuration for hosting a tunnel.
type HostConfig struct {
	TunnelID string // tunnel ID
	Ports    []int  // local ports to forward (empty means issued by the gateway)
	JWTToken string // JWT token (mutually exclusive with APIKey)
	APIKey   string // API key (mutually exclusive with JWTToken)

	// OnReady runs once the session is ready, with the hosted ports. May be nil.
	OnReady func(ports []int)
}

type relayPortMessage struct {
	Type  string   `json:"@type"`
	Ports []uint16 `json:"ports"`
}

var (
	hostKeyOnce       sync.Once
	persistentHostKey ssh.KeyPair
	hostKeyErr        error
)

// ensureHostKey generates a process-wide SSH host key.
// If generation fails, the error is cached and returned on subsequent calls, avoiding silent use of a nil key.
func ensureHostKey() error {
	hostKeyOnce.Do(func() {
		persistentHostKey, hostKeyErr = ssh.GenerateKeyPair(ssh.AlgoPKEcdsaSha2P256)
	})
	return hostKeyErr
}

// Host starts the Host hosting service.
//
// This is a blocking call that returns when ctx is canceled or the connection is fully lost;
// brief network interruptions are reconnected automatically.
//
// Workflow:
//  1. Connect WebSocket to wss://<tunnelId>.<gatewayHost>/<tunnelId>
//  2. Establish an SSH session over the WebSocket
//  3. Accept relay channels and create an inner SSH session for each
//  4. Forward traffic from the remote side to the local port
func (d *Devbridge) Host(ctx context.Context, cfg HostConfig) error {
	if err := ensureHostKey(); err != nil {
		return fmt.Errorf("generate host key: %w", err)
	}
	if err := validateTunnelID(cfg.TunnelID); err != nil {
		return err
	}

	// Auth fallback: use the Devbridge instance's API key when cfg does not specify one.
	apiKey := cfg.APIKey
	if apiKey == "" && cfg.JWTToken == "" {
		apiKey = d.apiKey
	}

	header, subprotocols := buildWSHeader(cfg.JWTToken, apiKey)
	header.Set("Cookie", "APP_COOKIE=7")

	sniHost := cfg.TunnelID + "." + d.gatewayHost
	wsURL := "wss://" + sniHost + "/" + cfg.TunnelID

	params := sessionParams{
		wsURL:        wsURL,
		sniHost:      sniHost,
		header:       header,
		subprotocols: subprotocols,
		tunnelID:     cfg.TunnelID,
		ports:        cfg.Ports,
	}

	everConnected := false
	return d.reconnectLoop(ctx, func() (bool, error) {
		return d.runHostSession(ctx, params, cfg.OnReady)
	}, reconnectDecision{
		// Abort on gateway rejection (quota/tunnel) or a duplicate host before the first
		// connection. onConnected runs after shouldStop, so everConnected is the prior value.
		shouldStop: func(err error) bool {
			if errors.Is(err, ErrDuplicateHost) && !everConnected {
				d.logger.Debug("duplicate host, tunnel already has a listener", "tunnelID", cfg.TunnelID)
				return true
			}
			if gatewayRejectedError(err) {
				d.logger.Debug("connection rejected by gateway", "tunnelID", cfg.TunnelID, "err", err)
				return true
			}
			return false
		},
		onReconnect: func(err error) {
			d.statusln("Connection lost, reconnecting...")
		},
		onConnected: func() {
			everConnected = true
		},
	})
}

// hostSessionDiagnostics holds a host session's shutdown state: a one-shot
// disconnect signal and the last close reason captured for logging.
type hostSessionDiagnostics struct {
	disconnected chan struct{}
	closedMu     sync.Mutex
	closedArgs   *ssh.SessionClosedEventArgs
}

func newHostSessionDiagnostics() *hostSessionDiagnostics {
	return &hostSessionDiagnostics{disconnected: make(chan struct{}, 1)}
}

func (d *Devbridge) runHostSession(ctx context.Context, params sessionParams, onReady func([]int)) (connected bool, err error) {
	outerSession, netConn, err := d.connectOuterSession(ctx, params)
	if err != nil {
		return false, err
	}
	defer func() { _ = netConn.Close() }()

	d.logger.Debug("host: outer SSH session established", "tunnelID", params.tunnelID)

	diag := newHostSessionDiagnostics()
	d.installSessionCallbacks(outerSession, params.tunnelID, diag)

	pn := newPortNotifier()
	go d.startHostAcceptLoop(ctx, outerSession, params.ports, pn)

	if len(params.ports) == 0 {
		if err := d.waitForGatewayPorts(ctx, outerSession, params.tunnelID, pn, diag); err != nil {
			return true, err
		}
	}

	printPorts := params.ports
	if len(params.ports) == 0 {
		printPorts = pn.ports
	}
	d.reportReady(params.tunnelID, printPorts, onReady)

	return d.waitSessionEnd(ctx, outerSession, params.tunnelID, diag)
}

func (d *Devbridge) connectOuterSession(ctx context.Context, params sessionParams) (*ssh.ClientSession, net.Conn, error) {
	netConn, err := d.dialWebSocket(ctx, params.wsURL, params.sniHost, params.header, params.subprotocols, 5)
	if err != nil {
		if errors.Is(err, ErrDuplicateHost) {
			return nil, nil, fmt.Errorf("outer SSH connect failed: %w", err)
		}
		return nil, nil, err
	}

	outerConfig := ssh.NewNoSecurityConfig()
	outerConfig.KeepAliveIntervalSeconds = 10
	outerConfig.KeyRotationThreshold = 0
	tcp.AddPortForwardingService(outerConfig)
	outerSession := ssh.NewClientSession(outerConfig)
	outerSession.Trace = sshTraceFunc(d.logger)

	if err := outerSession.Connect(ctx, netConn); err != nil {
		_ = netConn.Close()
		return nil, nil, fmt.Errorf("outer SSH connect failed: %w", parseSSHCloseError(err))
	}
	return outerSession, netConn, nil
}

func (d *Devbridge) installSessionCallbacks(outerSession *ssh.ClientSession, tunnelID string, diag *hostSessionDiagnostics) {
	outerSession.OnDisconnected = func() {
		select {
		case diag.disconnected <- struct{}{}:
		default:
		}
	}
	outerSession.OnClosed = func(args *ssh.SessionClosedEventArgs) {
		diag.closedMu.Lock()
		diag.closedArgs = args
		diag.closedMu.Unlock()
		d.logger.Debug("host: session closed (diagnostic)",
			"tunnelID", tunnelID,
			"reason", args.Reason,
			"message", args.Message,
			"err", args.Err)
	}
	outerSession.OnKeepAliveFailed = func(count int) {
		if count >= 5 {
			d.logger.Error("keepalive failed 5 times, forcing reconnect", "tunnelID", tunnelID)
			_ = outerSession.Close()
		}
	}
}

func (d *Devbridge) waitForGatewayPorts(ctx context.Context, outerSession *ssh.ClientSession, tunnelID string, pn *portNotifier, diag *hostSessionDiagnostics) error {
	select {
	case <-pn.ready:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		d.logger.Warn("timeout waiting for port notification from gateway")
	case <-diag.disconnected:
		d.logDiagnosticClose(tunnelID, diag)
		return fmt.Errorf("disconnected")
	case <-outerSession.Session.Done():
		d.logDiagnosticClose(tunnelID, diag)
		return fmt.Errorf("session closed")
	}
	return nil
}

func (d *Devbridge) reportReady(tunnelID string, ports []int, onReady func([]int)) {
	realPorts := filterForwardPorts(ports)
	if len(realPorts) == 0 && len(ports) > 0 {
		d.statusln("All ports mode: this tunnel accepts any port via URL")
		d.statusf("Access your service at: https://%s-<port>.%s\n", tunnelID, d.gatewayHost)
	}
	for _, p := range realPorts {
		d.statusf("Hosting port: %s%d%s\n", colorCyan, p, colorReset)
	}
	for _, p := range realPorts {
		d.statusf("Tunnel URL: https://%s-%d.%s\n", tunnelID, p, d.gatewayHost)
	}
	d.statusln("Ready to accept connections")
	d.statusln("Auto reconnect: enabled")
	if onReady != nil {
		onReady(realPorts)
	}
}

func (d *Devbridge) waitSessionEnd(ctx context.Context, outerSession *ssh.ClientSession, tunnelID string, diag *hostSessionDiagnostics) (bool, error) {
	for {
		select {
		case <-diag.disconnected:
			d.logDiagnosticClose(tunnelID, diag)
			return true, fmt.Errorf("disconnected")
		case <-outerSession.Session.Done():
			d.logDiagnosticClose(tunnelID, diag)
			return true, fmt.Errorf("session closed")
		case <-ctx.Done():
			return true, nil
		}
	}
}

// logDiagnosticClose logs why the host connection was lost (diagnostic only).
func (d *Devbridge) logDiagnosticClose(tunnelID string, diag *hostSessionDiagnostics) {
	diag.closedMu.Lock()
	a := diag.closedArgs
	diag.closedMu.Unlock()
	if a == nil {
		d.logger.Debug("host connection lost (no close event captured)", "tunnelID", tunnelID)
		return
	}
	d.logger.Debug("host connection lost",
		"tunnelID", tunnelID,
		"reason", a.Reason,
		"message", a.Message,
		"err", a.Err)
}

type portNotifier struct {
	ready    chan struct{}
	ports    []int
	received atomic.Bool
}

func newPortNotifier() *portNotifier {
	return &portNotifier{ready: make(chan struct{})}
}

func (d *Devbridge) startHostAcceptLoop(ctx context.Context, outerSession *ssh.ClientSession, ports []int, pn *portNotifier) {
	for {
		channel, err := outerSession.AcceptChannel(ctx)
		if err != nil {
			return
		}

		switch channel.ChannelType {
		case relayChannelType:
			if !pn.received.Load() {
				// Treat the first relay channel as the gateway's port notification only when it
				// parses as a RelayRequest; otherwise forward it as normal data.
				notifPorts, isNotif := readPortNotification(channel)
				if isNotif {
					if len(ports) == 0 {
						pn.ports = notifPorts
					}
					pn.received.Store(true)
					close(pn.ready)
					continue
				}
			}

			effectivePorts := ports
			if len(effectivePorts) == 0 {
				effectivePorts = pn.ports
			}
			go d.handleRelayChannel(ctx, channel, effectivePorts)
		default:
			d.logger.Debug("host: draining non-relay channel",
				"channelType", channel.ChannelType, "channelID", channel.ChannelID)
		}
	}
}

// readPortNotification reports whether a relay channel is the gateway's port
// notification; callers forward the channel as data when it returns false.
func readPortNotification(channel *ssh.Channel) ([]int, bool) {
	stream := ssh.NewStream(channel)
	buf := make([]byte, 4096)
	n, err := stream.Read(buf)
	if err != nil {
		return nil, false
	}
	return parsePortNotification(buf[:n])
}

// parsePortNotification returns the ports of a RelayRequest payload, or false when
// the payload is not a valid port notification.
func parsePortNotification(data []byte) ([]int, bool) {
	var msg relayPortMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, false
	}
	if msg.Type != "RelayRequest" {
		return nil, false
	}
	ports := make([]int, len(msg.Ports))
	for i, p := range msg.Ports {
		ports[i] = int(p)
	}
	return ports, true
}

func (d *Devbridge) handleRelayChannel(ctx context.Context, channel *ssh.Channel, ports []int) {
	innerConfig := ssh.NewNoSecurityConfig()
	tcp.AddPortForwardingService(innerConfig)
	innerSession := ssh.NewServerSession(innerConfig)
	innerSession.Credentials = &ssh.ServerCredentials{PublicKeys: []ssh.KeyPair{persistentHostKey}}
	innerSession.Trace = sshTraceFunc(d.logger)

	if err := innerSession.Connect(ctx, ssh.NewStream(channel)); err != nil {
		d.logger.Error("inner SSH session failed", "channelID", channel.ChannelID, "err", err)
		return
	}
	d.logger.Debug("host: inner SSH server session established", "channelID", channel.ChannelID)

	pfs := tcp.GetPortForwardingService(&innerSession.Session)
	if pfs != nil && len(ports) > 0 {
		// Filter out the "all ports" sentinel value (-1): it is not a valid listening port and should not be forwarded.
		for _, port := range filterForwardPorts(ports) {
			if _, err := pfs.ForwardFromRemotePort(ctx, "127.0.0.1", port, "127.0.0.1", port); err != nil {
				d.logger.Error("forward port failed", "port", port, "err", err)
			}
		}
	}

	go func() {
		defer func() { _ = innerSession.Close() }()
		for {
			if _, err := innerSession.AcceptChannel(ctx); err != nil {
				return
			}
		}
	}()
}
