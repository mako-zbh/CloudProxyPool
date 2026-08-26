package dashboard

import (
	"cloud-proxy-pool/cloud"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// StatsReporter exposes stats from ProxyServer
type StatsReporter interface {
	GetStats() ProxyStats
}

// RequestRecord 单条代理请求流水 (供面板展示)
type RequestRecord struct {
	Time     string `json:"time"`
	Method   string `json:"method"`
	Host     string `json:"host"`
	Path     string `json:"path"`
	Status   int    `json:"status"`
	Duration int64  `json:"duration_ms"`
	Node     string `json:"node"`
	Error    string `json:"error,omitempty"`
}

type ProxyStats struct {
	TotalRequests   uint64
	SuccessRequests uint64
	FailedRequests  uint64
	StartedAt       time.Time
	Recent          []RequestRecord
	Nodes           []*cloud.Node
}

// Global interface to access proxy stats
var Reporter StatsReporter

// StartDashboard starts the monitoring web server
func StartDashboard(addr string, reporter StatsReporter) {
	Reporter = reporter

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(htmlContent))
	})

	http.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		if Reporter == nil {
			http.Error(w, "Stats not available", 503)
			return
		}
		stats := Reporter.GetStats()

		type NodeStat struct {
			Name       string `json:"name"`
			URL        string `json:"url"`
			Requests   uint64 `json:"requests"`
			FailTotal  uint64 `json:"fail_total"`
			Consecutive int   `json:"consecutive_failures"`
			Status     string `json:"status"`
			Cooldown   int    `json:"cooldown_sec"`
		}

		nodeStats := make([]NodeStat, len(stats.Nodes))
		for i, n := range stats.Nodes {
			requests, failTotal, consecutive, cooldownSec := n.Stats()
			status := "healthy"
			if !n.IsHealthy() {
				status = "cooling"
			}
			nodeStats[i] = NodeStat{
				Name:       n.Name,
				URL:        n.URL,
				Requests:   requests,
				FailTotal:  failTotal,
				Consecutive: consecutive,
				Status:     status,
				Cooldown:   cooldownSec,
			}
		}

		recent := stats.Recent
		if recent == nil {
			recent = []RequestRecord{}
		}

		resp := map[string]interface{}{
			"total":      stats.TotalRequests,
			"success":    stats.SuccessRequests,
			"failed":     stats.FailedRequests,
			"started_at": stats.StartedAt.Unix(),
			"nodes":      nodeStats,
			"recent":     recent,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	log.Printf("监控面板正在监听: http://localhost%s", addr)
	go http.ListenAndServe(addr, nil)
}

// Embedded real-time dashboard (polls /api/stats every second, computes QPS client-side)
const htmlContent = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>Cloud ProxyPool 实时监控</title>
<style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { font-family: -apple-system, "PingFang SC", sans-serif; background: #0f1419; color: #d6d6d6; padding: 20px; }
    h2 { font-size: 16px; color: #eaeaea; margin-bottom: 12px; }
    .cards { display: grid; grid-template-columns: repeat(5, 1fr); gap: 12px; margin-bottom: 16px; }
    .card { background: #161b22; border: 1px solid #21262d; border-radius: 8px; padding: 14px; }
    .card .label { font-size: 12px; color: #8b949e; margin-bottom: 6px; }
    .card .value { font-size: 26px; font-weight: 600; color: #58a6ff; }
    .card .value.green { color: #3fb950; }
    .card .value.red { color: #f85149; }
    .panel { background: #161b22; border: 1px solid #21262d; border-radius: 8px; padding: 16px; margin-bottom: 16px; }
    canvas { width: 100%; height: 80px; display: block; }
    table { width: 100%; border-collapse: collapse; font-size: 13px; }
    th, td { text-align: left; padding: 6px 10px; border-bottom: 1px solid #21262d; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 420px; }
    th { color: #8b949e; font-weight: 500; }
    td.ok { color: #3fb950; } td.err { color: #f85149; } td.warn { color: #d29922; }
    .healthy { color: #3fb950; } .cooling { color: #f85149; }
    #recent tr.error-row td { color: #f85149; opacity: .85; }
</style>
</head>
<body>
    <div class="cards">
        <div class="card"><div class="label">实时 QPS</div><div class="value" id="qps">0</div></div>
        <div class="card"><div class="label">总请求</div><div class="value" id="total">0</div></div>
        <div class="card"><div class="label">成功率</div><div class="value green" id="rate">-</div></div>
        <div class="card"><div class="label">平均耗时</div><div class="value" id="avg">-</div></div>
        <div class="card"><div class="label">健康节点</div><div class="value green" id="healthy">-</div></div>
    </div>

    <div class="panel">
        <h2>QPS 趋势 (最近 60 秒)</h2>
        <canvas id="spark" width="1200" height="80"></canvas>
    </div>

    <div class="panel">
        <h2>云函数节点</h2>
        <table>
            <thead><tr><th>节点</th><th>函数 URL</th><th>承接请求</th><th>平台级失败</th><th>连续失败</th><th>状态</th></tr></thead>
            <tbody id="nodeBody"></tbody>
        </table>
    </div>

    <div class="panel">
        <h2>最近请求 (最新在前, 保留 200 条)</h2>
        <table>
            <thead><tr><th>时间</th><th>方法</th><th>目标</th><th>状态</th><th>耗时</th><th>节点</th><th>错误</th></tr></thead>
            <tbody id="recentBody"></tbody>
        </table>
    </div>

<script>
var prev = null;            // 上一轮采样 {total, t} 用于计算 QPS
var qpsHistory = [];        // 最近 60 秒 QPS 采样

function fmtDuration(ms) {
    if (ms >= 1000) return (ms/1000).toFixed(2) + ' s';
    return ms + ' ms';
}

function esc(s) {
    return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}

function renderNodes(nodes) {
    var tb = document.getElementById('nodeBody');
    var html = '';
    for (var i = 0; i < nodes.length; i++) {
        var n = nodes[i];
        var st = '<span class="' + n.status + '">' + (n.status === 'healthy' ? '健康' : '熔断 ' + n.cooldown_sec + 's') + '</span>';
        html += '<tr><td>' + esc(n.name) + '</td><td>' + esc(n.url.substring(0, 60)) + '</td><td>' + n.requests +
                '</td><td>' + n.fail_total + '</td><td>' + n.consecutive_failures + '</td><td>' + st + '</td></tr>';
    }
    tb.innerHTML = html;
}

function renderRecent(recent) {
    var tb = document.getElementById('recentBody');
    var html = '';
    var max = Math.min(recent.length, 30);
    for (var i = 0; i < max; i++) {
        var r = recent[i];
        var cls = '', st;
        if (r.status === 0) { cls = ' class="error-row"'; st = '<td class="err">失败</td>'; }
        else if (r.status >= 500) { st = '<td class="err">' + r.status + '</td>'; }
        else if (r.status >= 400) { st = '<td class="warn">' + r.status + '</td>'; }
        else { st = '<td class="ok">' + r.status + '</td>'; }
        html += '<tr' + cls + '><td>' + esc(r.time) + '</td><td>' + esc(r.method) + '</td><td title="' + esc(r.host + r.path) + '">' +
                esc(r.host) + esc(r.path.substring(0, 40)) + '</td>' + st + '<td>' + fmtDuration(r.duration_ms) +
                '</td><td>' + esc(r.node) + '</td><td title="' + esc(r.error || '') + '">' + esc((r.error||'').substring(0, 60)) + '</td></tr>';
    }
    tb.innerHTML = html;
}

function drawSpark() {
    var c = document.getElementById('spark');
    var ctx = c.getContext('2d');
    var w = c.width, h = c.height;
    ctx.clearRect(0, 0, w, h);
    if (qpsHistory.length < 2) return;
    var maxQ = Math.max.apply(null, qpsHistory.concat([1]));
    ctx.strokeStyle = '#58a6ff';
    ctx.lineWidth = 2;
    ctx.beginPath();
    for (var i = 0; i < qpsHistory.length; i++) {
        var x = w - (qpsHistory.length - 1 - i) * (w / 59);
        var y = h - 6 - (qpsHistory[i] / maxQ) * (h - 12);
        if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    }
    ctx.stroke();
    ctx.fillStyle = '#8b949e';
    ctx.font = '11px sans-serif';
    ctx.fillText('峰值 ' + maxQ.toFixed(1), 8, 14);
}

function update() {
    fetch('/api/stats').then(function(r){ return r.json(); }).then(function(data) {
        var now = Date.now();
        var qps = 0;
        if (prev) {
            var dt = (now - prev.t) / 1000;
            if (dt > 0) qps = (data.total - prev.total) / dt;
        }
        prev = { total: data.total, t: now };
        qpsHistory.push(qps);
        if (qpsHistory.length > 60) qpsHistory.shift();

        document.getElementById('qps').textContent = qps.toFixed(1);
        document.getElementById('total').textContent = data.total;
        var done = data.success + data.failed;
        document.getElementById('rate').textContent = done > 0 ? (data.success * 100 / done).toFixed(1) + '%' : '-';
        var avgMs = 0, n = 0;
        for (var i = 0; i < data.recent.length && i < 50; i++) { avgMs += data.recent[i].duration_ms; n++; }
        document.getElementById('avg').textContent = n > 0 ? fmtDuration(Math.round(avgMs / n)) : '-';
        var healthy = 0;
        for (var i = 0; i < data.nodes.length; i++) if (data.nodes[i].status === 'healthy') healthy++;
        document.getElementById('healthy').textContent = healthy + ' / ' + data.nodes.length;

        renderNodes(data.nodes);
        renderRecent(data.recent);
        drawSpark();
    }).catch(function(){});
}
setInterval(update, 1000);
update();
</script>
</body>
</html>
`
