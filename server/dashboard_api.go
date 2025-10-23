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
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/host"
	"github.com/shirou/gopsutil/load"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/net"

	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/consts"
	frpMem "github.com/fatedier/frp/pkg/metrics/mem"
	"github.com/fatedier/frp/pkg/util/log"
	"github.com/fatedier/frp/pkg/util/version"
)

type GeneralResponse struct {
	Code int
	Msg  string
}

type serverInfoResp struct {
	Version               string `json:"version"`
	BindPort              int    `json:"bind_port"`
	BindUDPPort           int    `json:"bind_udp_port"`
	VhostHTTPPort         int    `json:"vhost_http_port"`
	VhostHTTPSPort        int    `json:"vhost_https_port"`
	TCPMuxHTTPConnectPort int    `json:"tcpmux_httpconnect_port"`
	KCPBindPort           int    `json:"kcp_bind_port"`
	QUICBindPort          int    `json:"quic_bind_port"`
	SubdomainHost         string `json:"subdomain_host"`
	MaxPoolCount          int64  `json:"max_pool_count"`
	MaxPortsPerClient     int64  `json:"max_ports_per_client"`
	HeartBeatTimeout      int64  `json:"heart_beat_timeout"`
	AllowPortsStr         string `json:"allow_ports_str,omitempty"`
	TLSOnly               bool   `json:"tls_only,omitempty"`

	TotalTrafficIn  int64            `json:"total_traffic_in"`
	TotalTrafficOut int64            `json:"total_traffic_out"`
	CurConns        int64            `json:"cur_conns"`
	ClientCounts    int64            `json:"client_counts"`
	ProxyTypeCounts map[string]int64 `json:"proxy_type_counts"`

	CPUUsage          float64 `json:"cpu_usage"`
	CPUInfo           string  `json:"cpu_info"`
	NumCores          int     `json:"num_cores"`
	MemoryTotal       uint64  `json:"memory_total"`
	MemoryUsed        uint64  `json:"memory_used"`
	PageTables        uint64  `json:"page_tables"`
	StorageTotal      uint64  `json:"storage_total"`
	StorageUsed       uint64  `json:"storage_used"`
	UploadBandwidth   float64 `json:"upload_bandwidth"`
	DownloadBandwidth float64 `json:"download_bandwidth"`

	Load1         float64 `json:"load1"`
	Load5         float64 `json:"load5"`
	Load15        float64 `json:"load15"`
	UptimeSeconds uint64  `json:"uptime_seconds"`
	SentPackets   uint64  `json:"sent_packets"`
	RecvPackets   uint64  `json:"recv_packets"`
	ActiveConn    int     `json:"active_conn"`
	PassiveConn   int     `json:"passive_conn"`
}

var (
	prevNetIO                net.IOCountersStat
	prevTimestamp            time.Time
	currentBandwidthMutex    sync.Mutex
	currentUploadBandwidth   float64
	currentDownloadBandwidth float64
)

func init() {
	netIO, _ := net.IOCounters(false)
	prevNetIO = netIO[0]
	prevTimestamp = time.Now()

	go func() {
		for {
			time.Sleep(1 * time.Second)
			updateBandwidth()
		}
	}()
}

func updateBandwidth() {
	netIO, _ := net.IOCounters(false)
	currentNetIO := netIO[0]
	currentTimestamp := time.Now()

	duration := currentTimestamp.Sub(prevTimestamp).Seconds()
	uploadBandwidth := float64(currentNetIO.BytesSent-prevNetIO.BytesSent) * 8 / duration
	downloadBandwidth := float64(currentNetIO.BytesRecv-prevNetIO.BytesRecv) * 8 / duration

	currentBandwidthMutex.Lock()
	currentUploadBandwidth = uploadBandwidth
	currentDownloadBandwidth = downloadBandwidth
	currentBandwidthMutex.Unlock()

	prevNetIO = currentNetIO
	prevTimestamp = currentTimestamp
}

func getNetworkStats() (activeConn, passiveConn int) {
	conns, _ := net.Connections("all")
	for _, conn := range conns {
		switch conn.Status {
		case "ESTABLISHED":
			activeConn++
		case "SYN_SENT":
			passiveConn++
		}
	}
	return
}

// /healthz
func (svr *Service) Healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(200)
}

// /api/serverinfo
func (svr *Service) APIServerInfo(w http.ResponseWriter, r *http.Request) {
	res := GeneralResponse{Code: 200}
	defer func() {
		log.Info("Http response [%s]: code [%d]", r.URL.Path, res.Code)
		w.WriteHeader(res.Code)
		if len(res.Msg) > 0 {
			_, _ = w.Write([]byte(res.Msg))
		}
	}()

	log.Info("Http request: [%s]", r.URL.Path)
	serverStats := frpMem.StatsCollector.GetServer()

	// Get CPU usage
	cpuUsage, _ := cpu.Percent(0, false)

	// Get CPU info
	cpuInfo, _ := cpu.Info()

	// Get logical CPU count
	numCores, _ := cpu.Counts(true)

	// Get memory info
	vMem, _ := mem.VirtualMemory()

	// Get storage info
	diskInfo, _ := disk.Usage("/")

	// Get load averages
	loadAvg, _ := load.Avg()

	// Get uptime
	uptime, _ := host.Uptime()

	// Get current bandwidth
	currentBandwidthMutex.Lock()
	uploadBandwidth := currentUploadBandwidth
	downloadBandwidth := currentDownloadBandwidth
	sentPackets := prevNetIO.PacketsSent
	recvPackets := prevNetIO.PacketsRecv
	currentBandwidthMutex.Unlock()

	// Get network connections stats
	activeConn, passiveConn := getNetworkStats()

	svrResp := serverInfoResp{
		Version:               version.Full(),
		BindPort:              svr.cfg.BindPort,
		BindUDPPort:           svr.cfg.BindUDPPort,
		VhostHTTPPort:         svr.cfg.VhostHTTPPort,
		VhostHTTPSPort:        svr.cfg.VhostHTTPSPort,
		TCPMuxHTTPConnectPort: svr.cfg.TCPMuxHTTPConnectPort,
		KCPBindPort:           svr.cfg.KCPBindPort,
		QUICBindPort:          svr.cfg.QUICBindPort,
		SubdomainHost:         svr.cfg.SubDomainHost,
		MaxPoolCount:          svr.cfg.MaxPoolCount,
		MaxPortsPerClient:     svr.cfg.MaxPortsPerClient,
		HeartBeatTimeout:      svr.cfg.HeartbeatTimeout,
		AllowPortsStr:         svr.cfg.AllowPortsStr,
		TLSOnly:               svr.cfg.TLSOnly,

		TotalTrafficIn:  serverStats.TotalTrafficIn,
		TotalTrafficOut: serverStats.TotalTrafficOut,
		CurConns:        serverStats.CurConns,
		ClientCounts:    serverStats.ClientCounts,
		ProxyTypeCounts: serverStats.ProxyTypeCounts,

		CPUUsage:          cpuUsage[0],
		CPUInfo:           cpuInfo[0].ModelName,
		NumCores:          numCores,
		MemoryTotal:       vMem.Total,
		MemoryUsed:        vMem.Used,
		PageTables:        vMem.PageTables,
		StorageTotal:      diskInfo.Total,
		StorageUsed:       diskInfo.Used,
		UploadBandwidth:   uploadBandwidth,
		DownloadBandwidth: downloadBandwidth,

		Load1:         loadAvg.Load1,
		Load5:         loadAvg.Load5,
		Load15:        loadAvg.Load15,
		UptimeSeconds: uptime,
		SentPackets:   sentPackets,
		RecvPackets:   recvPackets,
		ActiveConn:    activeConn,
		PassiveConn:   passiveConn,
	}

	buf, _ := json.Marshal(&svrResp)
	res.Msg = string(buf)
}

type BaseOutConf struct {
	config.BaseProxyConf
}

type TCPOutConf struct {
	BaseOutConf
	RemotePort int `json:"remote_port"`
}

type TCPMuxOutConf struct {
	BaseOutConf
	config.DomainConf
	Multiplexer string `json:"multiplexer"`
}

type UDPOutConf struct {
	BaseOutConf
	RemotePort int `json:"remote_port"`
}

type HTTPOutConf struct {
	BaseOutConf
	config.DomainConf
	Locations         []string `json:"locations"`
	HostHeaderRewrite string   `json:"host_header_rewrite"`
}

type HTTPSOutConf struct {
	BaseOutConf
	config.DomainConf
}

type STCPOutConf struct {
	BaseOutConf
}

type XTCPOutConf struct {
	BaseOutConf
}

func getConfByType(proxyType string) interface{} {
	switch proxyType {
	case consts.TCPProxy:
		return &TCPOutConf{}
	case consts.TCPMuxProxy:
		return &TCPMuxOutConf{}
	case consts.UDPProxy:
		return &UDPOutConf{}
	case consts.HTTPProxy:
		return &HTTPOutConf{}
	case consts.HTTPSProxy:
		return &HTTPSOutConf{}
	case consts.STCPProxy:
		return &STCPOutConf{}
	case consts.XTCPProxy:
		return &XTCPOutConf{}
	default:
		return nil
	}
}

// Get proxy info.
type ProxyStatsInfo struct {
	Name            string      `json:"name"`
	Conf            interface{} `json:"conf"`
	ClientVersion   string      `json:"client_version,omitempty"`
	TodayTrafficIn  int64       `json:"today_traffic_in"`
	TodayTrafficOut int64       `json:"today_traffic_out"`
	CurConns        int64       `json:"cur_conns"`
	LastStartTime   string      `json:"last_start_time"`
	LastCloseTime   string      `json:"last_close_time"`
	Status          string      `json:"status"`
}

type GetProxyInfoResp struct {
	Proxies []*ProxyStatsInfo `json:"proxies"`
}

// /api/proxy/:type
func (svr *Service) APIProxyByType(w http.ResponseWriter, r *http.Request) {
	res := GeneralResponse{Code: 200}
	params := mux.Vars(r)
	proxyType := params["type"]

	defer func() {
		log.Info("Http response [%s]: code [%d]", r.URL.Path, res.Code)
		w.WriteHeader(res.Code)
		if len(res.Msg) > 0 {
			_, _ = w.Write([]byte(res.Msg))
		}
	}()
	log.Info("Http request: [%s]", r.URL.Path)

	proxyInfoResp := GetProxyInfoResp{}
	proxyInfoResp.Proxies = svr.getProxyStatsByType(proxyType)

	buf, _ := json.Marshal(&proxyInfoResp)
	res.Msg = string(buf)
}

func (svr *Service) getProxyStatsByType(proxyType string) (proxyInfos []*ProxyStatsInfo) {
	proxyStats := frpMem.StatsCollector.GetProxiesByType(proxyType)
	proxyInfos = make([]*ProxyStatsInfo, 0, len(proxyStats))
	for _, ps := range proxyStats {
		proxyInfo := &ProxyStatsInfo{}
		if pxy, ok := svr.pxyManager.GetByName(ps.Name); ok {
			content, err := json.Marshal(pxy.GetConf())
			if err != nil {
				log.Warn("marshal proxy [%s] conf info error: %v", ps.Name, err)
				continue
			}
			proxyInfo.Conf = getConfByType(ps.Type)
			if err = json.Unmarshal(content, &proxyInfo.Conf); err != nil {
				log.Warn("unmarshal proxy [%s] conf info error: %v", ps.Name, err)
				continue
			}
			proxyInfo.Status = consts.Online
			if pxy.GetLoginMsg() != nil {
				proxyInfo.ClientVersion = pxy.GetLoginMsg().Version
			}
		} else {
			proxyInfo.Status = consts.Offline
		}
		proxyInfo.Name = ps.Name
		proxyInfo.TodayTrafficIn = ps.TodayTrafficIn
		proxyInfo.TodayTrafficOut = ps.TodayTrafficOut
		proxyInfo.CurConns = ps.CurConns
		proxyInfo.LastStartTime = ps.LastStartTime
		proxyInfo.LastCloseTime = ps.LastCloseTime
		proxyInfos = append(proxyInfos, proxyInfo)
	}
	return
}

// Get proxy info by name.
type GetProxyStatsResp struct {
	Name            string      `json:"name"`
	Conf            interface{} `json:"conf"`
	TodayTrafficIn  int64       `json:"today_traffic_in"`
	TodayTrafficOut int64       `json:"today_traffic_out"`
	CurConns        int64       `json:"cur_conns"`
	LastStartTime   string      `json:"last_start_time"`
	LastCloseTime   string      `json:"last_close_time"`
	Status          string      `json:"status"`
}

// /api/proxy/:type/:name
func (svr *Service) APIProxyByTypeAndName(w http.ResponseWriter, r *http.Request) {
	res := GeneralResponse{Code: 200}
	params := mux.Vars(r)
	proxyType := params["type"]
	name := params["name"]

	defer func() {
		log.Info("Http response [%s]: code [%d]", r.URL.Path, res.Code)
		w.WriteHeader(res.Code)
		if len(res.Msg) > 0 {
			_, _ = w.Write([]byte(res.Msg))
		}
	}()
	log.Info("Http request: [%s]", r.URL.Path)

	var proxyStatsResp GetProxyStatsResp
	proxyStatsResp, res.Code, res.Msg = svr.getProxyStatsByTypeAndName(proxyType, name)
	if res.Code != 200 {
		return
	}

	buf, _ := json.Marshal(&proxyStatsResp)
	res.Msg = string(buf)
}

func (svr *Service) getProxyStatsByTypeAndName(proxyType string, proxyName string) (proxyInfo GetProxyStatsResp, code int, msg string) {
	proxyInfo.Name = proxyName
	ps := frpMem.StatsCollector.GetProxiesByTypeAndName(proxyType, proxyName)
	if ps == nil {
		code = 404
		msg = "no proxy info found"
	} else {
		if pxy, ok := svr.pxyManager.GetByName(proxyName); ok {
			content, err := json.Marshal(pxy.GetConf())
			if err != nil {
				log.Warn("marshal proxy [%s] conf info error: %v", ps.Name, err)
				code = 400
				msg = "parse conf error"
				return
			}
			proxyInfo.Conf = getConfByType(ps.Type)
			if err = json.Unmarshal(content, &proxyInfo.Conf); err != nil {
				log.Warn("unmarshal proxy [%s] conf info error: %v", ps.Name, err)
				code = 400
				msg = "parse conf error"
				return
			}
			proxyInfo.Status = consts.Online
		} else {
			proxyInfo.Status = consts.Offline
		}
		proxyInfo.TodayTrafficIn = ps.TodayTrafficIn
		proxyInfo.TodayTrafficOut = ps.TodayTrafficOut
		proxyInfo.CurConns = ps.CurConns
		proxyInfo.LastStartTime = ps.LastStartTime
		proxyInfo.LastCloseTime = ps.LastCloseTime
		code = 200
	}

	return
}

// /api/traffic/:name
type GetProxyTrafficResp struct {
	Name       string  `json:"name"`
	TrafficIn  []int64 `json:"traffic_in"`
	TrafficOut []int64 `json:"traffic_out"`
}

func (svr *Service) APIProxyTraffic(w http.ResponseWriter, r *http.Request) {
	res := GeneralResponse{Code: 200}
	params := mux.Vars(r)
	name := params["name"]

	defer func() {
		log.Info("Http response [%s]: code [%d]", r.URL.Path, res.Code)
		w.WriteHeader(res.Code)
		if len(res.Msg) > 0 {
			_, _ = w.Write([]byte(res.Msg))
		}
	}()
	log.Info("Http request: [%s]", r.URL.Path)

	trafficResp := GetProxyTrafficResp{}
	trafficResp.Name = name
	proxyTrafficInfo := frpMem.StatsCollector.GetProxyTraffic(name)

	if proxyTrafficInfo == nil {
		res.Code = 404
		res.Msg = "no proxy info found"
		return
	}
	trafficResp.TrafficIn = proxyTrafficInfo.TrafficIn
	trafficResp.TrafficOut = proxyTrafficInfo.TrafficOut

	buf, _ := json.Marshal(&trafficResp)
	res.Msg = string(buf)
}

type CloseUserResp struct {
	Status int    `json:"status"`
	Msg    string `json:"message"`
	runID  string `json:"runid"`
}

func (svr *Service) APICloseClient(w http.ResponseWriter, r *http.Request) {
	var (
		buf  []byte
		resp = CloseUserResp{}
	)
	params := mux.Vars(r)
	user := params["user"]
	defer func() {
		log.Info("Http response [/api/client/close/{user}]: code [%d]", resp.Status)
	}()
	log.Info("Http request: [/api/client/close/{user}] %#v", user)
	err := svr.CloseUser(user)
	if err != nil {
		resp.Status = 404
		resp.Msg = err.Error()
		resp.runID = "nan"
	} else {
		resp.Status = 200
		resp.Msg = "OK"
		resp.runID = user
	}
	buf, _ = json.Marshal(&resp)
	w.Write(buf)
}

func (svr *Service) APIListUserProxies(w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)
	user := params["user"]

	ctl, ok := svr.ctlManager.SearchByID(user)
	if !ok || ctl == nil {
		http.Error(w, "User not login", http.StatusNotFound)
		return
	}

	result := make([]string, 0)
	ctl.mu.RLock()
	for name := range ctl.proxies {
		result = append(result, name)
	}
	ctl.mu.RUnlock()

	// 返回当前用户的所有隧道名
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  200,
		"proxies": result,
	})
}

func (svr *Service) APICloseUserProxy(w http.ResponseWriter, r *http.Request) {
	var (
		buf  []byte
		resp = CloseUserResp{}
	)

	params := mux.Vars(r)
	user := params["user"]
	proxy := params["proxy"]

	log.Info("Http request: [/api/client/close/{user}/{proxy}] user: %s, proxy: %s", user, proxy)

	ctl, ok := svr.ctlManager.SearchByID(user)
	if !ok {
		resp.Status = 404
		resp.Msg = "user not login"
		resp.runID = "nan"
	} else {
		err := ctl.ForceCloseProxy(proxy)
		if err != nil {
			resp.Status = 404
			resp.Msg = err.Error()
			resp.runID = user
		} else {
			resp.Status = 200
			resp.Msg = "Proxy closed"
			resp.runID = user
		}
	}

	buf, _ = json.Marshal(&resp)
	w.Write(buf)
}
