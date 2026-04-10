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
	http.HandleFunc("/api/verify", handleVerify)

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
	Proof      *proofData     `json:"proof,omitempty"`
	RequestLog []requestLogEntry `json:"request_log,omitempty"`
}

type proofData struct {
	Before map[string]userState `json:"before"`
	After  map[string]userState `json:"after"`
}

type userState struct {
	FrequencyEstimate uint64             `json:"frequency_estimate"`
	TierTokens        map[string]uint64  `json:"tier_tokens"`
	Remaining         int64              `json:"remaining"`
}

type requestLogEntry struct {
	Timestamp string `json:"ts"`
	Key       string `json:"key"`
	Allowed   bool   `json:"allowed"`
	LatencyUs int64  `json:"latency_us"`
	Remaining int64  `json:"remaining"`
}

type verifyResponse struct {
	Key               string            `json:"key"`
	FrequencyEstimate uint64            `json:"frequency_estimate"`
	TotalCMSEntries   uint64            `json:"total_cms_entries"`
	TierTokens        map[string]uint64 `json:"tier_tokens"`
	Remaining         int64             `json:"remaining"`
	ServerUptime      int64             `json:"server_uptime"`
}

// snapshotUserState queries the gRPC backend for a user's current state.
func snapshotUserState(ctx context.Context, key string) userState {
	us := userState{TierTokens: map[string]uint64{}}
	if grpcClient == nil {
		return us
	}
	st, err := grpcClient.GetStatus(ctx, &pb.GetStatusRequest{Key: key})
	if err == nil {
		us.Remaining = st.Remaining
		us.FrequencyEstimate = st.FrequencyEstimate
		for tier, tokens := range st.TierTokens {
			us.TierTokens[tier] = tokens
		}
	}
	freq, err := grpcClient.EstimateFrequency(ctx, &pb.EstimateFrequencyRequest{Key: key})
	if err == nil {
		us.FrequencyEstimate = freq.Estimate
	}
	return us
}

func handleVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if grpcClient == nil {
		http.Error(w, `{"error":"gRPC server not connected"}`, http.StatusServiceUnavailable)
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, `{"error":"key parameter required"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	resp := verifyResponse{Key: key, TierTokens: map[string]uint64{}}

	st, err := grpcClient.GetStatus(ctx, &pb.GetStatusRequest{Key: key})
	if err == nil {
		resp.Remaining = st.Remaining
		resp.FrequencyEstimate = st.FrequencyEstimate
		for tier, tokens := range st.TierTokens {
			resp.TierTokens[tier] = tokens
		}
	}
	freq, err := grpcClient.EstimateFrequency(ctx, &pb.EstimateFrequencyRequest{Key: key})
	if err == nil {
		resp.FrequencyEstimate = freq.Estimate
		resp.TotalCMSEntries = freq.Total
	}
	health, err := grpcClient.Health(ctx, &pb.HealthRequest{})
	if err == nil {
		resp.ServerUptime = health.UptimeSeconds
	}

	json.NewEncoder(w).Encode(resp)
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

	ctx := context.Background()

	// PROOF: snapshot state BEFORE test
	testKeys := []string{}
	if req.Burst {
		testKeys = append(testKeys, "noisy-neighbor")
	} else {
		for i := 0; i < req.Users; i++ {
			testKeys = append(testKeys, fmt.Sprintf("user-%d", i))
		}
	}
	beforeState := map[string]userState{}
	for _, k := range testKeys {
		beforeState[k] = snapshotUserState(ctx, k)
	}

	// Run load test
	dur := time.Duration(req.Duration) * time.Second
	interval := time.Second / time.Duration(req.RPS)

	var totalAllowed, totalDenied, totalErrors uint64
	var totalLatency int64
	var wg sync.WaitGroup

	// Capture a sample of individual requests for the live log (max 200)
	type logEntry struct {
		ts      time.Time
		key     string
		allowed bool
		latUs   int64
		remain  int64
	}
	logCh := make(chan logEntry, 5000)

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
				latUs := elapsed.Microseconds()
				atomic.AddInt64(&totalLatency, latUs)
				if err != nil {
					atomic.AddUint64(&totalErrors, 1)
					return
				}
				if resp.Allowed {
					atomic.AddUint64(&totalAllowed, 1)
				} else {
					atomic.AddUint64(&totalDenied, 1)
				}
				// Non-blocking send to log channel
				select {
				case logCh <- logEntry{ts: start, key: key, allowed: resp.Allowed, latUs: latUs, remain: resp.Remaining}:
				default:
				}
			}()
		}
	}
	wg.Wait()
	close(logCh)

	total := totalAllowed + totalDenied + totalErrors
	avgLatency := float64(0)
	if total > 0 {
		avgLatency = float64(totalLatency) / float64(total) / 1000.0
	}

	// PROOF: snapshot state AFTER test
	afterState := map[string]userState{}
	for _, k := range testKeys {
		afterState[k] = snapshotUserState(ctx, k)
	}

	// Collect sampled request log (take evenly-spaced samples, max 50)
	var allLogs []logEntry
	for e := range logCh {
		allLogs = append(allLogs, e)
	}
	var sampledLog []requestLogEntry
	maxSamples := 50
	step := 1
	if len(allLogs) > maxSamples {
		step = len(allLogs) / maxSamples
	}
	for i := 0; i < len(allLogs) && len(sampledLog) < maxSamples; i += step {
		e := allLogs[i]
		sampledLog = append(sampledLog, requestLogEntry{
			Timestamp: e.ts.Format("15:04:05.000"),
			Key:       e.key,
			Allowed:   e.allowed,
			LatencyUs: e.latUs,
			Remaining: e.remain,
		})
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
		Proof:      &proofData{Before: beforeState, After: afterState},
		RequestLog: sampledLog,
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

  /* Proof panel */
  .proof-section { margin-top: 16px; padding-top: 16px; border-top: 1px solid #30363d; display: none; }
  .proof-section.show { display: block; }
  .proof-header { display: flex; align-items: center; gap: 8px; cursor: pointer; user-select: none; margin-bottom: 10px; }
  .proof-header h3 { font-size: 14px; color: #f0883e; text-transform: uppercase; letter-spacing: 0.5px; }
  .proof-header .arrow { transition: transform 0.2s; font-size: 12px; color: #8b949e; }
  .proof-header .arrow.open { transform: rotate(90deg); }
  .proof-badge { display: inline-block; background: #f0883e22; border: 1px solid #f0883e; color: #f0883e; padding: 1px 8px; border-radius: 10px; font-size: 11px; font-weight: 600; }
  .proof-body { overflow: hidden; transition: max-height 0.3s ease; }
  .proof-table { width: 100%; border-collapse: collapse; font-size: 13px; font-family: 'SF Mono', Monaco, monospace; }
  .proof-table th { color: #8b949e; font-weight: 600; text-align: left; padding: 6px 10px; border-bottom: 1px solid #30363d; font-size: 11px; text-transform: uppercase; }
  .proof-table td { padding: 6px 10px; border-bottom: 1px solid #21262d; }
  .proof-table .delta { font-weight: 700; }
  .proof-table .delta.up { color: #f85149; }
  .proof-table .delta.down { color: #3fb950; }
  .proof-table .delta.same { color: #484f58; }

  /* Request log */
  .req-log { max-height: 220px; overflow-y: auto; background: #0d1117; border: 1px solid #21262d; border-radius: 6px; padding: 8px; margin-top: 8px; font-family: 'SF Mono', Monaco, monospace; font-size: 12px; line-height: 1.6; }
  .req-log .entry { display: flex; gap: 8px; }
  .req-log .ts { color: #484f58; min-width: 90px; }
  .req-log .key { color: #79c0ff; min-width: 110px; }
  .req-log .allow { color: #3fb950; font-weight: 600; }
  .req-log .deny { color: #f85149; font-weight: 600; }
  .req-log .lat { color: #8b949e; }
  .req-log .rem { color: #d2a8ff; }

  /* Verify card */
  .verify-input { display: flex; gap: 8px; }
  .verify-input input { flex: 1; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; padding: 10px 12px; color: #c9d1d9; font-size: 14px; font-family: 'SF Mono', Monaco, monospace; }
  .verify-input input:focus { outline: none; border-color: #58a6ff; }
  .verify-input button { background: #1f6feb; color: #fff; border: none; padding: 10px 20px; border-radius: 6px; font-weight: 600; cursor: pointer; white-space: nowrap; }
  .verify-input button:hover { background: #388bfd; }
  .verify-result { margin-top: 12px; display: none; }
  .verify-result.show { display: block; }
  .verify-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: 10px; margin-top: 8px; }
  .verify-item { background: #0d1117; border: 1px solid #21262d; border-radius: 6px; padding: 10px; text-align: center; }
  .verify-item .v-num { font-size: 22px; font-weight: 700; color: #58a6ff; font-family: 'SF Mono', Monaco, monospace; }
  .verify-item .v-lbl { font-size: 11px; color: #8b949e; text-transform: uppercase; margin-top: 2px; }
  .verify-item .v-num.hot { color: #f85149; }
  .verify-item .v-num.cool { color: #3fb950; }

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

      <!-- PROOF: Before/After State -->
      <div class="proof-section" id="proof-section">
        <div class="proof-header" onclick="toggleProof('proof')">
          <span class="arrow" id="proof-arrow">▶</span>
          <h3>🔬 Server-Side Proof</h3>
          <span class="proof-badge">VERIFIED BY gRPC BACKEND</span>
        </div>
        <div class="proof-body" id="proof-body">
          <p style="font-size:12px;color:#8b949e;margin-bottom:8px;">
            State captured directly from the gRPC server <strong>before</strong> and <strong>after</strong> the test.
            Δ columns show real changes in the rate limiter data structures.
          </p>
          <div id="proof-table-container"></div>
        </div>
      </div>

      <!-- PROOF: Request Log -->
      <div class="proof-section" id="log-section">
        <div class="proof-header" onclick="toggleProof('log')">
          <span class="arrow" id="log-arrow">▶</span>
          <h3>📜 Request Log (Sampled)</h3>
          <span class="proof-badge" id="log-count">0 ENTRIES</span>
        </div>
        <div class="proof-body" id="log-body">
          <p style="font-size:12px;color:#8b949e;margin-bottom:8px;">
            Each line is a real gRPC <code>CheckRate()</code> call with its actual response from the server.
          </p>
          <div class="req-log" id="req-log"></div>
        </div>
      </div>
    </div>
  </div>

  <!-- Independent Verify Tool -->
  <div class="card full">
    <h2>🔍 Independent Verify — Query Backend Directly</h2>
    <p style="font-size:13px;color:#8b949e;margin-bottom:12px;">
      Type any user key to query the gRPC server independently. If the load test was real, tested keys will show non-zero frequency. Untested keys will show zero.
    </p>
    <div class="verify-input">
      <input type="text" id="verify-key" placeholder="e.g. user-0, noisy-neighbor, phantom-user-999" onkeydown="if(event.key==='Enter')verifyKey()">
      <button onclick="verifyKey()">🔍 Verify</button>
    </div>
    <div class="verify-result" id="verify-result">
      <div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;">
        <span style="font-size:13px;color:#8b949e;">Querying gRPC server at <code>localhost:50051</code> →</span>
        <code style="color:#79c0ff;font-size:14px;" id="verify-key-display"></code>
      </div>
      <div class="verify-grid">
        <div class="verify-item"><div class="v-num" id="v-freq">—</div><div class="v-lbl">CMS Frequency</div></div>
        <div class="verify-item"><div class="v-num" id="v-total">—</div><div class="v-lbl">Total CMS Entries</div></div>
        <div class="verify-item"><div class="v-num" id="v-remain">—</div><div class="v-lbl">Window Remaining</div></div>
        <div class="verify-item"><div class="v-num" id="v-uptime">—</div><div class="v-lbl">Server Uptime (s)</div></div>
      </div>
      <div id="v-tokens" style="margin-top:8px;"></div>
      <div id="v-verdict" style="margin-top:10px;font-size:14px;font-weight:600;"></div>
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

function toggleProof(id) {
  const body = document.getElementById(id + '-body');
  const arrow = document.getElementById(id + '-arrow');
  if (body.style.maxHeight && body.style.maxHeight !== '0px') {
    body.style.maxHeight = '0px';
    arrow.className = 'arrow';
  } else {
    body.style.maxHeight = body.scrollHeight + 500 + 'px';
    arrow.className = 'arrow open';
  }
}

function renderProof(proof) {
  if (!proof || !proof.before || !proof.after) return;
  const container = document.getElementById('proof-table-container');
  const keys = Object.keys(proof.after);
  if (keys.length === 0) { container.innerHTML = '<div class="empty">No state data</div>'; return; }

  // Limit to first 10 keys for readability
  const showKeys = keys.slice(0, 10);
  let html = '<table class="proof-table"><tr><th>User Key</th><th>Freq Before</th><th>Freq After</th><th>Δ Frequency</th><th>Tokens Before</th><th>Tokens After</th></tr>';
  for (const k of showKeys) {
    const b = proof.before[k] || {frequency_estimate:0, tier_tokens:{}};
    const a = proof.after[k] || {frequency_estimate:0, tier_tokens:{}};
    const delta = (a.frequency_estimate||0) - (b.frequency_estimate||0);
    const cls = delta > 0 ? 'up' : delta < 0 ? 'down' : 'same';
    const bTokens = b.tier_tokens ? Object.entries(b.tier_tokens).map(([t,v])=>t+':'+v).join(', ') : '—';
    const aTokens = a.tier_tokens ? Object.entries(a.tier_tokens).map(([t,v])=>t+':'+v).join(', ') : '—';
    html += '<tr><td><code style="color:#79c0ff">' + k + '</code></td>';
    html += '<td>' + (b.frequency_estimate||0).toLocaleString() + '</td>';
    html += '<td>' + (a.frequency_estimate||0).toLocaleString() + '</td>';
    html += '<td class="delta ' + cls + '">' + (delta>0?'+':'') + delta.toLocaleString() + '</td>';
    html += '<td style="font-size:12px">' + bTokens + '</td>';
    html += '<td style="font-size:12px">' + aTokens + '</td></tr>';
  }
  html += '</table>';
  if (keys.length > 10) html += '<div style="font-size:12px;color:#8b949e;margin-top:6px;">...and ' + (keys.length-10) + ' more users</div>';
  container.innerHTML = html;
}

function renderRequestLog(log) {
  const container = document.getElementById('req-log');
  const badge = document.getElementById('log-count');
  if (!log || log.length === 0) {
    container.innerHTML = '<div class="empty">No log entries</div>';
    badge.textContent = '0 ENTRIES';
    return;
  }
  badge.textContent = log.length + ' ENTRIES';
  let html = '';
  for (const e of log) {
    const statusCls = e.allowed ? 'allow' : 'deny';
    const statusTxt = e.allowed ? 'ALLOW' : 'DENY ';
    html += '<div class="entry">';
    html += '<span class="ts">' + e.ts + '</span>';
    html += '<span class="key">' + e.key + '</span>';
    html += '<span class="' + statusCls + '">' + statusTxt + '</span>';
    html += '<span class="lat">' + (e.latency_us/1000).toFixed(2) + 'ms</span>';
    html += '<span class="rem">remain:' + e.remaining + '</span>';
    html += '</div>';
  }
  container.innerHTML = html;
}

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

  // Hide proof sections while running
  document.getElementById('proof-section').className = 'proof-section';
  document.getElementById('log-section').className = 'proof-section';

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

    // Show proof panels
    if (data.proof) {
      renderProof(data.proof);
      document.getElementById('proof-section').className = 'proof-section show';
      // Auto-expand proof
      const proofBody = document.getElementById('proof-body');
      proofBody.style.maxHeight = proofBody.scrollHeight + 500 + 'px';
      document.getElementById('proof-arrow').className = 'arrow open';
    }
    if (data.request_log && data.request_log.length > 0) {
      renderRequestLog(data.request_log);
      document.getElementById('log-section').className = 'proof-section show';
      const logBody = document.getElementById('log-body');
      logBody.style.maxHeight = logBody.scrollHeight + 500 + 'px';
      document.getElementById('log-arrow').className = 'arrow open';
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

async function verifyKey() {
  const keyInput = document.getElementById('verify-key');
  const key = keyInput.value.trim();
  if (!key) { keyInput.focus(); return; }

  document.getElementById('verify-key-display').textContent = key;
  document.getElementById('v-freq').textContent = '...';
  document.getElementById('v-total').textContent = '...';
  document.getElementById('v-remain').textContent = '...';
  document.getElementById('v-uptime').textContent = '...';
  document.getElementById('verify-result').className = 'verify-result show';

  try {
    const resp = await fetch('/api/verify?key=' + encodeURIComponent(key));
    const data = await resp.json();

    const freq = data.frequency_estimate || 0;
    const freqEl = document.getElementById('v-freq');
    freqEl.textContent = freq.toLocaleString();
    freqEl.className = freq > 0 ? 'v-num hot' : 'v-num cool';

    document.getElementById('v-total').textContent = (data.total_cms_entries || 0).toLocaleString();
    document.getElementById('v-remain').textContent = (data.remaining || 0).toLocaleString();
    document.getElementById('v-uptime').textContent = (data.server_uptime || 0).toLocaleString();

    const tokDiv = document.getElementById('v-tokens');
    if (data.tier_tokens && Object.keys(data.tier_tokens).length > 0) {
      tokDiv.innerHTML = '<div style="font-size:12px;color:#8b949e;margin-bottom:4px;">Token Bucket Tiers:</div>' +
        Object.entries(data.tier_tokens).map(([tier,tokens]) =>
          '<code style="color:#d2a8ff">' + tier + '</code>: <strong>' + tokens + '</strong> tokens remaining'
        ).join(' &nbsp;│&nbsp; ');
    } else {
      tokDiv.innerHTML = '';
    }

    const verdict = document.getElementById('v-verdict');
    if (freq > 0) {
      verdict.innerHTML = '✅ <span style="color:#3fb950">CONFIRMED:</span> This key has been seen by the rate limiter ' + freq.toLocaleString() + ' times. The load test was real.';
    } else {
      verdict.innerHTML = '🔵 <span style="color:#58a6ff">NOT FOUND:</span> This key has zero frequency — it was never sent to the rate limiter. Try a key from the load test (e.g. <code>user-0</code>).';
    }
  } catch (e) {
    document.getElementById('v-verdict').innerHTML = '❌ <span style="color:#f85149">Error: ' + e.message + '</span>';
  }
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
