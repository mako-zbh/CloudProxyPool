package dashboard

import (
	"cloud-proxy-pool/cloud"
	"encoding/json"
	"log"
	"net/http"
)

// StatsReporter exposes stats from ProxyServer
type StatsReporter interface {
	GetStats() ProxyStats
}

type ProxyStats struct {
	TotalRequests   uint64
	SuccessRequests uint64
	FailedRequests  uint64
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

		// Simplify node stats for JSON
		type NodeStat struct {
			URL          string `json:"url"`
			FailureCount int    `json:"failure_count"`
			Status       string `json:"status"`
			LastFail     string `json:"last_fail"`
		}

		nodeStats := make([]NodeStat, len(stats.Nodes))
		for i, n := range stats.Nodes {
			status := "Healthy"
			if !n.IsHealthy() {
				status = "Melting (Fused)"
			}
			lastFail := ""
			if !n.LastFailTime.IsZero() {
				lastFail = n.LastFailTime.Format("15:04:05")
			}

			nodeStats[i] = NodeStat{
				URL:          n.URL,
				FailureCount: n.FailureCount,
				Status:       status,
				LastFail:     lastFail,
			}
		}

		resp := map[string]interface{}{
			"total":   stats.TotalRequests,
			"success": stats.SuccessRequests,
			"failed":  stats.FailedRequests,
			"nodes":   nodeStats,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	log.Printf("监控面板正在监听: http://localhost%s", addr)
	go http.ListenAndServe(addr, nil)
}

// Embedded simple HTML dashboard
const htmlContent = `
<!DOCTYPE html>
<html>
<head>
    <title>Cloud ProxyPool Dashboard</title>
    <style>
        body { font-family: sans-serif; padding: 20px; background: #f0f2f5; }
        .card { background: white; padding: 20px; border-radius: 8px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); margin-bottom: 20px; }
        .stat-box { display: inline-block; margin-right: 40px; }
        .stat-val { font-size: 24px; font-weight: bold; color: #108ee9; }
        table { width: 100%; border-collapse: collapse; margin-top: 10px; }
        th, td { text-align: left; padding: 8px; border-bottom: 1px solid #ddd; }
        .status-Healthy { color: green; font-weight: bold; }
        .status-Melting { color: red; font-weight: bold; }
    </style>
</head>
<body>
    <div class="card">
        <h2>📊 实时监控 (Live Monitor)</h2>
        <div class="stat-box">Total: <span id="total" class="stat-val">0</span></div>
        <div class="stat-box">Success: <span id="success" class="stat-val">0</span></div>
        <div class="stat-box">Failed: <span id="failed" class="stat-val">0</span></div>
    </div>
    <div class="card">
        <h3>☁️ 云函数节点状态 (Nodes)</h3>
        <table id="nodeTable">
            <thead><tr><th>URL</th><th>Failures</th><th>Status</th><th>Last Fail</th></tr></thead>
            <tbody></tbody>
        </table>
    </div>
    <script>
        function update() {
            fetch('/api/stats').then(r => r.json()).then(data => {
                document.getElementById('total').innerText = data.total;
                document.getElementById('success').innerText = data.success;
                document.getElementById('failed').innerText = data.failed;
                
                const tbody = document.querySelector('#nodeTable tbody');
                tbody.innerHTML = '';
                data.nodes.forEach(n => {
                    const row = ` + "`<tr><td>${n.url.substring(0, 50)}...</td><td>${n.failure_count}</td><td class='status-${n.status.split(' ')[0]}'>${n.status}</td><td>${n.last_fail}</td></tr>`" + `;
                    tbody.innerHTML += row;
                });
            });
        }
        setInterval(update, 2000);
        update();
    </script>
</body>
</html>
`
