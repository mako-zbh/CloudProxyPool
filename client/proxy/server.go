package proxy

import (
	"bufio"
	"bytes"
	"cloud-proxy-pool/cloud"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"cloud-proxy-pool/dashboard"

	"github.com/armon/go-socks5"
	"github.com/elazarl/goproxy"
	"github.com/elazarl/goproxy/ext/auth"
	"github.com/fatih/color"
)

type ProxyServer struct {
	Addr      string
	SocksAddr string
	User      string
	Password  string
	Dump      bool
	DumpFile  string
	Provider  *cloud.Provider
	Verbose   bool
	Quiet     bool // 静默模式: 关闭逐请求日志，只输出错误和周期统计
	dumpMu    sync.Mutex

	// Stats (Atomic)
	TotalRequests   uint64
	SuccessRequests uint64
	FailedRequests  uint64
}

// GetStats implements dashboard.StatsReporter
func (s *ProxyServer) GetStats() dashboard.ProxyStats {
	return dashboard.ProxyStats{
		TotalRequests:   atomic.LoadUint64(&s.TotalRequests),
		SuccessRequests: atomic.LoadUint64(&s.SuccessRequests),
		FailedRequests:  atomic.LoadUint64(&s.FailedRequests),
		Nodes:           s.Provider.Nodes,
	}
}

func NewProxyServer(addr, socksAddr, user, password string, dump bool, dumpFile string, provider *cloud.Provider, verbose, quiet bool) *ProxyServer {
	if dumpFile == "" {
		dumpFile = "traffic.log"
	}
	return &ProxyServer{
		Addr:      addr,
		SocksAddr: socksAddr,
		User:      user,
		Password:  password,
		Dump:      dump,
		DumpFile:  dumpFile,
		Provider:  provider,
		Verbose:   verbose,
		Quiet:     quiet,
	}
}

func (s *ProxyServer) Start() error {
	// 初始化 / 加载 CA 证书
	if err := LoadOrGenerateCA("certs"); err != nil {
		fmt.Printf("警告: 加载/生成 CA 证书失败: %v\n", err)
	} else {
		fmt.Println(" [证书] CA 证书已从 'certs/' 目录加载")
	}

	if s.Dump {
		fmt.Printf(" [录制] 流量录制已开启，输出文件: %s\n", s.DumpFile)
	}

	// 1. 启动 HTTP 代理 (主服务)
	go s.startHTTPProxy()

	// 2. 启动 SOCKS5 代理 (如果已配置)
	if s.SocksAddr != "" {
		go s.startSocks5Proxy()
	}

	// 3. 静默模式下周期输出运行统计
	if s.Quiet {
		go s.statsReporter()
	}

	// 阻塞主 Goroutine 防止退出
	select {}
}

// statsReporter 静默模式下每分钟输出一次累计统计 (仅在有新请求时)
func (s *ProxyServer) statsReporter() {
	var lastTotal, lastSuccess, lastFailed uint64
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		total := atomic.LoadUint64(&s.TotalRequests)
		if total == lastTotal {
			continue
		}
		success := atomic.LoadUint64(&s.SuccessRequests)
		failed := atomic.LoadUint64(&s.FailedRequests)
		fmt.Printf("[统计] 累计 %d 请求 | 本分钟 +%d (成功 +%d / 失败 +%d)\n",
			total, total-lastTotal, success-lastSuccess, failed-lastFailed)
		lastTotal, lastSuccess, lastFailed = total, success, failed
	}
}

func (s *ProxyServer) startHTTPProxy() {
	proxy := goproxy.NewProxyHttpServer()

	// 强制屏蔽 goproxy 内部的各种调试日志
	// 我们在 handleRequest 中有极其干净的自定义日志
	proxy.Logger = log.New(ioutil.Discard, "", 0)
	proxy.Verbose = false

	// 配置 Basic Auth 认证中间件
	if s.User != "" && s.Password != "" {
		// 对普通 HTTP 请求进行认证检查
		proxy.OnRequest().Do(auth.Basic("CloudProxy", func(user, passwd string) bool {
			return user == s.User && passwd == s.Password
		}))
	}

	// 配置 MITM (中间人攻击) 策略
	// 总是劫持 HTTPS 流量以便我们能"看到"并转发它
	// 注意: 客户端必须信任生成的 CA 证书
	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)

	// 核心逻辑: 自定义请求处理函数
	// 将所有流量重定向到 s.handleRequest
	proxy.OnRequest().DoFunc(func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		return r, s.handleRequest(r)
	})

	log.Printf("HTTP 代理服务正在监听: %s", s.Addr)
	if s.User != "" {
		log.Printf("已启用身份认证 (User: %s)", s.User)
	}
	if err := http.ListenAndServe(s.Addr, proxy); err != nil {
		log.Fatalf("HTTP 代理启动失败: %v", err)
	}
}

// startSocks5Proxy 启动 SOCKS5 服务端
func (s *ProxyServer) startSocks5Proxy() {
	conf := &socks5.Config{
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// 策略路由：
			// 1. 如果是 HTTP (端口 80, 8080)，使用内部透明管道 (避免 MITM 强制 TLS)
			// 2. 如果是 HTTPS (端口 443)，使用 CONNECT 桥接到本地代理
			host, port, _ := net.SplitHostPort(addr)

			// 云函数出口网络仅有 IPv4，IPv6 目标 (如微信 mmtls 的 240e: 地址) 必然失败；
			// 在 SOCKS 握手阶段直接拒绝，让应用立即回退到 IPv4 服务器，
			// 避免连接"假成功"后中途收到 502 破坏上层协议
			if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
				return nil, fmt.Errorf("IPv6 目标不支持 (云函数出口仅 IPv4): %s", addr)
			}

			if port == "80" || port == "8080" {
				// 创建内存管道
				clientConn, serverConn := net.Pipe()
				// 启动内部 HTTP 处理协程 (异步)
				go s.handleSocksHTTP(serverConn, addr)

				// 关键修复: 包装 clientConn 以伪装成 TCP 连接
				return &FakeTCPConn{clientConn}, nil
			}

			// 2. 连接到本地 HTTP 代理 (HTTPS / 其他)
			proxyConn, err := net.Dial("tcp", s.Addr)
			if err != nil {
				return nil, fmt.Errorf("连接本地 HTTP 代理失败: %v", err)
			}

			// 发送 HTTP CONNECT 指令
			connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
			if s.User != "" && s.Password != "" {
				auth := base64.StdEncoding.EncodeToString([]byte(s.User + ":" + s.Password))
				connectReq = fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", addr, addr, auth)
			}

			_, err = proxyConn.Write([]byte(connectReq))
			if err != nil {
				proxyConn.Close()
				return nil, fmt.Errorf("发送 HTTP CONNECT 失败: %v", err)
			}

			// 读取响应（循环读到响应头结束，避免分包漏读）
			buf := make([]byte, 1024)
			var respHead []byte
			for {
				n, err := proxyConn.Read(buf)
				if n > 0 {
					respHead = append(respHead, buf[:n]...)
					if bytes.Contains(respHead, []byte("\r\n\r\n")) {
						break
					}
				}
				if err != nil {
					proxyConn.Close()
					return nil, fmt.Errorf("读取 HTTP 代理握手响应失败: %v", err)
				}
			}

			// goproxy 对 CONNECT 应答 "HTTP/1.0 200 OK"，按前缀判断状态行
			if bytes.HasPrefix(respHead, []byte("HTTP/1.1 200")) || bytes.HasPrefix(respHead, []byte("HTTP/1.0 200")) {
				return proxyConn, nil
			}

			proxyConn.Close()
			return nil, fmt.Errorf("HTTP 代理握手被拒绝: %s", respHead)
		},
		Logger: log.New(ioutil.Discard, "", 0),
	}

	server, err := socks5.New(conf)
	if err != nil {
		log.Printf("[错误] SOCKS5 服务初始化失败: %v", err)
		return
	}

	log.Printf("SOCKS5 代理服务正在监听: %s", s.SocksAddr)
	if err := server.ListenAndServe("tcp", s.SocksAddr); err != nil {
		log.Printf("[错误] SOCKS5 服务运行失败: %v", err)
	}
}

// handleSocksHTTP 处理通过 SOCKS5 进来的普通 HTTP 流量
// 这里的 conn 是一个内存管道 (net.Pipe)
// 修复: 每个连接只处理一个请求，避免管道复用引起的数据混乱
func (s *ProxyServer) handleSocksHTTP(conn net.Conn, targetAddr string) {
	defer conn.Close()

	reader := bufio.NewReader(conn)

	req, err := http.ReadRequest(reader)
	if err != nil {
		if err != io.EOF {
			log.Printf("[SOCKS-HTTP] 读取请求失败: %v", err)
		}
		return
	}

	// 修正 URL（SOCKS5 客户端发送的是相对路径）
	if req.URL.Scheme == "" {
		req.URL.Scheme = "http"
	}
	if req.URL.Host == "" {
		req.URL.Host = targetAddr
	}

	// 调用核心处理逻辑
	resp := s.handleRequest(req)

	// 将响应写回管道
	if err := resp.Write(conn); err != nil {
		log.Printf("[SOCKS-HTTP] 响应写入失败: %v", err)
	}
}

func (s *ProxyServer) handleRequest(r *http.Request) *http.Response {
	// Stat: Total ++
	atomic.AddUint64(&s.TotalRequests, 1)

	// 云函数出口仅有 IPv4，IPv6 字面量目标直接失败，不浪费函数调用
	if host := r.URL.Hostname(); host != "" {
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			atomic.AddUint64(&s.FailedRequests, 1)
			return goproxy.NewResponse(r, goproxy.ContentTypeText, http.StatusBadGateway,
				"Cloud Proxy: IPv6 target not supported (function egress is IPv4-only): "+host)
		}
	}

	startTime := time.Now()

	// 读取请求体
	bodyBytes, _ := ioutil.ReadAll(r.Body)
	r.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes)) // 恢复 body

	// 流量录制 (Dump Request)
	if s.Dump {
		s.dumpRequest(r, bodyBytes)
	}

	// 编码 Body
	isBase64 := true
	encodedBody := base64.StdEncoding.EncodeToString(bodyBytes)

	// 准备 Headers
	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	// 修复: Go 的 r.Header 不包含 Host，必须从 r.Host 手动添加
	// 否则云函数转发时会缺失 Host 头，导致部分站点 (如 myip.ipip.net) 返回 400/404
	if r.Host != "" {
		headers["Host"] = r.Host
	}

	payload := cloud.FunctionRequest{
		Method:       r.Method,
		URL:          r.URL.String(),
		Headers:      headers,
		Body:         encodedBody,
		IsBodyBase64: isBase64,
	}

	// 核心调用
	funcResp, err := s.Provider.Invoke(payload)
	duration := time.Since(startTime)

	if err != nil {
		// Stat: Failed ++
		atomic.AddUint64(&s.FailedRequests, 1)

		if !s.Quiet {
			fmt.Printf("[%s] %s %s -> 错误 (%v)\n",
				startTime.Format("15:04:05"), r.Method, r.URL.Host, duration)
		}
		log.Printf("[错误] 调用云函数失败 %s: %v", r.URL, err)

		if s.Dump {
			s.dumpError(r.URL.String(), err)
		}
		return goproxy.NewResponse(r, goproxy.ContentTypeText, http.StatusBadGateway, "Cloud Proxy Error: "+err.Error())
	}

	// Stat: Success ++
	atomic.AddUint64(&s.SuccessRequests, 1)

	// 日志输出 (静默模式下跳过)
	if !s.Quiet {
		statusColor := color.New(color.FgGreen).SprintFunc()
		if funcResp.StatusCode >= 400 {
			statusColor = color.New(color.FgRed).SprintFunc()
		} else if funcResp.StatusCode >= 300 {
			statusColor = color.New(color.FgYellow).SprintFunc()
		}

		fmt.Printf("[%s] %s %s -> %s (%v)\n",
			startTime.Format("15:04:05"),
			r.Method,
			r.URL.Host,
			statusColor(fmt.Sprintf("%d", funcResp.StatusCode)),
			duration.Round(time.Millisecond))
	}

	// 解码响应
	var respBody []byte
	if funcResp.IsContentBase64 {
		decoded, err := base64.StdEncoding.DecodeString(funcResp.Content)
		if err != nil {
			log.Printf("[错误] 解码响应内容失败: %v", err)
			return goproxy.NewResponse(r, goproxy.ContentTypeText, http.StatusBadGateway, "Response Decode Error")
		}
		respBody = decoded
	} else {
		respBody = []byte(funcResp.Content)
	}

	// 流量录制 (Dump Response)
	if s.Dump {
		s.dumpResponse(r.URL.String(), funcResp.StatusCode, len(respBody))
	}

	// 构造响应
	resp := &http.Response{
		Request:       r,
		StatusCode:    funcResp.StatusCode,
		Status:        http.StatusText(funcResp.StatusCode),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          ioutil.NopCloser(bytes.NewBuffer(respBody)),
		ContentLength: int64(len(respBody)),
	}

	for k, v := range funcResp.Headers {
		resp.Header.Set(k, v)
	}

	return resp
}

func (s *ProxyServer) dumpRequest(r *http.Request, body []byte) {
	s.dumpMu.Lock()
	defer s.dumpMu.Unlock()

	f, err := os.OpenFile(s.DumpFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[错误] 无法打开流量录制文件: %v", err)
		return
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	reqLine := fmt.Sprintf("[%s] REQUEST: %s %s\n", timestamp, r.Method, r.URL.String())
	f.WriteString(reqLine)

	// headers
	for k, v := range r.Header {
		f.WriteString(fmt.Sprintf("> %s: %s\n", k, v[0]))
	}
	f.WriteString("\n")

	// body preview (limit to 1KB)
	if len(body) > 0 {
		limit := 1024
		if len(body) < limit {
			limit = len(body)
		}
		f.WriteString(string(body[:limit]))
		if len(body) > limit {
			f.WriteString("\n... (body truncated)")
		}
		f.WriteString("\n")
	}
	f.WriteString("--------------------------------------------------\n")
}

func (s *ProxyServer) dumpResponse(url string, code int, size int) {
	s.dumpMu.Lock()
	defer s.dumpMu.Unlock()

	f, err := os.OpenFile(s.DumpFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	resLine := fmt.Sprintf("[%s] RESPONSE: %s -> %d (Size: %d bytes)\n", timestamp, url, code, size)
	f.WriteString(resLine)
	f.WriteString("==================================================\n\n")
}

func (s *ProxyServer) dumpError(url string, err error) {
	s.dumpMu.Lock()
	defer s.dumpMu.Unlock()

	f, err := os.OpenFile(s.DumpFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	errLine := fmt.Sprintf("[%s] ERROR: %s -> %v\n", timestamp, url, err)
	f.WriteString(errLine)
	f.WriteString("==================================================\n\n")
}

// FakeTCPConn wraps a net.Conn to return fake TCP addresses
// needed because armon/go-socks5 type assertions fail on net.Pipe()
type FakeTCPConn struct {
	net.Conn
}

func (c *FakeTCPConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
}

func (c *FakeTCPConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80}
}
