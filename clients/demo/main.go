package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	pb "github.com/kranthi0003/SketchGate/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	addr := flag.String("addr", "localhost:50051", "gRPC server address")
	key := flag.String("key", "demo-user", "user key to test")
	count := flag.Int("n", 5, "number of requests to send")
	flag.Parse()

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer conn.Close()
	client := pb.NewRateLimiterClient(conn)
	ctx := context.Background()

	fmt.Println("🛡️  SketchGate Demo Client")
	fmt.Printf("   Server: %s | Key: %s\n\n", *addr, *key)

	// 1. Health check
	fmt.Println("─── Health Check ───")
	health, err := client.Health(ctx, &pb.HealthRequest{})
	if err != nil {
		log.Fatalf("health check failed: %v", err)
	}
	fmt.Printf("  Healthy: %v | Redis: %s | Uptime: %ds\n\n", health.Healthy, health.RedisStatus, health.UptimeSeconds)

	// 2. Send requests
	fmt.Println("─── Sending Requests ───")
	for i := 1; i <= *count; i++ {
		resp, err := client.CheckRate(ctx, &pb.CheckRateRequest{Key: *key, Tokens: 1})
		if err != nil {
			fmt.Printf("  #%d ⚠️  Error: %v\n", i, err)
			continue
		}

		status := "✅ Allowed"
		if !resp.Allowed {
			status = "❌ Denied"
		}
		penalty := ""
		if resp.Penalized {
			penalty = fmt.Sprintf(" [PENALIZED: %.0f]", resp.PenaltyScore)
		}

		resetIn := time.Until(time.UnixMilli(resp.ResetAtMs)).Round(time.Millisecond)
		fmt.Printf("  #%d %s | Remaining: %d | Reset in: %s%s\n",
			i, status, resp.Remaining, resetIn, penalty)
	}

	// 3. Get status
	fmt.Println("\n─── User Status ───")
	status, err := client.GetStatus(ctx, &pb.GetStatusRequest{Key: *key})
	if err != nil {
		log.Printf("status error: %v", err)
	} else {
		fmt.Printf("  Requests: %d / %d | Frequency: %d\n",
			status.RequestCount, status.Limit, status.FrequencyEstimate)
		fmt.Printf("  Penalized: %v (score: %.0f)\n", status.Penalized, status.PenaltyScore)
		if len(status.TierTokens) > 0 {
			fmt.Println("  Token buckets:")
			for tier, tokens := range status.TierTokens {
				fmt.Printf("    %s: %d remaining\n", tier, tokens)
			}
		}
	}

	// 4. Frequency estimate
	fmt.Println("\n─── Frequency Estimate (CMS) ───")
	freq, err := client.EstimateFrequency(ctx, &pb.EstimateFrequencyRequest{Key: *key})
	if err != nil {
		log.Printf("frequency error: %v", err)
	} else {
		fmt.Printf("  Key %q: ~%d requests (total tracked: %d)\n", *key, freq.Estimate, freq.Total)
	}

	// 5. Penalty queue
	fmt.Println("\n─── Penalty Queue ───")
	pq, err := client.GetPenaltyQueue(ctx, &pb.GetPenaltyQueueRequest{TopN: 5})
	if err != nil {
		log.Printf("penalty queue error: %v", err)
	} else if len(pq.Entries) == 0 {
		fmt.Println("  Empty — all clear! ✅")
	} else {
		for _, e := range pq.Entries {
			fmt.Printf("  %s → %.0f violations\n", e.Key, e.Violations)
		}
		fmt.Printf("  Total penalized: %d\n", pq.TotalPenalized)
	}
}
