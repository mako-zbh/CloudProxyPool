package main

import (
	"cloud-proxy-pool/cloud"
	"cloud-proxy-pool/config"
	"cloud-proxy-pool/dashboard"
	"cloud-proxy-pool/proxy"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/fatih/color"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "C", "config.toml", "Path to configuration file")
	flag.Parse()

	// 1. 加载配置
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		color.Yellow("未找到配置文件，正在创建默认配置: %s...", configPath)
		if err := config.CreateDefaultConfig(configPath); err != nil {
			log.Fatalf("创建默认配置失败: %v", err)
		}
		color.Yellow("请编辑 %s 填入您的云函数 URL 后重启。", configPath)
		return
	}

	conf, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	if len(conf.Cloud.FunctionURLs) == 0 {
		color.Red("错误: 请在 %s 中设置有效的云函数 URL (function_urls)", configPath)
		return
	}

	// 2. 初始化云函数提供者
	provider := cloud.NewProvider(conf.Cloud.FunctionURLs, conf.Cloud.Token)

	// 3. 健康检查
	color.Cyan("正在执行健康检查 (Health Check)...")
	ip, err := provider.HealthCheck()
	if err != nil {
		color.Red("健康检查失败: %v", err)
		color.Red("请检查您的云函数 URL 配置以及网络连接。")
		// 遇到严重错误退出
		os.Exit(1)
	}

	// 4. 显示 Banner
	showBanner(conf, ip)

	// 5. 启动代理服务
	srv := proxy.NewProxyServer(
		conf.Client.ListenAddr,
		conf.Client.SocksAddr,
		conf.Client.User,
		conf.Client.Password,
		conf.Client.Dump,
		conf.Client.DumpFile,
		provider,
		conf.Client.Debug,
	)

	// 6. 启动 Web Dashboard (如果配置)
	if conf.Client.DashboardAddr != "" {
		go dashboard.StartDashboard(conf.Client.DashboardAddr, srv)
	}

	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}
}

func showBanner(conf *config.Config, ip string) {
	banner := `
   ________                __   ____                        ____             __
  / ____/ /___  __  ______/ /  / __ \_________  ____  __  _/ __ \____  ____ / /
 / /   / / __ \/ / / / __  /  / /_/ / ___/ __ \/ __ \/ / / / /_/ / __ \/ __ \/ / 
/ /___/ / /_/ / /_/ / /_/ /  / ____/ /  / /_/ / /_>  </ /_/ / ____/ /_/ / /_/ / /  
\____/_/\____/\__,_/\__,_/  /_/   /_/   \____/\___/\__, /_/     \____/\____/_/   
                                                  /____/                        
`
	color.HiBlue(banner)
	fmt.Println("================================================================")
	color.Green(" [客户端] 监听地址  : %s", conf.Client.ListenAddr)
	color.Green(" [云函数] 加载节点数: %d 个 Function URL", len(conf.Cloud.FunctionURLs))
	color.Green(" [状  态] 健康检查  : 通过 (PASS)")
	color.Green(" [云  端] 当前出口IP: %s (随请求自动轮换)", ip)
	fmt.Println("================================================================")
	fmt.Println("MITM 加密代理已就绪。请配置您的工具 (如 Burp, 浏览器) 使用此代理。")
	fmt.Println("例如: export http_proxy=http://" + conf.Client.ListenAddr + " https_proxy=http://" + conf.Client.ListenAddr)
	fmt.Println("注意: 首次使用请务必安装 certs/ 目录下的 CA 证书，否则 HTTPS 会报错。")
}
