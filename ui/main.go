package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/kranthi0003/SketchGate/proto"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var rdb *redis.Client
var grpcClient pb.RateLimiterClient

func main() {
	host := getEnv("REDIS_HOST", "localhost")
	port := getEnv("REDIS_PORT", "6379")
	grpcAddr := getEnv("GRPC_ADDR", "localhost:50051")

	rdb = redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%s", host, port),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("⚠️  Redis unavailable (%v) — dashboard will show empty data", err)
		rdb = nil
	} else {
		log.Println("✅ Connected to Redis")
	}

	// Connect to gRPC server
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("⚠️  gRPC unavailable (%v) — load testing disabled", err)
	} else {
		grpcClient = pb.NewRateLimiterClient(conn)
		log.Printf("✅ Connected to gRPC server at %s", grpcAddr)
	}

	http.HandleFunc("/", serveDashboard)
	http.HandleFunc("/api/stats", handleStats)
	http.HandleFunc("/api/penalties", handlePenalties)
	http.HandleFunc("/api/health", handleHealth)
	http.HandleFunc("/api/loadtest", handleLoadTest)

	log.Println("🖥️  SketchGate Dashboard → http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

type statsResponse struct {
	TotalRequests  int64      `json:"total_requests"`
	TopKeys        []keyCount `json:"top_keys"`
	RedisConnected bool       `json:"redis_connected"`
}

type keyCount struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

type penaltyResponse struct {
	Entries []penaltyEntry `json:"entries"`
	Total   int64          `json:"total"`
}

type penaltyEntry struct {
	Key        string  `json:"key"`
	Violations float64 `json:"violations"`
}

type loadTestRequest struct {
	RPS      int  `json:"rps"`
	Users    int  `json:"users"`
	Duration int  `json:"duration"` // seconds
	Burst    bool `json:"burst"`
}

type loadTestResponse struct {
	Total      uint64  `json:"total"`
	Allowed    uint64  `json:"allowed"`
	Denied     uint64  `json:"denied"`
	Errors     uint64  `json:"errors"`
	AvgLatency float64 `json:"avg_latency_ms"`
	Throughput float64 `json:"throughput"`
	AllowedPct float64 `json:"allowed_pct"`
	DeniedPct  float64 `json:"denied_pct"`
	Penalties  []penaltyEntry `json:"penalties"`
}

func handleLoadTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if grpcClient == nil {
		http.Error(w, `{"error":"gRPC server not connected"}`, http.StatusServiceUnavailable)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
		return
	}

	// Parse params from JSON body or query params
	var req loadTestRequest
	if r.Header.Get("Content-Type") == "application/json" {
		json.NewDecoder(r.Body).Decode(&req)
	}
	if req.RPS == 0 {
		req.RPS, _ = strconv.Atoi(r.URL.Query().Get("rps"))
	}
	if req.Users == 0 {
		req.Users, _ = strconv.Atoi(r.URL.Query().Get("users"))
	}
	if req.Duration == 0 {
		req.Duration, _ = strconv.Atoi(r.URL.Query().Get("duration"))
	}

	// Defaults and safety limits
	if req.RPS <= 0 { req.RPS = 100 }
	if req.RPS > 1000 { req.RPS = 1000 }
	if req.Users <= 0 { req.Users = 5 }
	if req.Users > 50 { req.Users = 50 }
	if req.Duration <= 0 { req.Duration = 5 }
	if req.Duration > 30 { req.Duration = 30 }

	// Run load test
	ctx := context.Background()
	dur := time.Duration(req.Duration) * time.Second
	interval := time.Second / time.Duration(req.RPS)

	var totalAllowed, totalDenied, totalErrors uint64
	var totalLatency int64
	var wg sync.WaitGroup

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.After(dur)
	done := make(chan struct{})
	go func() { <-deadline; close(done) }()

loop:
	for {
		select {
		case <-done:
			break loop
		case <-ticker.C:
			wg.Add(1)
			go func() {
				defer wg.Done()
				var key string
				if req.Burst {
					key = "noisy-neighbor"
				} else {
					key = fmt.Sprintf("user-%d", rand.Intn(req.Users))
				}
				start := time.Now()
				resp, err := grpcClient.CheckRate(ctx, &pb.CheckRateRequest{Key: key, Tokens: 1})
				elapsed := time.Since(start)
				atomic.AddInt64(&totalLatency, elapsed.Microseconds())
				if err != nil {
					atomic.AddUint64(&totalErrors, 1)
					return
				}
				if resp.Allowed {
					atomic.AddUint64(&totalAllowed, 1)
				} else {
					atomic.AddUint64(&totalDenied, 1)
				}
			}()
		}
	}
	wg.Wait()

	total := totalAllowed + totalDenied + totalErrors
	avgLatency := float64(0)
	if total > 0 {
		avgLatency = float64(totalLatency) / float64(total) / 1000.0
	}

	resp := loadTestResponse{
		Total:      total,
		Allowed:    totalAllowed,
		Denied:     totalDenied,
		Errors:     totalErrors,
		AvgLatency: avgLatency,
		Throughput: float64(total) / dur.Seconds(),
		AllowedPct: pct(totalAllowed, total),
		DeniedPct:  pct(totalDenied, total),
	}

	// Fetch penalty queue after test
	pq, err := grpcClient.GetPenaltyQueue(ctx, &pb.GetPenaltyQueueRequest{TopN: 5})
	if err == nil {
		for _, e := range pq.Entries {
			resp.Penalties = append(resp.Penalties, penaltyEntry{Key: e.Key, Violations: e.Violations})
		}
	}

	json.NewEncoder(w).Encode(resp)
}

func pct(n, total uint64) float64 {
	if total == 0 { return 0 }
	return float64(n) / float64(total) * 100
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := statsResponse{RedisConnected: rdb != nil}

	if rdb != nil {
		ctx := r.Context()
		total, _ := rdb.Get(ctx, "sketchgate:cms:total").Int64()
		resp.TotalRequests = total

		iter := rdb.Scan(ctx, 0, "sketchgate:ratelimit:*", 50).Iterator()
		for iter.Next(ctx) {
			key := iter.Val()
			count, _ := rdb.ZCard(ctx, key).Result()
			if count > 0 {
				shortKey := key[len("sketchgate:ratelimit:"):]
				resp.TopKeys = append(resp.TopKeys, keyCount{Key: shortKey, Count: count})
			}
		}
	}

	// If gRPC is connected, also get frequency data from the server
	if grpcClient != nil {
		ctx := r.Context()
		health, err := grpcClient.Health(ctx, &pb.HealthRequest{})
		if err == nil {
			resp.RedisConnected = health.RedisStatus == "connected"
		}
	}

	json.NewEncoder(w).Encode(resp)
}

func handlePenalties(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := penaltyResponse{}

	if grpcClient != nil {
		ctx := r.Context()
		pq, err := grpcClient.GetPenaltyQueue(ctx, &pb.GetPenaltyQueueRequest{TopN: 20})
		if err == nil {
			for _, e := range pq.Entries {
				resp.Entries = append(resp.Entries, penaltyEntry{Key: e.Key, Violations: e.Violations})
			}
			resp.Total = pq.TotalPenalized
		}
	} else if rdb != nil {
		ctx := r.Context()
		results, _ := rdb.ZRevRangeWithScores(ctx, "sketchgate:penalties", 0, 19).Result()
		for _, z := range results {
			resp.Entries = append(resp.Entries, penaltyEntry{Key: z.Member.(string), Violations: z.Score})
		}
		resp.Total, _ = rdb.ZCard(ctx, "sketchgate:penalties").Result()
	}

	json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := map[string]interface{}{
		"healthy":    true,
		"redis":      "disconnected",
		"grpc":       "disconnected",
		"uptime":     0,
	}

	if grpcClient != nil {
		h, err := grpcClient.Health(r.Context(), &pb.HealthRequest{})
		if err == nil {
			status["grpc"] = "connected"
			status["redis"] = h.RedisStatus
			status["uptime"] = h.UptimeSeconds
			status["healthy"] = h.Healthy
		}
	} else if rdb != nil {
		if err := rdb.Ping(r.Context()).Err(); err == nil {
			status["redis"] = "connected"
		}
	}

	json.NewEncoder(w).Encode(status)
}

func serveDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, dashboardHTML)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>SketchGate Dashboard</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: #0d1117; color: #c9d1d9; }
  .header { background: #161b22; border-bottom: 1px solid #30363d; padding: 16px 24px; display: flex; align-items: center; gap: 12px; }
  .header h1 { font-size: 20px; color: #58a6ff; }
  .header .badge { background: #238636; color: #fff; padding: 2px 8px; border-radius: 12px; font-size: 12px; }
  .header .badge.red { background: #da3633; }
  .header .badge.yellow { background: #d29922; color: #000; }
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(340px, 1fr)); gap: 16px; padding: 24px; }
  .card { background: #161b22; border: 1px solid #30363d; border-radius: 8px; padding: 20px; }
  .card h2 { font-size: 14px; text-transform: uppercase; color: #8b949e; margin-bottom: 12px; letter-spacing: 0.5px; }
  .card.full { grid-column: 1 / -1; }
  .stat { font-size: 36px; font-weight: 700; color: #58a6ff; }
  .stat-label { font-size: 13px; color: #8b949e; margin-top: 4px; }
  table { width: 100%; border-collapse: collapse; margin-top: 8px; }
  th, td { text-align: left; padding: 8px 12px; border-bottom: 1px solid #21262d; font-size: 14px; }
  th { color: #8b949e; font-weight: 600; }
  .bar { height: 6px; background: #238636; border-radius: 3px; transition: width 0.5s; }
  .bar.warn { background: #d29922; }
  .bar.danger { background: #da3633; }
  .empty { color: #484f58; font-style: italic; padding: 16px 0; }

  /* Load Test Panel */
  .lt-controls { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: 12px; margin-bottom: 16px; }
  .lt-field label { display: block; font-size: 12px; color: #8b949e; margin-bottom: 4px; text-transform: uppercase; }
  .lt-field input[type=range] { width: 100%; accent-color: #58a6ff; }
  .lt-field .val { font-size: 18px; font-weight: 700; color: #58a6ff; }
  .lt-toggle { display: flex; align-items: center; gap: 8px; margin-top: 14px; }
  .lt-toggle input { width: 18px; height: 18px; accent-color: #da3633; }
  .lt-toggle label { font-size: 14px; color: #f85149; font-weight: 600; }
  .lt-btn { background: #238636; color: #fff; border: none; padding: 12px 32px; border-radius: 6px; font-size: 16px; font-weight: 600; cursor: pointer; width: 100%; margin-top: 12px; transition: all 0.2s; }
  .lt-btn:hover { background: #2ea043; }
  .lt-btn:disabled { background: #21262d; color: #484f58; cursor: not-allowed; }
  .lt-btn.running { background: #d29922; color: #000; }

  /* Results */
  .lt-results { display: none; margin-top: 16px; padding-top: 16px; border-top: 1px solid #30363d; }
  .lt-results.show { display: block; }
  .result-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(120px, 1fr)); gap: 12px; margin-bottom: 12px; }
  .result-item { text-align: center; }
  .result-item .num { font-size: 28px; font-weight: 700; }
  .result-item .num.green { color: #3fb950; }
  .result-item .num.red { color: #f85149; }
  .result-item .num.blue { color: #58a6ff; }
  .result-item .lbl { font-size: 11px; color: #8b949e; text-transform: uppercase; margin-top: 2px; }

  /* Progress bar */
  .lt-progress { height: 4px; background: #21262d; border-radius: 2px; margin-top: 12px; overflow: hidden; display: none; }
  .lt-progress.show { display: block; }
  .lt-progress-fill { height: 100%; background: linear-gradient(90deg, #238636, #58a6ff); border-radius: 2px; transition: width 0.1s linear; }

  .refresh { color: #8b949e; font-size: 12px; text-align: center; padding: 12px; }
</style>
</head>
<body>
<div class="header">
  <h1>🛡️ SketchGate</h1>
  <span class="badge" id="health-badge">checking...</span>
  <span class="badge" id="redis-badge">Redis: ...</span>
  <span class="badge" id="grpc-badge">gRPC: ...</span>
</div>
<div class="grid">
  <!-- Load Test Panel -->
  <div class="card full">
    <h2>⚡ Load Test Console</h2>
    <div class="lt-controls">
      <div class="lt-field">
        <label>Requests/sec</label>
        <div class="val" id="rps-val">100</div>
        <input type="range" id="rps" min="10" max="1000" step="10" value="100" oninput="document.getElementById('rps-val').textContent=this.value">
      </div>
      <div class="lt-field">
        <label>Simulated Users</label>
        <div class="val" id="users-val">5</div>
        <input type="range" id="users" min="1" max="50" value="5" oninput="document.getElementById('users-val').textContent=this.value">
      </div>
      <div class="lt-field">
        <label>Duration (sec)</label>
        <div class="val" id="dur-val">5</div>
        <input type="range" id="duration" min="1" max="30" value="5" oninput="document.getElementById('dur-val').textContent=this.value">
      </div>
      <div class="lt-field">
        <div class="lt-toggle">
          <input type="checkbox" id="burst">
          <label for="burst">🔥 DDoS / Burst Mode</label>
        </div>
        <div style="font-size:12px; color:#8b949e; margin-top:6px;">Single noisy neighbor</div>
      </div>
    </div>
    <button class="lt-btn" id="run-btn" onclick="runLoadTest()">🚀 Launch Load Test</button>
    <div class="lt-progress" id="progress"><div class="lt-progress-fill" id="progress-fill" style="width:0%"></div></div>

    <div class="lt-results" id="results">
      <div class="result-grid">
        <div class="result-item"><div class="num blue" id="r-total">—</div><div class="lbl">Total Requests</div></div>
        <div class="result-item"><div class="num green" id="r-allowed">—</div><div class="lbl">Allowed</div></div>
        <div class="result-item"><div class="num red" id="r-denied">—</div><div class="lbl">Denied</div></div>
        <div class="result-item"><div class="num blue" id="r-latency">—</div><div class="lbl">Avg Latency</div></div>
        <div class="result-item"><div class="num blue" id="r-throughput">—</div><div class="lbl">Throughput</div></div>
        <div class="result-item"><div class="num" id="r-errors" style="color:#d29922">—</div><div class="lbl">Errors</div></div>
      </div>
      <!-- Visual bar -->
      <div style="display:flex;height:24px;border-radius:4px;overflow:hidden;margin:8px 0;">
        <div id="bar-allowed" style="background:#3fb950;transition:width 0.5s" title="Allowed"></div>
        <div id="bar-denied" style="background:#f85149;transition:width 0.5s" title="Denied"></div>
      </div>
      <div style="display:flex;justify-content:space-between;font-size:12px;color:#8b949e;">
        <span>✅ Allowed: <span id="r-allowed-pct">0</span>%</span>
        <span>❌ Denied: <span id="r-denied-pct">0</span>%</span>
      </div>
      <div id="r-penalties" style="margin-top:12px;"></div>
    </div>
  </div>

  <div class="card">
    <h2>📊 Server Uptime</h2>
    <div class="stat" id="uptime">—</div>
    <div class="stat-label">seconds</div>
  </div>
  <div class="card">
    <h2>👥 Active Rate-Limited Keys</h2>
    <div id="active-keys"><div class="empty">No active keys</div></div>
  </div>
  <div class="card full">
    <h2>⚠️ Penalty Queue — Top Offenders</h2>
    <div id="penalty-queue"><div class="empty">No penalized users</div></div>
  </div>
</div>
<div class="refresh">Stats auto-refresh every 3 seconds</div>

<script>
let isRunning = false;

async function runLoadTest() {
  if (isRunning) return;
  isRunning = true;

  const btn = document.getElementById('run-btn');
  const progress = document.getElementById('progress');
  const progressFill = document.getElementById('progress-fill');
  const results = document.getElementById('results');

  const rps = parseInt(document.getElementById('rps').value);
  const users = parseInt(document.getElementById('users').value);
  const duration = parseInt(document.getElementById('duration').value);
  const burst = document.getElementById('burst').checked;

  btn.textContent = '⏳ Running...';
  btn.className = 'lt-btn running';
  btn.disabled = true;
  results.className = 'lt-results';
  progress.className = 'lt-progress show';

  // Animate progress bar
  let elapsed = 0;
  const progressInterval = setInterval(() => {
    elapsed += 100;
    const pct = Math.min((elapsed / (duration * 1000)) * 100, 95);
    progressFill.style.width = pct + '%';
  }, 100);

  try {
    const resp = await fetch('/api/loadtest', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({rps, users, duration, burst})
    });
    const data = await resp.json();

    clearInterval(progressInterval);
    progressFill.style.width = '100%';

    document.getElementById('r-total').textContent = data.total.toLocaleString();
    document.getElementById('r-allowed').textContent = data.allowed.toLocaleString();
    document.getElementById('r-denied').textContent = data.denied.toLocaleString();
    document.getElementById('r-latency').textContent = data.avg_latency_ms.toFixed(2) + ' ms';
    document.getElementById('r-throughput').textContent = data.throughput.toFixed(0) + ' /s';
    document.getElementById('r-errors').textContent = data.errors;
    document.getElementById('r-allowed-pct').textContent = data.allowed_pct.toFixed(1);
    document.getElementById('r-denied-pct').textContent = data.denied_pct.toFixed(1);
    document.getElementById('bar-allowed').style.width = data.allowed_pct + '%';
    document.getElementById('bar-denied').style.width = data.denied_pct + '%';

    const penDiv = document.getElementById('r-penalties');
    if (data.penalties && data.penalties.length > 0) {
      penDiv.innerHTML = '<div style="font-size:13px;color:#f85149;margin-bottom:4px;">⚠️ Penalized after test:</div>' +
        data.penalties.map(p => '<code style="color:#f85149">' + p.key + '</code> → ' + p.violations + ' violations').join('<br>');
    } else {
      penDiv.innerHTML = '';
    }

    results.className = 'lt-results show';
  } catch (e) {
    clearInterval(progressInterval);
    alert('Load test failed: ' + e.message);
  }

  setTimeout(() => { progress.className = 'lt-progress'; }, 1000);
  btn.textContent = '🚀 Launch Load Test';
  btn.className = 'lt-btn';
  btn.disabled = false;
  isRunning = false;
}

async function fetchData() {
  try {
    const [penalties, health] = await Promise.all([
      fetch('/api/penalties').then(r => r.json()),
      fetch('/api/health').then(r => r.json()),
    ]);

    const hb = document.getElementById('health-badge');
    hb.textContent = health.healthy ? 'Healthy' : 'Unhealthy';
    hb.className = 'badge' + (health.healthy ? '' : ' red');

    const rb = document.getElementById('redis-badge');
    rb.textContent = 'Redis: ' + (health.redis || 'disconnected');
    rb.className = 'badge' + (health.redis === 'connected' ? '' : ' yellow');

    const gb = document.getElementById('grpc-badge');
    gb.textContent = 'gRPC: ' + (health.grpc || 'disconnected');
    gb.className = 'badge' + (health.grpc === 'connected' ? '' : ' red');

    document.getElementById('uptime').textContent = health.uptime || '—';

    const pqDiv = document.getElementById('penalty-queue');
    if (penalties.entries && penalties.entries.length > 0) {
      pqDiv.innerHTML = '<table><tr><th>User</th><th>Violations</th><th>Severity</th></tr>' +
        penalties.entries.map(e => {
          const sev = e.violations >= 10 ? '🔴 Critical' : e.violations >= 5 ? '🟡 Warning' : '🟢 Low';
          return '<tr><td><code>' + e.key + '</code></td><td>' + e.violations + '</td><td>' + sev + '</td></tr>';
        }).join('') + '</table><div class="stat-label" style="margin-top:8px">Total penalized: ' + penalties.total + '</div>';
    } else {
      pqDiv.innerHTML = '<div class="empty">No penalized users — all clear! ✅</div>';
    }
  } catch (e) {
    console.error('Fetch error:', e);
  }
}
fetchData();
setInterval(fetchData, 3000);
</script>
</body>
</html>`
