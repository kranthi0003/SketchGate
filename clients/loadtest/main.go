package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/kranthi0003/SketchGate/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	addr := flag.String("addr", "localhost:50051", "gRPC server address")
	users := flag.Int("users", 10, "number of simulated users")
	rps := flag.Int("rps", 100, "total requests per second")
	duration := flag.Duration("duration", 10*time.Second, "test duration")
	burst := flag.Bool("burst", false, "simulate burst/DDoS from a single user")
	flag.Parse()

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer conn.Close()
	client := pb.NewRateLimiterClient(conn)

	// Health check
	ctx := context.Background()
	health, err := client.Health(ctx, &pb.HealthRequest{})
	if err != nil {
		log.Fatalf("health check failed: %v", err)
	}
	fmt.Printf("🛡️  SketchGate Load Tester\n")
	fmt.Printf("   Server: %s | Redis: %s | Uptime: %ds\n\n", *addr, health.RedisStatus, health.UptimeSeconds)

	var (
		totalAllowed uint64
		totalDenied  uint64
		totalErrors  uint64
		totalLatency int64
	)

	interval := time.Second / time.Duration(*rps)
	deadline := time.After(*duration)
	var wg sync.WaitGroup

	fmt.Printf("⚡ Sending ~%d req/s across %d users for %s", *rps, *users, *duration)
	if *burst {
		fmt.Print(" [BURST MODE — single noisy neighbor]")
	}
	fmt.Println("")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	done := make(chan struct{})
	go func() {
		<-deadline
		close(done)
	}()

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
				if *burst {
					key = "noisy-neighbor"
				} else {
					key = fmt.Sprintf("user-%d", rand.Intn(*users))
				}

				start := time.Now()
				resp, err := client.CheckRate(ctx, &pb.CheckRateRequest{
					Key:    key,
					Tokens: 1,
				})
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

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  📊 Load Test Results")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  Total requests:  %d\n", total)
	fmt.Printf("  ✅ Allowed:      %d (%.1f%%)\n", totalAllowed, pct(totalAllowed, total))
	fmt.Printf("  ❌ Denied:       %d (%.1f%%)\n", totalDenied, pct(totalDenied, total))
	fmt.Printf("  ⚠️  Errors:       %d\n", totalErrors)
	fmt.Printf("  ⏱️  Avg latency:  %.2f ms\n", avgLatency)
	fmt.Printf("  📈 Throughput:   %.0f req/s\n", float64(total)/duration.Seconds())
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Show penalty queue
	pq, err := client.GetPenaltyQueue(ctx, &pb.GetPenaltyQueueRequest{TopN: 5})
	if err == nil && len(pq.Entries) > 0 {
		fmt.Println("\n  ⚠️  Penalty Queue:")
		for _, e := range pq.Entries {
			fmt.Printf("    %s → %.0f violations\n", e.Key, e.Violations)
		}
	}
}

func pct(n, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}
