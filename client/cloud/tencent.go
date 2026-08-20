package cloud

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"time"
)

// FunctionRequest 定义发送给云函数的请求结构
type FunctionRequest struct {
	Method       string            `json:"method"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers"`
	Body         string            `json:"body"`
	IsBodyBase64 bool              `json:"is_body_base64"`
}

// FunctionResponse 定义从云函数返回的响应结构
type FunctionResponse struct {
	StatusCode      int               `json:"status_code"`
	Headers         map[string]string `json:"headers"`
	Content         string            `json:"content"` // Base64 编码的内容
	IsContentBase64 bool              `json:"is_content_base64"`
}

// Node Represents a cloud function node with health status
type Node struct {
	URL          string
	FailureCount int
	LastFailTime time.Time
	mu           sync.RWMutex
}

// MarkFailure records a failure for the node
func (n *Node) MarkFailure() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.FailureCount++
	n.LastFailTime = time.Now()
}

// MarkSuccess resets the failure count
func (n *Node) MarkSuccess() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.FailureCount > 0 {
		n.FailureCount = 0
	}
}

// IsHealthy checks if the node is healthy or in cooldown
func (n *Node) IsHealthy() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()

	// Threshold: 5 consecutive failures
	if n.FailureCount >= 5 {
		// Cooldown: 2 minutes
		if time.Since(n.LastFailTime) < 2*time.Minute {
			return false
		}
		// After cooldown, try recovering (reset count or allow one probe)
		// Here we allow it, but don't reset count immediately until success
		return true
	}
	return true
}

type Provider struct {
	Nodes        []*Node
	CurrentIndex int
	Client       *http.Client
	Token        string // 云函数鉴权 Token，随每个请求以 X-Auth-Token 头发送
}

func NewProvider(functionURLs []string, token string) *Provider {
	nodes := make([]*Node, len(functionURLs))
	for i, url := range functionURLs {
		nodes[i] = &Node{URL: url}
	}
	return &Provider{
		Nodes:        nodes,
		CurrentIndex: 0,
		Token:        token,
		Client: &http.Client{
			// 设置较长的超时时间以适应云函数冷启动或网络延迟 (90秒)
			Timeout: 90 * time.Second,
		},
	}
}

// getNextNode 获取下一个可用的节点 (Round Robin + Circuit Breaker)
func (p *Provider) getNextNode() *Node {
	if len(p.Nodes) == 0 {
		return nil
	}

	startIdx := p.CurrentIndex
	for i := 0; i < len(p.Nodes); i++ {
		idx := (startIdx + i) % len(p.Nodes)
		node := p.Nodes[idx]

		if node.IsHealthy() {
			p.CurrentIndex = (idx + 1) % len(p.Nodes)
			return node
		}
	}

	// 如果所有节点都熔断了，无奈返回当前节点（强行尝试）
	// 或者返回 nil? 返回当前节点比较好，死马当活马医
	log.Printf("[警告] 所有云函数节点均处于熔断状态，正在尝试使用: %s", p.Nodes[p.CurrentIndex].URL)
	node := p.Nodes[p.CurrentIndex]
	p.CurrentIndex = (p.CurrentIndex + 1) % len(p.Nodes)
	return node
}

// HealthCheck 验证云函数是否可达并返回出口 IP
func (p *Provider) HealthCheck() (string, error) {
	// 通过代理请求一个 IP echo 服务 (http://myip.ipip.net)
	// 我们手动构造一个 FunctionRequest

	payload := FunctionRequest{
		Method: "GET",
		URL:    "http://myip.ipip.net",
		Headers: map[string]string{
			"User-Agent": "CloudProxyPool-HealthCheck",
		},
	}

	// 简单策略：遍历所有节点只要有一个能通就可以
	var lastErr error

	for _, node := range p.Nodes {
		resp, err := p.invokeNode(node, payload)
		if err == nil && resp.StatusCode == 200 {
			content, err := base64.StdEncoding.DecodeString(resp.Content)
			if err == nil {
				return string(content), nil
			}
		}
		lastErr = err
	}

	return "", fmt.Errorf("所有云函数节点均不可用，最后错误: %v", lastErr)
}

// Invoke 调用云函数执行请求 (自动选择节点)
func (p *Provider) Invoke(payload FunctionRequest) (*FunctionResponse, error) {
	node := p.getNextNode()
	if node == nil {
		return nil, fmt.Errorf("未配置云函数 URL")
	}
	return p.invokeNode(node, payload)
}

// invokeNode 执行具体的节点调用并处理熔断计数
func (p *Provider) invokeNode(node *Node, payload FunctionRequest) (*FunctionResponse, error) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", node.URL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Token != "" {
		req.Header.Set("X-Auth-Token", p.Token)
	}

	resp, err := p.Client.Do(req)
	if err != nil {
		node.MarkFailure()
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		node.MarkFailure()
		body, _ := ioutil.ReadAll(resp.Body)
		// 只有 5xx 错误才算节点故障？ 4xx 可能是用户请求本身的问题
		// 这里暂且认为所有非200都是调用失败，或者可以细化
		return nil, fmt.Errorf("云函数调用失败: %s | 响应体: %s", resp.Status, string(body))
	}

	var funcResp FunctionResponse
	if err := json.NewDecoder(resp.Body).Decode(&funcResp); err != nil {
		node.MarkFailure() // 解码失败也算
		return nil, fmt.Errorf("解码云函数响应失败: %v", err)
	}

	// 成功！
	node.MarkSuccess()
	return &funcResp, nil
}
