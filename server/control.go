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

package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/fatedier/golib/control/shutdown"
	"github.com/fatedier/golib/crypto"
	"github.com/fatedier/golib/errors"

	"github.com/fatedier/frp/pkg/api"
	"github.com/fatedier/frp/pkg/auth"
	"github.com/fatedier/frp/pkg/config"
	pkgerr "github.com/fatedier/frp/pkg/errors"
	"github.com/fatedier/frp/pkg/limit"
	"github.com/fatedier/frp/pkg/msg"
	plugin "github.com/fatedier/frp/pkg/plugin/server"
	"github.com/fatedier/frp/pkg/transport"
	"github.com/fatedier/frp/pkg/util/util"
	"github.com/fatedier/frp/pkg/util/version"
	"github.com/fatedier/frp/pkg/util/xlog"
	"github.com/fatedier/frp/server/controller"
	"github.com/fatedier/frp/server/metrics"
	"github.com/fatedier/frp/server/proxy"
)

type ControlManager struct {
	// controls indexed by run id
	ctlsByRunID map[string]*Control

	mu sync.RWMutex
}

func NewControlManager() *ControlManager {
	return &ControlManager{
		ctlsByRunID: make(map[string]*Control),
	}
}

func (cm *ControlManager) Add(runID string, ctl *Control) (old *Control) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	var ok bool
	old, ok = cm.ctlsByRunID[runID]
	if ok {
		old.Replaced(ctl)
	}
	cm.ctlsByRunID[runID] = ctl
	return
}

// we should make sure if it's the same control to prevent delete a new one
func (cm *ControlManager) Del(runID string, ctl *Control) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if c, ok := cm.ctlsByRunID[runID]; ok && c == ctl {
		delete(cm.ctlsByRunID, runID)
	}
}

func (cm *ControlManager) GetByID(runID string) (ctl *Control, ok bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	ctl, ok = cm.ctlsByRunID[runID]
	return
}

func (cm *ControlManager) SearchByID(runID string) (ctl *Control, ok bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	for k, v := range cm.ctlsByRunID {
		if strings.Contains(k, runID+"-") == true {
			if v == nil {
				return
			}
			ctl, ok = cm.ctlsByRunID[k]
		}
	}
	return
}

func (cm *ControlManager) Close() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for _, ctl := range cm.ctlsByRunID {
		ctl.Close()
	}
	cm.ctlsByRunID = make(map[string]*Control)
	return nil
}

type Control struct {
	// all resource managers and controllers
	rc *controller.ResourceController

	// proxy manager
	pxyManager *proxy.Manager

	// plugin manager
	pluginManager *plugin.Manager

	// verifies authentication based on selected method
	authVerifier auth.Verifier

	// other components can use this to communicate with client
	msgTransporter transport.MessageTransporter

	// login message
	loginMsg *msg.Login

	// control connection
	conn net.Conn

	// put a message in this channel to send it over control connection to client
	sendCh chan (msg.Message)

	// read from this channel to get the next message sent by client
	readCh chan (msg.Message)

	// work connections
	workConnCh chan net.Conn

	// proxies in one client
	proxies map[string]proxy.Proxy

	// pool count
	poolCount int

	// ports used, for limitations
	portsUsedNum int

	// last time got the Ping message
	lastPing time.Time

	// A new run id will be generated when a new client login.
	// If run id got from login message has same run id, it means it's the same client, so we can
	// replace old controller instantly.
	runID string

	readerShutdown  *shutdown.Shutdown
	writerShutdown  *shutdown.Shutdown
	managerShutdown *shutdown.Shutdown
	allShutdown     *shutdown.Shutdown

	started bool

	mu sync.RWMutex

	// Server configuration information
	serverCfg config.ServerCommonConf

	xl       *xlog.Logger
	ctx      context.Context
	inLimit  uint64
	outLimit uint64
}

func NewControl(
	ctx context.Context,
	rc *controller.ResourceController,
	pxyManager *proxy.Manager,
	pluginManager *plugin.Manager,
	authVerifier auth.Verifier,
	ctlConn net.Conn,
	loginMsg *msg.Login,
	serverCfg config.ServerCommonConf,
	inLimit uint64,
	outLimit uint64,
) *Control {
	poolCount := loginMsg.PoolCount
	if poolCount > int(serverCfg.MaxPoolCount) {
		poolCount = int(serverCfg.MaxPoolCount)
	}
	ctl := &Control{
		rc:              rc,
		pxyManager:      pxyManager,
		pluginManager:   pluginManager,
		authVerifier:    authVerifier,
		conn:            ctlConn,
		loginMsg:        loginMsg,
		sendCh:          make(chan msg.Message, 10),
		readCh:          make(chan msg.Message, 10),
		workConnCh:      make(chan net.Conn, poolCount+10),
		proxies:         make(map[string]proxy.Proxy),
		poolCount:       poolCount,
		portsUsedNum:    0,
		lastPing:        time.Now(),
		runID:           loginMsg.RunID,
		readerShutdown:  shutdown.New(),
		writerShutdown:  shutdown.New(),
		managerShutdown: shutdown.New(),
		allShutdown:     shutdown.New(),
		serverCfg:       serverCfg,
		xl:              xlog.FromContextSafe(ctx),
		ctx:             ctx,
		inLimit:         inLimit,  //rate.NewLimiter(rate.Limit(inLimit*limit.KB), int(inLimit*limit.KB)),
		outLimit:        outLimit, //rate.NewLimiter(rate.Limit(outLimit*limit.KB), int(outLimit*limit.KB)),

	}
	ctl.msgTransporter = transport.NewMessageTransporter(ctl.sendCh)
	return ctl
}

// Start send a login success message to client and start working.
func (ctl *Control) Start() {
	loginRespMsg := &msg.LoginResp{
		Version: version.Full(),
		RunID:   ctl.runID,
		Error:   "",
	}
	_ = msg.WriteMsg(ctl.conn, loginRespMsg)
	ctl.mu.Lock()
	ctl.started = true
	ctl.mu.Unlock()

	go ctl.writer()
	go func() {
		for i := 0; i < ctl.poolCount; i++ {
			// ignore error here, that means that this control is closed
			_ = errors.PanicToError(func() {
				ctl.sendCh <- &msg.ReqWorkConn{}
			})
		}
	}()

	go ctl.manager()
	go ctl.reader()
	go ctl.stoper()
}

func (ctl *Control) Close() error {
	ctl.allShutdown.Start()
	return nil
}

func (ctl *Control) Replaced(newCtl *Control) {
	xl := ctl.xl
	xl.Info("Replaced by client [%s]", newCtl.runID)
	ctl.runID = ""
	ctl.allShutdown.Start()
}

func (ctl *Control) RegisterWorkConn(conn net.Conn) error {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	select {
	case ctl.workConnCh <- conn:
		xl.Debug("new work connection registered")
		return nil
	default:
		xl.Debug("work connection pool is full, discarding")
		return fmt.Errorf("work connection pool is full, discarding")
	}
}

// When frps get one user connection, we get one work connection from the pool and return it.
// If no workConn available in the pool, send message to frpc to get one or more
// and wait until it is available.
// return an error if wait timeout
func (ctl *Control) GetWorkConn() (workConn net.Conn, err error) {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	var ok bool
	// get a work connection from the pool
	select {
	case workConn, ok = <-ctl.workConnCh:
		if !ok {
			err = pkgerr.ErrCtlClosed
			return
		}
		xl.Debug("get work connection from pool")
	default:
		// no work connections available in the poll, send message to frpc to get more
		if err = errors.PanicToError(func() {
			ctl.sendCh <- &msg.ReqWorkConn{}
		}); err != nil {
			return nil, fmt.Errorf("control is already closed")
		}

		select {
		case workConn, ok = <-ctl.workConnCh:
			if !ok {
				err = pkgerr.ErrCtlClosed
				xl.Warn("no work connections available, %v", err)
				return
			}

		case <-time.After(time.Duration(ctl.serverCfg.UserConnTimeout) * time.Second):
			err = fmt.Errorf("timeout trying to get work connection")
			xl.Warn("%v", err)
			return
		}
	}

	// When we get a work connection from pool, replace it with a new one.
	_ = errors.PanicToError(func() {
		ctl.sendCh <- &msg.ReqWorkConn{}
	})
	return
}

func (ctl *Control) writer() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.writerShutdown.Done()

	encWriter, err := crypto.NewWriter(ctl.conn, []byte(ctl.serverCfg.Token))
	if err != nil {
		xl.Error("crypto new writer error: %v", err)
		ctl.allShutdown.Start()
		return
	}
	for {
		m, ok := <-ctl.sendCh
		if !ok {
			xl.Info("control writer is closing")
			return
		}

		if err := msg.WriteMsg(encWriter, m); err != nil {
			xl.Warn("write message to control connection error: %v", err)
			return
		}
	}
}

func (ctl *Control) reader() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.readerShutdown.Done()

	encReader := crypto.NewReader(ctl.conn, []byte(ctl.serverCfg.Token))
	for {
		m, err := msg.ReadMsg(encReader)
		if err != nil {
			if err == io.EOF {
				xl.Debug("control connection closed")
				return
			}
			xl.Warn("read error: %v", err)
			ctl.conn.Close()
			return
		}

		ctl.readCh <- m
	}
}

func (ctl *Control) stoper() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	ctl.allShutdown.WaitStart()

	ctl.conn.Close()
	ctl.readerShutdown.WaitDone()

	close(ctl.readCh)
	ctl.managerShutdown.WaitDone()

	close(ctl.sendCh)
	ctl.writerShutdown.WaitDone()

	ctl.mu.Lock()
	defer ctl.mu.Unlock()

	close(ctl.workConnCh)
	for workConn := range ctl.workConnCh {
		workConn.Close()
	}

	for _, pxy := range ctl.proxies {
		pxy.Close()
		ctl.pxyManager.Del(pxy.GetName())
		metrics.Server.CloseProxy(pxy.GetName(), pxy.GetConf().GetBaseConfig().ProxyType)

		notifyContent := &plugin.CloseProxyContent{
			User: plugin.UserInfo{
				User:  ctl.loginMsg.User,
				Metas: ctl.loginMsg.Metas,
				RunID: ctl.loginMsg.RunID,
			},
			CloseProxy: msg.CloseProxy{
				ProxyName: pxy.GetName(),
			},
		}
		go func() {
			_ = ctl.pluginManager.CloseProxy(notifyContent)
		}()
	}

	ctl.allShutdown.Done()
	xl.Info("client exit success")
	metrics.Server.CloseClient()
}

// block until Control closed
func (ctl *Control) WaitClosed() {
	ctl.mu.RLock()
	started := ctl.started
	ctl.mu.RUnlock()

	if !started {
		ctl.allShutdown.Done()
		return
	}
	ctl.allShutdown.WaitDone()
}

func (ctl *Control) manager() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.managerShutdown.Done()

	var heartbeatCh <-chan time.Time
	// Don't need application heartbeat if TCPMux is enabled,
	// yamux will do same thing.
	if !ctl.serverCfg.TCPMux && ctl.serverCfg.HeartbeatTimeout > 0 {
		heartbeat := time.NewTicker(time.Second)
		defer heartbeat.Stop()
		heartbeatCh = heartbeat.C
	}

	for {
		select {
		case <-heartbeatCh:
			if time.Since(ctl.lastPing) > time.Duration(ctl.serverCfg.HeartbeatTimeout)*time.Second {
				xl.Warn("heartbeat timeout")
				return
			}
		case rawMsg, ok := <-ctl.readCh:
			if !ok {
				return
			}

			switch m := rawMsg.(type) {
			case *msg.NewProxy:
				content := &plugin.NewProxyContent{
					User: plugin.UserInfo{
						User:  ctl.loginMsg.User,
						Metas: ctl.loginMsg.Metas,
						RunID: ctl.loginMsg.RunID,
					},
					NewProxy: *m,
				}
				var remoteAddr string
				retContent, err := ctl.pluginManager.NewProxy(content)
				if err == nil {
					m = &retContent.NewProxy
					remoteAddr, err = ctl.RegisterProxy(m)
				}

				// register proxy in this control
				resp := &msg.NewProxyResp{
					ProxyName: m.ProxyName,
				}
				if err != nil {
					xl.Warn("new proxy [%s] type [%s] error: %v", m.ProxyName, m.ProxyType, err)
					resp.Error = util.GenerateResponseErrorString(fmt.Sprintf("new proxy [%s] error", m.ProxyName), err, ctl.serverCfg.DetailedErrorsToClient)
				} else {
					resp.RemoteAddr = remoteAddr
					xl.Info("new proxy [%s] type [%s] success", m.ProxyName, m.ProxyType)
					metrics.Server.NewProxy(m.ProxyName, m.ProxyType)
				}
				ctl.sendCh <- resp
			case *msg.NatHoleVisitor:
				go ctl.HandleNatHoleVisitor(m)
			case *msg.NatHoleClient:
				go ctl.HandleNatHoleClient(m)
			case *msg.NatHoleReport:
				go ctl.HandleNatHoleReport(m)
			case *msg.CloseProxy:
				_ = ctl.CloseProxy(m)
				xl.Info("close proxy [%s] success", m.ProxyName)
			case *msg.Ping:
				content := &plugin.PingContent{
					User: plugin.UserInfo{
						User:  ctl.loginMsg.User,
						Metas: ctl.loginMsg.Metas,
						RunID: ctl.loginMsg.RunID,
					},
					Ping: *m,
				}
				retContent, err := ctl.pluginManager.Ping(content)
				if err == nil {
					m = &retContent.Ping
					err = ctl.authVerifier.VerifyPing(m)
				}
				if err != nil {
					xl.Warn("received invalid ping: %v", err)
					ctl.sendCh <- &msg.Pong{
						Error: util.GenerateResponseErrorString("invalid ping", err, ctl.serverCfg.DetailedErrorsToClient),
					}
					return
				}
				ctl.lastPing = time.Now()
				xl.Debug("receive heartbeat")
				ctl.sendCh <- &msg.Pong{}
			}
		}
	}
}

func (ctl *Control) HandleNatHoleVisitor(m *msg.NatHoleVisitor) {
	ctl.rc.NatHoleController.HandleVisitor(m, ctl.msgTransporter, ctl.loginMsg.User)
}

func (ctl *Control) HandleNatHoleClient(m *msg.NatHoleClient) {
	ctl.rc.NatHoleController.HandleClient(m, ctl.msgTransporter)
}

func (ctl *Control) HandleNatHoleReport(m *msg.NatHoleReport) {
	ctl.rc.NatHoleController.HandleReport(m)
}

func (ctl *Control) RegisterProxy(pxyMsg *msg.NewProxy) (remoteAddr string, err error) {
	var pxyConf config.ProxyConf
	s, err := api.NewService(ctl.serverCfg.ApiBaseUrl)
	var workConn proxy.GetWorkConnFn = ctl.GetWorkConn

	if err != nil {
		return remoteAddr, err
	}

	if ctl.serverCfg.EnableApi {

		nowTime := time.Now().Unix()
		ok, err := s.CheckProxy(ctl.loginMsg.User, pxyMsg, nowTime, ctl.serverCfg.ApiToken)

		if err != nil {
			return remoteAddr, err
		}

		if !ok {
			return remoteAddr, fmt.Errorf("invalid proxy configuration")
		}

		workConn = func() (net.Conn, error) {
			fconn, err := ctl.GetWorkConn()
			if err != nil {
				return nil, err
			}
			ctl.xl.Debug("client speed limit: %dKB/s (Inbound) / %dKB/s (Outbound)", ctl.inLimit, ctl.outLimit)
			return limit.NewLimitConn(ctl.inLimit, ctl.outLimit, fconn), nil
		}

	}
	// Load configures from NewProxy message and validate.
	pxyConf, err = config.NewProxyConfFromMsg(pxyMsg, ctl.serverCfg)
	if err != nil {
		return
	}

	// User info
	userInfo := plugin.UserInfo{
		User:  ctl.loginMsg.User,
		Metas: ctl.loginMsg.Metas,
		RunID: ctl.runID,
	}

	// NewProxy will return an interface Proxy.
	// In fact, it creates different proxies based on the proxy type. We just call run() here.
	pxy, err := proxy.NewProxy(ctl.ctx, userInfo, ctl.rc, ctl.poolCount, workConn, pxyConf, ctl.serverCfg, ctl.loginMsg)
	if err != nil {
		return remoteAddr, err
	}

	// Check ports used number in each client
	if ctl.serverCfg.MaxPortsPerClient > 0 {
		ctl.mu.Lock()
		if ctl.portsUsedNum+pxy.GetUsedPortsNum() > int(ctl.serverCfg.MaxPortsPerClient) {
			ctl.mu.Unlock()
			err = fmt.Errorf("exceed the max_ports_per_client")
			return
		}
		ctl.portsUsedNum += pxy.GetUsedPortsNum()
		ctl.mu.Unlock()

		defer func() {
			if err != nil {
				ctl.mu.Lock()
				ctl.portsUsedNum -= pxy.GetUsedPortsNum()
				ctl.mu.Unlock()
			}
		}()
	}

	if ctl.pxyManager.Exist(pxyMsg.ProxyName) {
		err = fmt.Errorf("proxy [%s] already exists", pxyMsg.ProxyName)
		return
	}

	remoteAddr, err = pxy.Run()
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			pxy.Close()
		}
	}()

	err = ctl.pxyManager.Add(pxyMsg.ProxyName, pxy)
	if err != nil {
		return
	}

	ctl.mu.Lock()
	ctl.proxies[pxy.GetName()] = pxy
	ctl.mu.Unlock()
	return
}

// service.go 或 control.go
func (ctl *Control) ForceCloseProxy(proxyName string) error {
	ctl.mu.Lock()
	defer ctl.mu.Unlock()

	// 1. 查找匹配的 proxy（完全匹配）
	pxy, ok := ctl.proxies[proxyName]
	if !ok {
		return fmt.Errorf("proxy %s not found", proxyName)
	}

	// 2. 更新已使用端口数
	if ctl.serverCfg.MaxPortsPerClient > 0 {
		ctl.portsUsedNum -= pxy.GetUsedPortsNum()
	}

	// 3. 关闭 proxy
	pxy.Close()

	// 4. 删除 proxy 引用
	ctl.pxyManager.Del(proxyName)
	delete(ctl.proxies, proxyName)

	// 5. 上报监控
	metrics.Server.CloseProxy(proxyName, pxy.GetConf().GetBaseConfig().ProxyType)

	// 6. 插件通知（如果启用插件系统）
	go func() {
		_ = ctl.pluginManager.CloseProxy(&plugin.CloseProxyContent{
			User: plugin.UserInfo{
				User:  ctl.loginMsg.User,
				Metas: ctl.loginMsg.Metas,
				RunID: ctl.loginMsg.RunID,
			},
			CloseProxy: msg.CloseProxy{
				ProxyName: proxyName,
			},
		})
	}()

	return nil
}

func (ctl *Control) CloseProxy(closeMsg *msg.CloseProxy) (err error) {
	ctl.mu.Lock()
	pxy, ok := ctl.proxies[closeMsg.ProxyName]
	if !ok {
		ctl.mu.Unlock()
		return
	}

	if ctl.serverCfg.MaxPortsPerClient > 0 {
		ctl.portsUsedNum -= pxy.GetUsedPortsNum()
	}
	pxy.Close()
	ctl.pxyManager.Del(pxy.GetName())
	delete(ctl.proxies, closeMsg.ProxyName)
	ctl.mu.Unlock()

	metrics.Server.CloseProxy(pxy.GetName(), pxy.GetConf().GetBaseConfig().ProxyType)

	notifyContent := &plugin.CloseProxyContent{
		User: plugin.UserInfo{
			User:  ctl.loginMsg.User,
			Metas: ctl.loginMsg.Metas,
			RunID: ctl.loginMsg.RunID,
		},
		CloseProxy: msg.CloseProxy{
			ProxyName: pxy.GetName(),
		},
	}
	go func() {
		_ = ctl.pluginManager.CloseProxy(notifyContent)
	}()

	return
}
