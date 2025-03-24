// Copyright 2018 fatedier, fatedier@gmail.com
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

package sub

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/api"
	"github.com/fatedier/frp/pkg/auth"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/util/log"
	"github.com/fatedier/frp/pkg/util/version"
)

const (
	CfgFileTypeIni = iota
	CfgFileTypeCmd
)

var (
	cfgFile     string
	cfgDir      string
	cfgProxyid  string
	cfgToken    string
	showVersion bool

	serverAddr      string
	user            string
	protocol        string
	token           string
	logLevel        string
	logFile         string
	logMaxDays      int
	disableLogColor bool
	dnsServer       string

	proxyName          string
	localIP            string
	localPort          int
	remotePort         int
	useEncryption      bool
	useCompression     bool
	bandwidthLimit     string
	bandwidthLimitMode string
	customDomains      string
	subDomain          string
	httpUser           string
	httpPwd            string
	locations          string
	hostHeaderRewrite  string
	role               string
	sk                 string
	multiplexer        string
	serverName         string
	bindAddr           string
	bindPort           int

	tlsEnable     bool
	tlsServerName string
)

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "./frpc.ini", "config file of frpc")
	rootCmd.PersistentFlags().StringVarP(&cfgDir, "config_dir", "", "", "config directory, run one frpc service for each file in config directory")
	rootCmd.PersistentFlags().BoolVarP(&showVersion, "version", "v", false, "version of frpc")
	rootCmd.PersistentFlags().StringVarP(&cfgToken, "token", "u", "", "The Token of ChmlFrp")
	rootCmd.PersistentFlags().StringVarP(&cfgProxyid, "id", "p", "", "The ProxyID of ChmlFrp")
}

func RegisterCommonFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVarP(&serverAddr, "server_addr", "s", "127.0.0.1:7000", "frp server's address")
	cmd.PersistentFlags().StringVarP(&user, "user", "u", "", "user")
	cmd.PersistentFlags().StringVarP(&protocol, "protocol", "p", "tcp", "tcp, kcp, quic, websocket, wss")
	cmd.PersistentFlags().StringVarP(&token, "token", "t", "", "auth token")
	cmd.PersistentFlags().StringVarP(&logLevel, "log_level", "", "info", "log level")
	cmd.PersistentFlags().StringVarP(&logFile, "log_file", "", "console", "console or file path")
	cmd.PersistentFlags().IntVarP(&logMaxDays, "log_max_days", "", 3, "log file reversed days")
	cmd.PersistentFlags().BoolVarP(&disableLogColor, "disable_log_color", "", false, "disable log color in console")
	cmd.PersistentFlags().BoolVarP(&tlsEnable, "tls_enable", "", true, "enable frpc tls")
	cmd.PersistentFlags().StringVarP(&tlsServerName, "tls_server_name", "", "", "specify the custom server name of tls certificate")
	cmd.PersistentFlags().StringVarP(&dnsServer, "dns_server", "", "", "specify dns server instead of using system default one")
}

type APIResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Version string `json:"version"`
}

var rootCmd = &cobra.Command{
	Use:   "frpc",
	Short: "Edited from fatedier/frp, Powered by ChmlFrp",
	RunE: func(cmd *cobra.Command, args []string) error {
		if showVersion {
			fmt.Println(version.Full())
			return nil
		}

		log.Info("欢迎使用ChmlFrp映射客户端!")
		var wg sync.WaitGroup

		// If cfgDir is not empty, run multiple frpc service for each config file in cfgDir.
		// Note that it's only designed for testing. It's not guaranteed to be stable.
		if cfgDir != "" {
			_ = runMultipleClients(cfgDir)
			return nil
		}

		log.Info("从ChmlFrp API获取配置文件...")
		s, err := api.NewService("https://cf-v1.uapis.cn/api/cfg.php")

		if err != nil {
			log.Warn("初始化API服务失败，错误: %s", err)
		}

		if cfgToken != "" && cfgProxyid != "" {
			var ids []string
			// 提前判断多开
			if strings.Contains(cfgProxyid, ",") {
				ids = strings.Split(cfgProxyid, ",")
			} else {
				ids = []string{cfgProxyid}
			}

			if len(ids) > 1 {
				for i := 0; i < len(ids); i++ {
					// 每一个都是新的协程
					wg.Add(1)
					id := ids[i]
					go func(id string) {
						defer wg.Done()
						// != -1 -> 存在多开
						if strings.Contains(cfgProxyid, ",") {
							cfg, err := s.EZStartGetCfg(cfgToken, id)
							if err != nil {
								log.Warn("获取配置文件失败，请检查您的Args，err: %s", err)
								// 无法获取配置文件，直接关闭软件，防止启动上一个配置文件导致二次报错
								os.Exit(1)
							}

							file, err := os.OpenFile("./ini/"+id+".ini", os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0777)
							if err != nil {
								log.Warn("打开文件失败，错误: %s", err)
								os.Exit(1)
							}
							defer file.Close()
							str := cfg
							_, err = file.WriteString(str) //直接写入字符串数据
							// 写入文件是否成功检测
							if err != nil {
								log.Warn("文本在写入的过程中发生了致命错误, Err: %s", err)
								os.Exit(1)
							}

							// 内容写入后直接启动
							fliePath := "./ini/" + id + ".ini"
							err4 := runClient(fliePath, &wg)
							if err4 != nil {
								log.Warn("启动的过程中发生致命错误, Err: %s", err4)
								os.Exit(1)
							}
						}
					}(id)
				}
				wg.Wait()
				return nil
			}

			// 没有多开现象
			cfg, err := s.EZStartGetCfg(cfgToken, cfgProxyid)
			if err != nil {
				log.Warn("获取配置文件失败，请检查您的Args，err: %s", err)
				// 无法获取配置文件，直接关闭软件，防止启动上一个配置文件导致二次报错
				os.Exit(1)
			}

			// 删除原先文件，防止窜行
			// OpenFlie 函数用 os.O_RDWR|os.O_TRUNC|os.O_CREATE 可以直接覆盖原文本
			// os.RemoveAll("./frpc.ini")

			// 有则打开，无则新建
			file, err2 := os.OpenFile("./frpc.ini", os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0777)
			if err2 != nil {
				log.Warn("打开文件失败，错误: %s", err2)
			}
			str := cfg
			num, err3 := file.WriteString(str) //直接写入字符串数据
			// 写入文件是否成功检测
			if err3 != nil {
				log.Warn("文本在写入的过程中发生了致命错误, Err: %s", err3)
				os.Exit(1)
			}

			file.Close()
			log.Info("成功写入文本，字符数：%s", num)

			// 内容写入后直接启动
			err4 := runClient(cfgFile, &wg)
			if err4 != nil {
				log.Warn("启动的过程中发生致命错误, Err: %s", err4)
				os.Exit(1)
			}
			return nil
		}

		// // 控制第一次执行（忽略第一次执行）
		// firstTime := true
		// go func() {
		// 	for {
		// 		if firstTime {
		// 			firstTime = false
		// 		} else {

		// 		}

		// 		time.Sleep(10 * time.Second)
		// 	}
		// }()

		// Do not show command usage here.
		err = runClient(cfgFile, &wg)
		if err != nil {
			os.Exit(1)
		}
		return nil
	},
}

func runMultipleClients(cfgDir string) error {
	var wg sync.WaitGroup
	err := filepath.WalkDir(cfgDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		wg.Add(1)
		time.Sleep(time.Millisecond)
		go func() {
			defer wg.Done()
			err := runClient(path, &wg)
			if err != nil {
				fmt.Printf("配置文件的frpc服务错误 [%s]\n", path)
			}
		}()
		return nil
	})
	wg.Wait()
	return err
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func handleTermSignal(svr *client.Service) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	svr.GracefulClose(500 * time.Millisecond)
}

func parseClientCommonCfgFromCmd() (cfg config.ClientCommonConf, err error) {
	cfg = config.GetDefaultClientConf()

	ipStr, portStr, err := net.SplitHostPort(serverAddr)
	if err != nil {
		err = fmt.Errorf("无效的server_addr（服务器IP/域名）: %v", err)
		return
	}

	cfg.ServerAddr = ipStr
	cfg.ServerPort, err = strconv.Atoi(portStr)
	if err != nil {
		err = fmt.Errorf("无效的server_addr（服务器IP/域名）: %v", err)
		return
	}

	cfg.User = user
	cfg.Protocol = protocol
	cfg.LogLevel = logLevel
	cfg.LogFile = logFile
	cfg.LogMaxDays = int64(logMaxDays)
	cfg.DisableLogColor = disableLogColor
	cfg.DNSServer = dnsServer

	// Only token authentication is supported in cmd mode
	cfg.ClientConfig = auth.GetDefaultClientConf()
	cfg.Token = token
	cfg.TLSEnable = tlsEnable
	cfg.TLSServerName = tlsServerName

	cfg.Complete()
	if err = cfg.Validate(); err != nil {
		err = fmt.Errorf("解析配置错误: %v", err)
		return
	}
	return
}

func runClient(cfgFilePath string, wg *sync.WaitGroup) error {
	defer wg.Done()
	cfg, pxyCfgs, visitorCfgs, err := config.ParseClientConfig(cfgFilePath)
	if err != nil {
		fmt.Println(err)
		return err
	}
	return startService(cfg, pxyCfgs, visitorCfgs, cfgFilePath)
}

func startService(
	cfg config.ClientCommonConf,
	pxyCfgs map[string]config.ProxyConf,
	visitorCfgs map[string]config.VisitorConf,
	cfgFile string,
) (err error) {
	log.InitLog(cfg.LogWay, cfg.LogFile, cfg.LogLevel,
		cfg.LogMaxDays, cfg.DisableLogColor)

	if cfgFile != "" {
		log.Info("启动配置文件的frpc服务 [%s]", cfgFile)
		defer log.Info("配置文件的frpc服务 [%s] 停止", cfgFile)
	}
	svr, errRet := client.NewService(cfg, pxyCfgs, visitorCfgs, cfgFile)
	if errRet != nil {
		err = errRet
		return
	}

	shouldGracefulClose := cfg.Protocol == "kcp" || cfg.Protocol == "quic"
	// Capture the exit signal if we use kcp or quic.
	if shouldGracefulClose {
		go handleTermSignal(svr)
	}

	_ = svr.Run(context.Background())
	return
}
