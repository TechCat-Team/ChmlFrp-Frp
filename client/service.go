// Copyright 2017 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/golib/crypto"
	libdial "github.com/fatedier/golib/net/dial"
	fmux "github.com/hashicorp/yamux"
	quic "github.com/quic-go/quic-go"

	"github.com/fatedier/frp/assets"
	"github.com/fatedier/frp/pkg/auth"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/transport"
	"github.com/fatedier/frp/pkg/util/log"
	utilnet "github.com/fatedier/frp/pkg/util/net"
	"github.com/fatedier/frp/pkg/util/util"
	"github.com/fatedier/frp/pkg/util/version"
	"github.com/fatedier/frp/pkg/util/xlog"
)

func init() {
	crypto.DefaultSalt = "frp"
	// TODO: remove this when we drop support for go1.19
	rand.Seed(time.Now().UnixNano())
}

// Service is a client service.
type Service struct {
	// uniq id got from frps, attach it in loginMsg
	runID string

	// manager control connection with server
	ctl   *Control
	ctlMu sync.RWMutex

	// Sets authentication based on selected method
	authSetter auth.Setter

	cfg         config.ClientCommonConf
	pxyCfgs     map[string]config.ProxyConf
	visitorCfgs map[string]config.VisitorConf
	cfgMu       sync.RWMutex

	// The configuration file used to initialize this client, or an empty
	// string if no configuration file was used.
	cfgFile string

	exit uint32 // 0 means not exit

	// service context
	ctx context.Context
	// call cancel to stop service
	cancel context.CancelFunc
}

func NewService(
	cfg config.ClientCommonConf,
	pxyCfgs map[string]config.ProxyConf,
	visitorCfgs map[string]config.VisitorConf,
	cfgFile string,
) (svr *Service, err error) {
	svr = &Service{
		authSetter:  auth.NewAuthSetter(cfg.ClientConfig),
		cfg:         cfg,
		cfgFile:     cfgFile,
		pxyCfgs:     pxyCfgs,
		visitorCfgs: visitorCfgs,
		ctx:         context.Background(),
		exit:        0,
	}
	return
}

func (svr *Service) GetController() *Control {
	svr.ctlMu.RLock()
	defer svr.ctlMu.RUnlock()
	return svr.ctl
}

func (svr *Service) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	svr.ctx = xlog.NewContext(ctx, xlog.New())
	svr.cancel = cancel

	xl := xlog.FromContextSafe(svr.ctx)

	// set custom DNSServer
	if svr.cfg.DNSServer != "" {
		dnsAddr := svr.cfg.DNSServer
		if _, _, err := net.SplitHostPort(dnsAddr); err != nil {
			dnsAddr = net.JoinHostPort(dnsAddr, "53")
		}
		// Change default dns server for frpc
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return net.Dial("udp", dnsAddr)
			},
		}
	}

	// login to frps
	for {
		conn, cm, err := svr.login()
		if err != nil {
			xl.Warn("无法连接至服务器: %v", err)
			if strings.Contains(err.Error(), "i/o timeout") {
				xl.Warn("请尝试将配置文件中tls_enable = false改为tls_enable = true再启动，如果依旧无法启动，则为上层防火墙拦截，请更换设备。")
			} else if strings.Contains(err.Error(), "invalid port") {
				xl.Warn("无效的节点端口，如果您没有随意更改配置文件，请前往交流群提交问题。您可以暂时更换节点解决")
			} else if strings.Contains(err.Error(), "token in login doesn't match token from configuration") {
				xl.Warn("节点TOKEN错误，如果您没有随意更改配置文件，请前往交流群提交问题。您可以暂时更换节点解决")
			} else if strings.Contains(err.Error(), "EOF") {
				xl.Warn("请尝试将配置文件中tls_enable = false改为tls_enable = true再启动，如果依旧无法启动，则为上层防火墙拦截，请更换设备。")
			} else if strings.Contains(err.Error(), "i/o deadline reached") {
				xl.Warn("请尝试将配置文件中tls_enable = false改为tls_enable = true再启动，如果依旧无法启动，则为上层防火墙拦截，请更换设备。或更换节点。")
			} else if strings.Contains(err.Error(), "dial tcp 127.0.0.1:7000: connectex: No connection could be made because the target machine actively refused it.") {
				xl.Warn("您尚未更改配置文件，请更改配置文件(frpc.ini)后再启动隧道。更改完后需要按Ctrl+S保存。")
			} else if strings.Contains(err.Error(), "connectex: No connection could be made because the target machine actively refused it.") {
				xl.Warn("此节点可能已离线，或您的网络连不上此节点，请更换节点后再启动。如若更换节点无用，请加入交流群询问。")
			}

			// 直接结束进程
			os.Exit(1)

			// if login_fail_exit is true, just exit this program
			// otherwise sleep a while and try again to connect to server
			if svr.cfg.LoginFailExit {
				return err
			}
			util.RandomSleep(5*time.Second, 0.9, 1.1)
		} else {
			// login success
			ctl := NewControl(svr.ctx, svr.runID, conn, cm, svr.cfg, svr.pxyCfgs, svr.visitorCfgs, svr.authSetter)
			ctl.Run()
			svr.ctlMu.Lock()
			svr.ctl = ctl
			svr.ctlMu.Unlock()
			break
		}
	}

	go svr.keepControllerWorking()

	if svr.cfg.AdminPort != 0 {
		// Init admin server assets
		assets.Load(svr.cfg.AssetsDir)

		address := net.JoinHostPort(svr.cfg.AdminAddr, strconv.Itoa(svr.cfg.AdminPort))
		err := svr.RunAdminServer(address)
		if err != nil {
			log.Warn("运行管理服务器错误: %v", err)
		}
		log.Info("管理服务器侦听 %s:%d", svr.cfg.AdminAddr, svr.cfg.AdminPort)
	}
	<-svr.ctx.Done()
	// service context may not be canceled by svr.Close(), we should call it here to release resources
	if atomic.LoadUint32(&svr.exit) == 0 {
		svr.Close()
	}
	return nil
}

func (svr *Service) keepControllerWorking() {
	xl := xlog.FromContextSafe(svr.ctx)

	reconnectDelays := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
	}
	reconnectCounts := 0

	for {
		<-svr.ctl.ClosedDoneCh()
		if atomic.LoadUint32(&svr.exit) != 0 {
			return
		}

		if reconnectCounts >= len(reconnectDelays) {
			xl.Error("重连已失败 %d 次，客户端即将退出", reconnectCounts)
			os.Exit(1)
		}

		wait := reconnectDelays[reconnectCounts]
		xl.Info("第 %d 次重连，等待 %v 后尝试连接", reconnectCounts+1, wait)
		time.Sleep(wait)
		reconnectCounts++

		for {
			if atomic.LoadUint32(&svr.exit) != 0 {
				return
			}

			xl.Info("尝试重新连接至服务器")
			conn, cm, err := svr.login()
			if err != nil {
				xl.Warn("重连失败: %v", err)
				break // 重连失败退出内层循环，进入下一次 retry
			}

			// 成功重连
			reconnectCounts = 0
			ctl := NewControl(svr.ctx, svr.runID, conn, cm, svr.cfg, svr.pxyCfgs, svr.visitorCfgs, svr.authSetter)
			ctl.Run()
			svr.ctlMu.Lock()
			if svr.ctl != nil {
				svr.ctl.Close()
			}
			svr.ctl = ctl
			svr.ctlMu.Unlock()
			break
		}
	}
}

// login creates a connection to frps and registers it self as a client
// conn: control connection
// session: if it's not nil, using tcp mux
func (svr *Service) login() (conn net.Conn, cm *ConnectionManager, err error) {
	xl := xlog.FromContextSafe(svr.ctx)
	cm = NewConnectionManager(svr.ctx, &svr.cfg)

	if err = cm.OpenConnection(); err != nil {
		return nil, nil, err
	}

	defer func() {
		if err != nil {
			cm.Close()
		}
	}()

	conn, err = cm.Connect()
	if err != nil {
		return
	}

	loginMsg := &msg.Login{
		Arch:      runtime.GOARCH,
		Os:        runtime.GOOS,
		PoolCount: svr.cfg.PoolCount,
		User:      svr.cfg.User,
		Version:   version.Full(),
		Timestamp: time.Now().Unix(),
		RunID:     svr.runID,
		Metas:     svr.cfg.Metas,
	}

	// Add auth
	if err = svr.authSetter.SetLogin(loginMsg); err != nil {
		return
	}

	if err = msg.WriteMsg(conn, loginMsg); err != nil {
		return
	}

	var loginRespMsg msg.LoginResp
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err = msg.ReadMsgInto(conn, &loginRespMsg); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	if loginRespMsg.Error != "" {
		err = fmt.Errorf("%s", loginRespMsg.Error)
		xl.Error("%s", loginRespMsg.Error)
		return
	}

	svr.runID = loginRespMsg.RunID
	xl.ResetPrefixes()

	xl.Info("成功登录至服务器, 获取到RunID [%v]", loginRespMsg.RunID)
	return
}

func (svr *Service) ReloadConf(pxyCfgs map[string]config.ProxyConf, visitorCfgs map[string]config.VisitorConf) error {
	svr.cfgMu.Lock()
	svr.pxyCfgs = pxyCfgs
	svr.visitorCfgs = visitorCfgs
	svr.cfgMu.Unlock()

	svr.ctlMu.RLock()
	ctl := svr.ctl
	svr.ctlMu.RUnlock()

	if ctl != nil {
		return svr.ctl.ReloadConf(pxyCfgs, visitorCfgs)
	}
	return nil
}

func (svr *Service) Close() {
	svr.GracefulClose(time.Duration(0))
}

func (svr *Service) GracefulClose(d time.Duration) {
	atomic.StoreUint32(&svr.exit, 1)

	svr.ctlMu.RLock()
	if svr.ctl != nil {
		svr.ctl.GracefulClose(d)
		svr.ctl = nil
	}
	svr.ctlMu.RUnlock()

	if svr.cancel != nil {
		svr.cancel()
	}
}

type ConnectionManager struct {
	ctx context.Context
	cfg *config.ClientCommonConf

	muxSession *fmux.Session
	quicConn   quic.Connection
}

func NewConnectionManager(ctx context.Context, cfg *config.ClientCommonConf) *ConnectionManager {
	return &ConnectionManager{
		ctx: ctx,
		cfg: cfg,
	}
}

func (cm *ConnectionManager) OpenConnection() error {
	xl := xlog.FromContextSafe(cm.ctx)

	// special for quic
	if strings.EqualFold(cm.cfg.Protocol, "quic") {
		var tlsConfig *tls.Config
		var err error
		sn := cm.cfg.TLSServerName
		if sn == "" {
			sn = cm.cfg.ServerAddr
		}
		if cm.cfg.TLSEnable {
			tlsConfig, err = transport.NewClientTLSConfig(
				cm.cfg.TLSCertFile,
				cm.cfg.TLSKeyFile,
				cm.cfg.TLSTrustedCaFile,
				sn)
		} else {
			tlsConfig, err = transport.NewClientTLSConfig("", "", "", sn)
		}
		if err != nil {
			xl.Warn("无法建立TLS链接: %v", err)
			return err
		}
		tlsConfig.NextProtos = []string{"frp"}

		conn, err := quic.DialAddr(
			cm.ctx,
			net.JoinHostPort(cm.cfg.ServerAddr, strconv.Itoa(cm.cfg.ServerPort)),
			tlsConfig, &quic.Config{
				MaxIdleTimeout:     time.Duration(cm.cfg.QUICMaxIdleTimeout) * time.Second,
				MaxIncomingStreams: int64(cm.cfg.QUICMaxIncomingStreams),
				KeepAlivePeriod:    time.Duration(cm.cfg.QUICKeepalivePeriod) * time.Second,
			})
		if err != nil {
			return err
		}
		cm.quicConn = conn
		return nil
	}

	if !cm.cfg.TCPMux {
		return nil
	}

	conn, err := cm.realConnect()
	if err != nil {
		return err
	}

	fmuxCfg := fmux.DefaultConfig()
	fmuxCfg.KeepAliveInterval = time.Duration(cm.cfg.TCPMuxKeepaliveInterval) * time.Second
	fmuxCfg.LogOutput = io.Discard
	fmuxCfg.MaxStreamWindowSize = 6 * 1024 * 1024
	session, err := fmux.Client(conn, fmuxCfg)
	if err != nil {
		return err
	}
	cm.muxSession = session
	return nil
}

func (cm *ConnectionManager) Connect() (net.Conn, error) {
	if cm.quicConn != nil {
		stream, err := cm.quicConn.OpenStreamSync(context.Background())
		if err != nil {
			return nil, err
		}
		return utilnet.QuicStreamToNetConn(stream, cm.quicConn), nil
	} else if cm.muxSession != nil {
		stream, err := cm.muxSession.OpenStream()
		if err != nil {
			return nil, err
		}
		return stream, nil
	}

	return cm.realConnect()
}

func (cm *ConnectionManager) realConnect() (net.Conn, error) {
	xl := xlog.FromContextSafe(cm.ctx)
	var tlsConfig *tls.Config
	var err error
	tlsEnable := cm.cfg.TLSEnable
	if cm.cfg.Protocol == "wss" {
		tlsEnable = true
	}
	if tlsEnable {
		sn := cm.cfg.TLSServerName
		if sn == "" {
			sn = cm.cfg.ServerAddr
		}

		tlsConfig, err = transport.NewClientTLSConfig(
			cm.cfg.TLSCertFile,
			cm.cfg.TLSKeyFile,
			cm.cfg.TLSTrustedCaFile,
			sn)
		if err != nil {
			xl.Warn("无法建立TLS链接: %v", err)
			return nil, err
		}
	}

	proxyType, addr, auth, err := libdial.ParseProxyURL(cm.cfg.HTTPProxy)
	if err != nil {
		xl.Error("fail to parse proxy url")
		return nil, err
	}
	dialOptions := []libdial.DialOption{}
	protocol := cm.cfg.Protocol
	switch protocol {
	case "websocket":
		protocol = "tcp"
		dialOptions = append(dialOptions, libdial.WithAfterHook(libdial.AfterHook{Hook: utilnet.DialHookWebsocket(protocol, "")}))
		dialOptions = append(dialOptions, libdial.WithAfterHook(libdial.AfterHook{
			Hook: utilnet.DialHookCustomTLSHeadByte(tlsConfig != nil, cm.cfg.DisableCustomTLSFirstByte),
		}))
		dialOptions = append(dialOptions, libdial.WithTLSConfig(tlsConfig))
	case "wss":
		protocol = "tcp"
		dialOptions = append(dialOptions, libdial.WithTLSConfigAndPriority(100, tlsConfig))
		// Make sure that if it is wss, the websocket hook is executed after the tls hook.
		dialOptions = append(dialOptions, libdial.WithAfterHook(libdial.AfterHook{Hook: utilnet.DialHookWebsocket(protocol, tlsConfig.ServerName), Priority: 110}))
	default:
		dialOptions = append(dialOptions, libdial.WithTLSConfig(tlsConfig))
	}

	if cm.cfg.ConnectServerLocalIP != "" {
		dialOptions = append(dialOptions, libdial.WithLocalAddr(cm.cfg.ConnectServerLocalIP))
	}
	dialOptions = append(dialOptions,
		libdial.WithProtocol(protocol),
		libdial.WithTimeout(time.Duration(cm.cfg.DialServerTimeout)*time.Second),
		libdial.WithKeepAlive(time.Duration(cm.cfg.DialServerKeepAlive)*time.Second),
		libdial.WithProxy(proxyType, addr),
		libdial.WithProxyAuth(auth),
	)
	conn, err := libdial.DialContext(
		cm.ctx,
		net.JoinHostPort(cm.cfg.ServerAddr, strconv.Itoa(cm.cfg.ServerPort)),
		dialOptions...,
	)
	return conn, err
}

func (cm *ConnectionManager) Close() error {
	if cm.quicConn != nil {
		_ = cm.quicConn.CloseWithError(0, "")
	}
	if cm.muxSession != nil {
		_ = cm.muxSession.Close()
	}
	return nil
}
