package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	pb "github.com/kranthi0003/SketchGate/proto"
	sgredis "github.com/kranthi0003/SketchGate/redis"
	"github.com/kranthi0003/SketchGate/service"
)

func main() {
	port := flag.Int("port", 50051, "gRPC server port")
	redisHost := flag.String("redis-host", "localhost", "Redis host")
	redisPort := flag.Int("redis-port", 6379, "Redis port")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	banner := `
   _____ _        _       _      ____       _       
  / ____| |      | |     | |    / ___| __ _| |_ ___ 
  \____ | | _____| |_ ___| |__ | |  _ / _' | __/ _ \
  |___) | |/ / _ \ __/ __| '_ \| |_| | (_| | ||  __/
  |____/|   <  __/ || (__| | | |\____|\__,_|\__\___|
        |_|\_\___|\__\___|_| |_|                     
  Distributed Rate Limiter — v0.1.0
`
	fmt.Println(banner)

	// Connect to Redis (optional — degrades to in-memory mode)
	var redisClient *sgredis.Client
	redisCfg := sgredis.Config{
		Host: *redisHost,
		Port: *redisPort,
	}
	rc, err := sgredis.NewClient(redisCfg)
	if err != nil {
		log.Printf("[sketchgate] Redis unavailable: %v (running in-memory mode)", err)
	} else {
		redisClient = rc
		defer redisClient.Close()
	}

	// Create service with default config
	cfg := service.DefaultConfig()
	srv := service.NewServer(cfg, redisClient)

	// Start gRPC server
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterRateLimiterServer(grpcServer, srv)
	reflection.Register(grpcServer) // enable grpcurl/grpcui

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("[sketchgate] shutting down...")
		grpcServer.GracefulStop()
	}()

	log.Printf("[sketchgate] gRPC server listening on :%d", *port)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("gRPC serve failed: %v", err)
	}
}
