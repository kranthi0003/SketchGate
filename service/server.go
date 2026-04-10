package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/kranthi0003/SketchGate/algorithms"
	pb "github.com/kranthi0003/SketchGate/proto"
	sgredis "github.com/kranthi0003/SketchGate/redis"
)

// Config holds the service configuration.
type Config struct {
	// Rate limiting
	WindowDuration time.Duration
	RequestLimit   int64

	// Count-Min Sketch
	CMSWidth uint
	CMSDepth uint

	// Token bucket tiers
	Tiers []TierDef

	// Penalty threshold: violations before penalizing
	PenaltyThreshold int64
}

// TierDef defines a token bucket tier.
type TierDef struct {
	Name  string
	Rate  float64
	Burst uint64
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		WindowDuration:   time.Minute,
		RequestLimit:     100,
		CMSWidth:         2718, // e/0.001
		CMSDepth:         5,    // ln(1/0.01)
		PenaltyThreshold: 3,
		Tiers: []TierDef{
			{Name: "per-second", Rate: 20, Burst: 20},
			{Name: "per-minute", Rate: 100, Burst: 100},
		},
	}
}

// Server implements the gRPC RateLimiter service.
type Server struct {
	pb.UnimplementedRateLimiterServer

	cfg       Config
	startTime time.Time

	// In-memory algorithms
	tokenBuckets *algorithms.HierarchicalTokenBucket
	cms          *algorithms.CountMinSketch

	// Redis-backed stores (nil if Redis unavailable)
	redisClient  *sgredis.Client
	swLimiter    *sgredis.SlidingWindowLimiter
	cmsStore     *sgredis.CountMinSketchStore
	penaltyQueue *sgredis.PenaltyQueue
}

// NewServer creates a new SketchGate gRPC server.
func NewServer(cfg Config, redisClient *sgredis.Client) *Server {
	// Initialize in-memory token buckets
	htb := algorithms.NewHierarchicalTokenBucket()
	for _, tier := range cfg.Tiers {
		htb.AddTier(tier.Name, tier.Rate, tier.Burst)
	}

	// Initialize in-memory CMS
	cms := algorithms.NewCountMinSketchWithSize(cfg.CMSWidth, cfg.CMSDepth)

	s := &Server{
		cfg:          cfg,
		startTime:    time.Now(),
		tokenBuckets: htb,
		cms:          cms,
	}

	// Wire up Redis if available
	if redisClient != nil {
		s.redisClient = redisClient
		s.swLimiter = sgredis.NewSlidingWindowLimiter(redisClient, "")
		s.cmsStore = sgredis.NewCountMinSketchStore(redisClient, "", cfg.CMSWidth, cfg.CMSDepth)
		s.penaltyQueue = sgredis.NewPenaltyQueue(redisClient, "")
		log.Println("[sketchgate] Redis connected — using distributed mode")
	} else {
		log.Println("[sketchgate] No Redis — using in-memory mode")
	}

	return s
}

// CheckRate implements the CheckRate RPC.
func (s *Server) CheckRate(ctx context.Context, req *pb.CheckRateRequest) (*pb.CheckRateResponse, error) {
	key := req.Key
	tokens := req.Tokens
	if tokens <= 0 {
		tokens = 1
	}

	resp := &pb.CheckRateResponse{Allowed: true}

	// 1. Check penalty queue first
	if s.penaltyQueue != nil {
		penalized, score, err := s.penaltyQueue.IsPenalized(ctx, key)
		if err != nil {
			log.Printf("[sketchgate] penalty check error: %v", err)
		}
		if penalized {
			resp.Penalized = true
			resp.PenaltyScore = score
			resp.Allowed = false
			resp.Remaining = 0
			return resp, nil
		}
	}

	// 2. Check token buckets (in-memory, per-instance)
	tbAllowed, tierRemaining := s.tokenBuckets.AllowN(key, uint64(tokens))
	if !tbAllowed {
		resp.Allowed = false
		s.recordViolation(ctx, key)
	}
	_ = tierRemaining

	// 3. Check sliding window (Redis if available, otherwise skip)
	if s.swLimiter != nil {
		result, err := s.swLimiter.Allow(ctx, key, s.cfg.WindowDuration, s.cfg.RequestLimit)
		if err != nil {
			log.Printf("[sketchgate] sliding window error: %v", err)
		} else {
			resp.Remaining = result.Remaining
			resp.ResetAtMs = result.ResetAt.UnixMilli()
			if !result.Allowed {
				resp.Allowed = false
				s.recordViolation(ctx, key)
			}
		}
	}

	// 4. Track frequency in CMS
	s.cms.Increment(key, uint64(tokens))
	if s.cmsStore != nil {
		if err := s.cmsStore.Increment(ctx, key, uint64(tokens)); err != nil {
			log.Printf("[sketchgate] cms increment error: %v", err)
		}
	}

	return resp, nil
}

// GetStatus implements the GetStatus RPC.
func (s *Server) GetStatus(ctx context.Context, req *pb.GetStatusRequest) (*pb.GetStatusResponse, error) {
	key := req.Key
	resp := &pb.GetStatusResponse{
		Limit: s.cfg.RequestLimit,
	}

	// Frequency estimate
	resp.FrequencyEstimate = s.cms.Estimate(key)

	// Redis-based status
	if s.swLimiter != nil {
		count, err := s.swLimiter.Count(ctx, key, s.cfg.WindowDuration)
		if err == nil {
			resp.RequestCount = count
			resp.Remaining = s.cfg.RequestLimit - count
		}
	}

	// Penalty status
	if s.penaltyQueue != nil {
		penalized, score, err := s.penaltyQueue.IsPenalized(ctx, key)
		if err == nil {
			resp.Penalized = penalized
			resp.PenaltyScore = score
		}
	}

	// Token bucket tiers
	resp.TierTokens = make(map[string]uint64)
	_, tierRemaining := s.tokenBuckets.Allow(key)
	for tier, rem := range tierRemaining {
		resp.TierTokens[tier] = rem
	}

	return resp, nil
}

// GetPenaltyQueue implements the GetPenaltyQueue RPC.
func (s *Server) GetPenaltyQueue(ctx context.Context, req *pb.GetPenaltyQueueRequest) (*pb.GetPenaltyQueueResponse, error) {
	topN := req.TopN
	if topN <= 0 {
		topN = 10
	}

	resp := &pb.GetPenaltyQueueResponse{}

	if s.penaltyQueue == nil {
		return resp, nil
	}

	entries, err := s.penaltyQueue.TopOffenders(ctx, topN)
	if err != nil {
		return nil, fmt.Errorf("failed to get penalty queue: %w", err)
	}

	for _, e := range entries {
		resp.Entries = append(resp.Entries, &pb.PenaltyEntry{
			Key:        e.Key,
			Violations: e.Violations,
		})
	}

	total, err := s.penaltyQueue.Size(ctx)
	if err == nil {
		resp.TotalPenalized = total
	}

	return resp, nil
}

// PardonUser implements the PardonUser RPC.
func (s *Server) PardonUser(ctx context.Context, req *pb.PardonUserRequest) (*pb.PardonUserResponse, error) {
	if s.penaltyQueue == nil {
		return &pb.PardonUserResponse{Success: false}, nil
	}

	err := s.penaltyQueue.Pardon(ctx, req.Key)
	return &pb.PardonUserResponse{Success: err == nil}, err
}

// EstimateFrequency implements the EstimateFrequency RPC.
func (s *Server) EstimateFrequency(ctx context.Context, req *pb.EstimateFrequencyRequest) (*pb.EstimateFrequencyResponse, error) {
	resp := &pb.EstimateFrequencyResponse{}

	// Prefer Redis store, fall back to in-memory
	if s.cmsStore != nil {
		est, err := s.cmsStore.Estimate(ctx, req.Key)
		if err == nil {
			resp.Estimate = est
		}
		total, err := s.cmsStore.Total(ctx)
		if err == nil {
			resp.Total = total
		}
	} else {
		resp.Estimate = s.cms.Estimate(req.Key)
		resp.Total = s.cms.Total()
	}

	return resp, nil
}

// Health implements the Health RPC.
func (s *Server) Health(ctx context.Context, req *pb.HealthRequest) (*pb.HealthResponse, error) {
	resp := &pb.HealthResponse{
		Healthy:       true,
		UptimeSeconds: int64(time.Since(s.startTime).Seconds()),
		RedisStatus:   "disconnected",
	}

	if s.redisClient != nil {
		err := s.redisClient.Raw().Ping(ctx).Err()
		if err == nil {
			resp.RedisStatus = "connected"
		} else {
			resp.RedisStatus = fmt.Sprintf("error: %v", err)
			resp.Healthy = false
		}
	}

	return resp, nil
}

// recordViolation increments the penalty score for a key.
func (s *Server) recordViolation(ctx context.Context, key string) {
	if s.penaltyQueue == nil {
		return
	}
	score, err := s.penaltyQueue.Penalize(ctx, key, 1)
	if err != nil {
		log.Printf("[sketchgate] penalty record error: %v", err)
		return
	}
	if int64(score) >= s.cfg.PenaltyThreshold {
		log.Printf("[sketchgate] key %q penalized (score=%.0f)", key, score)
	}
}
