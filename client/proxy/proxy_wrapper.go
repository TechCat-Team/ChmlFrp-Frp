// Copyright 2023 The frp Authors
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

package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/golib/errors"

	"github.com/fatedier/frp/client/event"
	"github.com/fatedier/frp/client/health"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/transport"
	"github.com/fatedier/frp/pkg/util/xlog"
)

const (
	ProxyPhaseNew         = "new"
	ProxyPhaseWaitStart   = "wait start"
	ProxyPhaseStartErr    = "start error"
	ProxyPhaseRunning     = "running"
	ProxyPhaseCheckFailed = "check failed"
	ProxyPhaseClosed      = "closed"
)

var (
	statusCheckInterval = 3 * time.Second
	waitResponseTimeout = 20 * time.Second
	startErrTimeout     = 30 * time.Second
)

type WorkingStatus struct {
	Name  string           `json:"name"`
	Type  string           `json:"type"`
	Phase string           `json:"status"`
	Err   string           `json:"err"`
	Cfg   config.ProxyConf `json:"cfg"`

	// Got from server.
	RemoteAddr string `json:"remote_addr"`
}

type Wrapper struct {
	WorkingStatus

	// underlying proxy
	pxy Proxy

	// if ProxyConf has healcheck config
	// monitor will watch if it is alive
	monitor *health.Monitor

	// event handler
	handler event.Handler

	msgTransporter transport.MessageTransporter

	health           uint32
	lastSendStartMsg time.Time
	lastStartErr     time.Time
	closeCh          chan struct{}
	healthNotifyCh   chan struct{}
	mu               sync.RWMutex

	xl  *xlog.Logger
	ctx context.Context
}

func NewWrapper(
	ctx context.Context,
	cfg config.ProxyConf,
	clientCfg config.ClientCommonConf,
	eventHandler event.Handler,
	msgTransporter transport.MessageTransporter,
) *Wrapper {
	baseInfo := cfg.GetBaseConfig()
	xl := xlog.FromContextSafe(ctx).Spawn().AppendPrefix(baseInfo.ProxyName)
	pw := &Wrapper{
		WorkingStatus: WorkingStatus{
			Name:  baseInfo.ProxyName,
			Type:  baseInfo.ProxyType,
			Phase: ProxyPhaseNew,
			Cfg:   cfg,
		},
		closeCh:        make(chan struct{}),
		healthNotifyCh: make(chan struct{}),
		handler:        eventHandler,
		msgTransporter: msgTransporter,
		xl:             xl,
		ctx:            xlog.NewContext(ctx, xl),
	}

	if baseInfo.HealthCheckType != "" {
		pw.health = 1 // means failed
		pw.monitor = health.NewMonitor(pw.ctx, baseInfo.HealthCheckType, baseInfo.HealthCheckIntervalS,
			baseInfo.HealthCheckTimeoutS, baseInfo.HealthCheckMaxFailed, baseInfo.HealthCheckAddr,
			baseInfo.HealthCheckURL, pw.statusNormalCallback, pw.statusFailedCallback)
		xl.Trace("enable health check monitor")
	}

	pw.pxy = NewProxy(pw.ctx, pw.Cfg, clientCfg, pw.msgTransporter)
	return pw
}

func (pw *Wrapper) SetRunningStatus(remoteAddr string, respErr string) error {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.Phase != ProxyPhaseWaitStart {
		return fmt.Errorf("status not wait start, ignore start message")
	}

	pw.RemoteAddr = remoteAddr
	if respErr != "" {
		pw.Phase = ProxyPhaseClosed // 不再重试，直接关闭代理
		pw.Err = respErr
		return fmt.Errorf(pw.Err)
	}	

	if err := pw.pxy.Run(); err != nil {
		pw.close()
		pw.Phase = ProxyPhaseClosed // 也直接关闭
		pw.Err = err.Error()
		return err
	}

	pw.Phase = ProxyPhaseRunning
	pw.Err = ""
	return nil
}

func (pw *Wrapper) Start() {
	go pw.checkWorker()
	if pw.monitor != nil {
		go pw.monitor.Start()
	}
}

func (pw *Wrapper) Stop() {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	close(pw.closeCh)
	close(pw.healthNotifyCh)
	pw.pxy.Close()
	if pw.monitor != nil {
		pw.monitor.Stop()
	}
	pw.Phase = ProxyPhaseClosed
	pw.close()
}

func (pw *Wrapper) close() {
	_ = pw.handler(&event.CloseProxyPayload{
		CloseProxyMsg: &msg.CloseProxy{
			ProxyName: pw.Name,
		},
	})
}

func (pw *Wrapper) checkWorker() {
	xl := pw.xl
	if pw.monitor != nil {
		// let monitor do check request first
		time.Sleep(500 * time.Millisecond)
	}
	for {
		// check proxy status
		now := time.Now()
		if atomic.LoadUint32(&pw.health) == 0 {
			pw.mu.Lock()
			if pw.Phase == ProxyPhaseNew ||
				pw.Phase == ProxyPhaseCheckFailed ||
				(pw.Phase == ProxyPhaseWaitStart && now.After(pw.lastSendStartMsg.Add(waitResponseTimeout))) ||
				(pw.Phase == ProxyPhaseStartErr && now.After(pw.lastStartErr.Add(startErrTimeout))) {

				xl.Trace("change status from [%s] to [%s]", pw.Phase, ProxyPhaseWaitStart)
				pw.Phase = ProxyPhaseWaitStart

				var newProxyMsg msg.NewProxy
				pw.Cfg.MarshalToMsg(&newProxyMsg)
				pw.lastSendStartMsg = now
				_ = pw.handler(&event.StartProxyPayload{
					NewProxyMsg: &newProxyMsg,
				})
			}
			pw.mu.Unlock()
		} else {
			pw.mu.Lock()
			if pw.Phase == ProxyPhaseRunning || pw.Phase == ProxyPhaseWaitStart {
				pw.close()
				xl.Trace("change status from [%s] to [%s]", pw.Phase, ProxyPhaseCheckFailed)
				pw.Phase = ProxyPhaseCheckFailed
			}
			pw.mu.Unlock()
		}

		select {
		case <-pw.closeCh:
			return
		case <-time.After(statusCheckInterval):
		case <-pw.healthNotifyCh:
		}
	}
}

func (pw *Wrapper) statusNormalCallback() {
	xl := pw.xl
	atomic.StoreUint32(&pw.health, 0)
	_ = errors.PanicToError(func() {
		select {
		case pw.healthNotifyCh <- struct{}{}:
		default:
		}
	})
	xl.Info("health check success")
}

func (pw *Wrapper) statusFailedCallback() {
	xl := pw.xl
	atomic.StoreUint32(&pw.health, 1)
	_ = errors.PanicToError(func() {
		select {
		case pw.healthNotifyCh <- struct{}{}:
		default:
		}
	})
	xl.Info("health check failed")
}

func (pw *Wrapper) InWorkConn(workConn net.Conn, m *msg.StartWorkConn) {
	xl := pw.xl
	pw.mu.RLock()
	pxy := pw.pxy
	pw.mu.RUnlock()
	if pxy != nil && pw.Phase == ProxyPhaseRunning {
		xl.Debug("start a new work connection, localAddr: %s remoteAddr: %s", workConn.LocalAddr().String(), workConn.RemoteAddr().String())
		go pxy.InWorkConn(workConn, m)
	} else {
		workConn.Close()
	}
}

func (pw *Wrapper) GetStatus() *WorkingStatus {
	pw.mu.RLock()
	defer pw.mu.RUnlock()
	ps := &WorkingStatus{
		Name:       pw.Name,
		Type:       pw.Type,
		Phase:      pw.Phase,
		Err:        pw.Err,
		Cfg:        pw.Cfg,
		RemoteAddr: pw.RemoteAddr,
	}
	return ps
}
