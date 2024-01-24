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
	"io"
	"net"
	"reflect"
	"strconv"
	"time"

	"github.com/fatedier/golib/errors"
	libio "github.com/fatedier/golib/io"

	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/proto/udp"
	"github.com/fatedier/frp/pkg/util/limit"
	utilnet "github.com/fatedier/frp/pkg/util/net"
)

func init() {
	RegisterProxyFactory(reflect.TypeOf(&config.UDPProxyConf{}), NewUDPProxy)
}

type UDPProxy struct {
	*BaseProxy

	cfg *config.UDPProxyConf

	localAddr *net.UDPAddr
	readCh    chan *msg.UDPPacket

	// include msg.UDPPacket and msg.Ping
	sendCh   chan msg.Message
	workConn net.Conn
	closed   bool
}

func NewUDPProxy(baseProxy *BaseProxy, cfg config.ProxyConf) Proxy {
	unwrapped, ok := cfg.(*config.UDPProxyConf)
	if !ok {
		return nil
	}
	return &UDPProxy{
		BaseProxy: baseProxy,
		cfg:       unwrapped,
	}
}

func (pxy *UDPProxy) Run() (err error) {
	pxy.localAddr, err = net.ResolveUDPAddr("udp", net.JoinHostPort(pxy.cfg.LocalIP, strconv.Itoa(pxy.cfg.LocalPort)))
	if err != nil {
		return
	}
	return
}

func (pxy *UDPProxy) Close() {
	pxy.mu.Lock()
	defer pxy.mu.Unlock()

	if !pxy.closed {
		pxy.closed = true
		if pxy.workConn != nil {
			pxy.workConn.Close()
		}
		if pxy.readCh != nil {
			close(pxy.readCh)
		}
		if pxy.sendCh != nil {
			close(pxy.sendCh)
		}
	}
}

func (pxy *UDPProxy) InWorkConn(conn net.Conn, _ *msg.StartWorkConn) {
	xl := pxy.xl
	xl.Info("传入udp代理的新工作连接, %s", conn.RemoteAddr().String())
	// close resources releated with old workConn
	pxy.Close()

	var rwc io.ReadWriteCloser = conn
	var err error
	if pxy.limiter != nil {
		rwc = libio.WrapReadWriteCloser(limit.NewReader(conn, pxy.limiter), limit.NewWriter(conn, pxy.limiter), func() error {
			return conn.Close()
		})
	}
	if pxy.cfg.UseEncryption {
		rwc, err = libio.WithEncryption(rwc, []byte(pxy.clientCfg.Token))
		if err != nil {
			conn.Close()
			xl.Error("创建加密流错误: %v", err)
			return
		}
	}
	if pxy.cfg.UseCompression {
		rwc = libio.WithCompression(rwc)
	}
	conn = utilnet.WrapReadWriteCloserToConn(rwc, conn)

	pxy.mu.Lock()
	pxy.workConn = conn
	pxy.readCh = make(chan *msg.UDPPacket, 1024)
	pxy.sendCh = make(chan msg.Message, 1024)
	pxy.closed = false
	pxy.mu.Unlock()

	workConnReaderFn := func(conn net.Conn, readCh chan *msg.UDPPacket) {
		for {
			var udpMsg msg.UDPPacket
			if errRet := msg.ReadMsgInto(conn, &udpMsg); errRet != nil {
				xl.Warn("从workConn读取udp错误: %v", errRet)
				return
			}
			if errRet := errors.PanicToError(func() {
				xl.Trace("从workConn获取udp包: %s", udpMsg.Content)
				readCh <- &udpMsg
			}); errRet != nil {
				xl.Info("udp工作连接的读取器goroutine关闭: %v", errRet)
				return
			}
		}
	}
	workConnSenderFn := func(conn net.Conn, sendCh chan msg.Message) {
		defer func() {
			xl.Info("udp工作连接的编写器goroutine关闭")
		}()
		var errRet error
		for rawMsg := range sendCh {
			switch m := rawMsg.(type) {
			case *msg.UDPPacket:
				xl.Trace("将udp包发送到workConn: %s", m.Content)
			case *msg.Ping:
				xl.Trace("向udp workConn发送ping消息")
			}
			if errRet = msg.WriteMsg(conn, rawMsg); errRet != nil {
				xl.Error("udp工作写入错误: %v", errRet)
				return
			}
		}
	}
	heartbeatFn := func(sendCh chan msg.Message) {
		var errRet error
		for {
			time.Sleep(time.Duration(30) * time.Second)
			if errRet = errors.PanicToError(func() {
				sendCh <- &msg.Ping{}
			}); errRet != nil {
				xl.Trace("udp工作连接的心跳goroutine关闭")
				break
			}
		}
	}

	go workConnSenderFn(pxy.workConn, pxy.sendCh)
	go workConnReaderFn(pxy.workConn, pxy.readCh)
	go heartbeatFn(pxy.sendCh)
	udp.Forwarder(pxy.localAddr, pxy.readCh, pxy.sendCh, int(pxy.clientCfg.UDPPacketSize))
}
