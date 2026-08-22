package config

import (
	"os"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Client ClientConfig `toml:"client"`
	Cloud  CloudConfig  `toml:"cloud"`
}

type ClientConfig struct {
	ListenAddr    string `toml:"listen_addr"`    // 本地监听地址
	SocksAddr     string `toml:"socks_addr"`     // [可选] HTP 代理同时监听 SOCKS5 (例如 ":10801")
	User          string `toml:"user"`           // [可选] Basic Auth 用户名
	Password      string `toml:"password"`       // [可选] Basic Auth 密码
	Dump          bool   `toml:"dump"`           // [可选] 是否开启流量录制
	DumpFile      string `toml:"dump_file"`      // [可选] 流量录制文件路径 (默认 "traffic.log")
	DashboardAddr string `toml:"dashboard_addr"` // [可选] 监控面板监听地址 (此字段需手动添加至 toml)
	Debug         bool   `toml:"debug"`          // 是否开启调试日志
	Quiet         bool   `toml:"quiet"`          // [可选] 静默模式: 关闭逐请求日志，只输出错误和周期统计
}

type CloudConfig struct {
	FunctionURLs []string `toml:"function_urls"`
	Region       string   `toml:"region"` // [可选] 区域标识 ("multi-region" 或具体区域)
	Token        string   `toml:"token"`  // [可选] 鉴权 Token
}

func LoadConfig(path string) (*Config, error) {
	var conf Config
	if _, err := toml.DecodeFile(path, &conf); err != nil {
		return nil, err
	}
	return &conf, nil
}

// CreateDefaultConfig 如果配置文件不存在，则创建一个默认配置
func CreateDefaultConfig(path string) error {
	defaultConf := Config{
		Client: ClientConfig{
			ListenAddr: "127.0.0.1:10800",
			// SocksAddr: ":10801",
			// User: "admin",
			// Password: "password",
			// Dump: true,
			// DumpFile: "traffic.log",
			// DashboardAddr: ":8081",
			Debug: false,
		},
		Cloud: CloudConfig{
			FunctionURLs: []string{
				"https://service-xxxxxx.sh.apigw.tencentcs.com/release/proxy_func_url_1",
				"https://service-yyyyyy.bj.apigw.tencentcs.com/release/proxy_func_url_2",
			},
			Region: "multi-region",
		},
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	return enc.Encode(defaultConf)
}
