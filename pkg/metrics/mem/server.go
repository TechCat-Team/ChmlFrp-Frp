// Copyright 2019 fatedier, fatedier@gmail.com
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

package mem

import (
	"sync"
	"time"

	"github.com/fatedier/frp/pkg/util/log"
	"github.com/fatedier/frp/pkg/util/metric"
	server "github.com/fatedier/frp/server/metrics"
	"github.com/shirou/gopsutil/net"
)

var (
	sm = newServerMetrics()

	ServerMetrics  server.ServerMetrics
	StatsCollector Collector

	// Daily network traffic tracking
	dailyNetTrafficMutex sync.Mutex
	dailyStartTime       time.Time
	dailyStartBytesRecv  int64
	dailyStartBytesSent  int64
)

func init() {
	ServerMetrics = sm
	StatsCollector = sm
	sm.run()
	initDailyNetTraffic()
}

func initDailyNetTraffic() {
	dailyNetTrafficMutex.Lock()
	defer dailyNetTrafficMutex.Unlock()

	now := time.Now()
	dailyStartTime = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	if netIO, err := net.IOCounters(false); err == nil && len(netIO) > 0 {
		dailyStartBytesRecv = int64(netIO[0].BytesRecv)
		dailyStartBytesSent = int64(netIO[0].BytesSent)
	}
}

func getDailyNetTraffic() (int64, int64) {
	dailyNetTrafficMutex.Lock()
	defer dailyNetTrafficMutex.Unlock()

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	// If it's a new day, reset the daily counters
	if today.After(dailyStartTime) {
		initDailyNetTraffic()
	}

	if netIO, err := net.IOCounters(false); err == nil && len(netIO) > 0 {
		currentBytesRecv := int64(netIO[0].BytesRecv)
		currentBytesSent := int64(netIO[0].BytesSent)

		// Calculate today's traffic by subtracting start-of-day values
		todayBytesRecv := currentBytesRecv - dailyStartBytesRecv
		todayBytesSent := currentBytesSent - dailyStartBytesSent

		// Ensure non-negative values (in case of system restart)
		if todayBytesRecv < 0 {
			todayBytesRecv = currentBytesRecv
			dailyStartBytesRecv = 0
		}
		if todayBytesSent < 0 {
			todayBytesSent = currentBytesSent
			dailyStartBytesSent = 0
		}

		return todayBytesRecv, todayBytesSent
	}

	return 0, 0
}

type serverMetrics struct {
	info *ServerStatistics
	mu   sync.Mutex
}

func newServerMetrics() *serverMetrics {
	return &serverMetrics{
		info: &ServerStatistics{
			TotalTrafficIn:  metric.NewDateCounter(ReserveDays),
			TotalTrafficOut: metric.NewDateCounter(ReserveDays),
			CurConns:        metric.NewCounter(),

			ClientCounts:    metric.NewCounter(),
			ProxyTypeCounts: make(map[string]metric.Counter),

			ProxyStatistics: make(map[string]*ProxyStatistics),
		},
	}
}

func (m *serverMetrics) run() {
	go func() {
		for {
			time.Sleep(12 * time.Hour)
			start := time.Now()
			count, total := m.clearUselessInfo()
			log.Debug("clear useless proxy statistics data count %d/%d, cost %v", count, total, time.Since(start))
		}
	}()
}

func (m *serverMetrics) clearUselessInfo() (int, int) {
	count := 0
	total := 0
	// To check if there are proxies that closed than 7 days and drop them.
	m.mu.Lock()
	defer m.mu.Unlock()
	total = len(m.info.ProxyStatistics)
	for name, data := range m.info.ProxyStatistics {
		if !data.LastCloseTime.IsZero() &&
			data.LastStartTime.Before(data.LastCloseTime) &&
			time.Since(data.LastCloseTime) > time.Duration(7*24)*time.Hour {
			delete(m.info.ProxyStatistics, name)
			count++
			log.Trace("clear proxy [%s]'s statistics data, lastCloseTime: [%s]", name, data.LastCloseTime.String())
		}
	}
	return count, total
}

func (m *serverMetrics) NewClient() {
	m.info.ClientCounts.Inc(1)
}

func (m *serverMetrics) CloseClient() {
	m.info.ClientCounts.Dec(1)
}

func (m *serverMetrics) NewProxy(name string, proxyType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	counter, ok := m.info.ProxyTypeCounts[proxyType]
	if !ok {
		counter = metric.NewCounter()
	}
	counter.Inc(1)
	m.info.ProxyTypeCounts[proxyType] = counter

	proxyStats, ok := m.info.ProxyStatistics[name]
	if !(ok && proxyStats.ProxyType == proxyType) {
		proxyStats = &ProxyStatistics{
			Name:       name,
			ProxyType:  proxyType,
			CurConns:   metric.NewCounter(),
			TrafficIn:  metric.NewDateCounter(ReserveDays),
			TrafficOut: metric.NewDateCounter(ReserveDays),
		}
		m.info.ProxyStatistics[name] = proxyStats
	}
	proxyStats.LastStartTime = time.Now()
}

func (m *serverMetrics) CloseProxy(name string, proxyType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if counter, ok := m.info.ProxyTypeCounts[proxyType]; ok {
		counter.Dec(1)
	}
	if proxyStats, ok := m.info.ProxyStatistics[name]; ok {
		proxyStats.LastCloseTime = time.Now()
	}
}

func (m *serverMetrics) OpenConnection(name string, _ string) {
	m.info.CurConns.Inc(1)

	m.mu.Lock()
	defer m.mu.Unlock()
	proxyStats, ok := m.info.ProxyStatistics[name]
	if ok {
		proxyStats.CurConns.Inc(1)
		m.info.ProxyStatistics[name] = proxyStats
	}
}

func (m *serverMetrics) CloseConnection(name string, _ string) {
	m.info.CurConns.Dec(1)

	m.mu.Lock()
	defer m.mu.Unlock()
	proxyStats, ok := m.info.ProxyStatistics[name]
	if ok {
		proxyStats.CurConns.Dec(1)
		m.info.ProxyStatistics[name] = proxyStats
	}
}

func (m *serverMetrics) AddTrafficIn(name string, _ string, trafficBytes int64) {
	m.info.TotalTrafficIn.Inc(trafficBytes)

	m.mu.Lock()
	defer m.mu.Unlock()

	proxyStats, ok := m.info.ProxyStatistics[name]
	if ok {
		proxyStats.TrafficIn.Inc(trafficBytes)
		m.info.ProxyStatistics[name] = proxyStats
	}
}

func (m *serverMetrics) AddTrafficOut(name string, _ string, trafficBytes int64) {
	m.info.TotalTrafficOut.Inc(trafficBytes)

	m.mu.Lock()
	defer m.mu.Unlock()

	proxyStats, ok := m.info.ProxyStatistics[name]
	if ok {
		proxyStats.TrafficOut.Inc(trafficBytes)
		m.info.ProxyStatistics[name] = proxyStats
	}
}

// Get stats data api.

func (m *serverMetrics) GetServer() *ServerStats {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Get daily system network statistics instead of proxy traffic aggregation
	totalTrafficIn, totalTrafficOut := getDailyNetTraffic()

	// If system stats are not available, fallback to proxy traffic
	if totalTrafficIn == 0 && totalTrafficOut == 0 {
		totalTrafficIn = m.info.TotalTrafficIn.TodayCount()
		totalTrafficOut = m.info.TotalTrafficOut.TodayCount()
	}

	s := &ServerStats{
		TotalTrafficIn:  totalTrafficIn,
		TotalTrafficOut: totalTrafficOut,
		CurConns:        int64(m.info.CurConns.Count()),
		ClientCounts:    int64(m.info.ClientCounts.Count()),
		ProxyTypeCounts: make(map[string]int64),
	}
	for k, v := range m.info.ProxyTypeCounts {
		s.ProxyTypeCounts[k] = int64(v.Count())
	}
	return s
}

func (m *serverMetrics) GetProxiesByType(proxyType string) []*ProxyStats {
	res := make([]*ProxyStats, 0)
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, proxyStats := range m.info.ProxyStatistics {
		if proxyStats.ProxyType != proxyType {
			continue
		}

		ps := &ProxyStats{
			Name:            name,
			Type:            proxyStats.ProxyType,
			TodayTrafficIn:  proxyStats.TrafficIn.TodayCount(),
			TodayTrafficOut: proxyStats.TrafficOut.TodayCount(),
			CurConns:        int64(proxyStats.CurConns.Count()),
		}
		if !proxyStats.LastStartTime.IsZero() {
			ps.LastStartTime = proxyStats.LastStartTime.Format("01-02 15:04:05")
		}
		if !proxyStats.LastCloseTime.IsZero() {
			ps.LastCloseTime = proxyStats.LastCloseTime.Format("01-02 15:04:05")
		}
		res = append(res, ps)
	}
	return res
}

func (m *serverMetrics) GetProxiesByTypeAndName(proxyType string, proxyName string) (res *ProxyStats) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, proxyStats := range m.info.ProxyStatistics {
		if proxyStats.ProxyType != proxyType {
			continue
		}

		if name != proxyName {
			continue
		}

		res = &ProxyStats{
			Name:            name,
			Type:            proxyStats.ProxyType,
			TodayTrafficIn:  proxyStats.TrafficIn.TodayCount(),
			TodayTrafficOut: proxyStats.TrafficOut.TodayCount(),
			CurConns:        int64(proxyStats.CurConns.Count()),
		}
		if !proxyStats.LastStartTime.IsZero() {
			res.LastStartTime = proxyStats.LastStartTime.Format("01-02 15:04:05")
		}
		if !proxyStats.LastCloseTime.IsZero() {
			res.LastCloseTime = proxyStats.LastCloseTime.Format("01-02 15:04:05")
		}
		break
	}
	return
}

func (m *serverMetrics) GetProxyTraffic(name string) (res *ProxyTrafficInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()

	proxyStats, ok := m.info.ProxyStatistics[name]
	if ok {
		res = &ProxyTrafficInfo{
			Name: name,
		}
		res.TrafficIn = proxyStats.TrafficIn.GetLastDaysCount(ReserveDays)
		res.TrafficOut = proxyStats.TrafficOut.GetLastDaysCount(ReserveDays)
	}
	return
}
