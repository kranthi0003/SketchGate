package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

var rdb *redis.Client

func main() {
	host := getEnv("REDIS_HOST", "localhost")
	port := getEnv("REDIS_PORT", "6379")

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

	http.HandleFunc("/", serveDashboard)
	http.HandleFunc("/api/stats", handleStats)
	http.HandleFunc("/api/penalties", handlePenalties)
	http.HandleFunc("/api/health", handleHealth)

	log.Println("🖥️  SketchGate Dashboard → http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

type statsResponse struct {
	TotalRequests int64            `json:"total_requests"`
	TopKeys       []keyCount       `json:"top_keys"`
	RedisConnected bool            `json:"redis_connected"`
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

func handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := statsResponse{RedisConnected: rdb != nil}

	if rdb != nil {
		ctx := r.Context()
		total, _ := rdb.Get(ctx, "sketchgate:cms:total").Int64()
		resp.TotalRequests = total

		// Scan for rate limit keys to show active users
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
	json.NewEncoder(w).Encode(resp)
}

func handlePenalties(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := penaltyResponse{}

	if rdb != nil {
		ctx := r.Context()
		results, _ := rdb.ZRevRangeWithScores(ctx, "sketchgate:penalties", 0, 19).Result()
		for _, z := range results {
			resp.Entries = append(resp.Entries, penaltyEntry{
				Key:        z.Member.(string),
				Violations: z.Score,
			})
		}
		resp.Total, _ = rdb.ZCard(ctx, "sketchgate:penalties").Result()
	}
	json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := map[string]interface{}{
		"healthy": true,
		"redis":   "disconnected",
	}
	if rdb != nil {
		if err := rdb.Ping(r.Context()).Err(); err == nil {
			status["redis"] = "connected"
		} else {
			status["redis"] = fmt.Sprintf("error: %v", err)
			status["healthy"] = false
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
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(340px, 1fr)); gap: 16px; padding: 24px; }
  .card { background: #161b22; border: 1px solid #30363d; border-radius: 8px; padding: 20px; }
  .card h2 { font-size: 14px; text-transform: uppercase; color: #8b949e; margin-bottom: 12px; letter-spacing: 0.5px; }
  .stat { font-size: 36px; font-weight: 700; color: #58a6ff; }
  .stat-label { font-size: 13px; color: #8b949e; margin-top: 4px; }
  table { width: 100%; border-collapse: collapse; margin-top: 8px; }
  th, td { text-align: left; padding: 8px 12px; border-bottom: 1px solid #21262d; font-size: 14px; }
  th { color: #8b949e; font-weight: 600; }
  .bar { height: 6px; background: #238636; border-radius: 3px; transition: width 0.5s; }
  .bar.warn { background: #d29922; }
  .bar.danger { background: #da3633; }
  .refresh { color: #8b949e; font-size: 12px; text-align: center; padding: 12px; }
  .empty { color: #484f58; font-style: italic; padding: 16px 0; }
</style>
</head>
<body>
<div class="header">
  <h1>🛡️ SketchGate</h1>
  <span class="badge" id="health-badge">checking...</span>
  <span class="badge" id="redis-badge">Redis: ...</span>
</div>
<div class="grid">
  <div class="card">
    <h2>📊 Total Requests Tracked</h2>
    <div class="stat" id="total-requests">—</div>
    <div class="stat-label">via Count-Min Sketch</div>
  </div>
  <div class="card">
    <h2>👥 Active Rate-Limited Keys</h2>
    <div id="active-keys">
      <div class="empty">No active keys</div>
    </div>
  </div>
  <div class="card" style="grid-column: 1 / -1;">
    <h2>⚠️ Penalty Queue — Top Offenders</h2>
    <div id="penalty-queue">
      <div class="empty">No penalized users</div>
    </div>
  </div>
</div>
<div class="refresh">Auto-refreshes every 2 seconds</div>
<script>
async function fetchData() {
  try {
    const [stats, penalties, health] = await Promise.all([
      fetch('/api/stats').then(r => r.json()),
      fetch('/api/penalties').then(r => r.json()),
      fetch('/api/health').then(r => r.json()),
    ]);

    // Health
    const hb = document.getElementById('health-badge');
    hb.textContent = health.healthy ? 'Healthy' : 'Unhealthy';
    hb.className = 'badge' + (health.healthy ? '' : ' red');
    const rb = document.getElementById('redis-badge');
    rb.textContent = 'Redis: ' + health.redis;
    rb.className = 'badge' + (health.redis === 'connected' ? '' : ' red');

    // Stats
    document.getElementById('total-requests').textContent = stats.total_requests.toLocaleString();

    // Active keys
    const akDiv = document.getElementById('active-keys');
    if (stats.top_keys && stats.top_keys.length > 0) {
      const maxCount = Math.max(...stats.top_keys.map(k => k.count));
      akDiv.innerHTML = '<table><tr><th>Key</th><th>Requests</th><th></th></tr>' +
        stats.top_keys.map(k => {
          const pct = (k.count / maxCount * 100).toFixed(0);
          const cls = pct > 80 ? 'danger' : pct > 50 ? 'warn' : '';
          return '<tr><td><code>' + k.key + '</code></td><td>' + k.count +
            '</td><td><div class="bar ' + cls + '" style="width:' + pct + '%"></div></td></tr>';
        }).join('') + '</table>';
    } else {
      akDiv.innerHTML = '<div class="empty">No active keys</div>';
    }

    // Penalties
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
setInterval(fetchData, 2000);
</script>
</body>
</html>`
