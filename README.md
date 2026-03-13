# SketchGate

A high-availability distributed rate limiter with penalty queues, built in Go.

## Overview
SketchGate protects SaaS APIs from DDoS attacks and noisy neighbors using advanced algorithms:
- Sliding Window Log (O(1) time, minimal space)
- Hierarchical Token Bucket (tiered burst management)
- Count-Min Sketch (sub-linear space frequency estimation)

## Architecture
- Go service with gRPC API
- Redis for distributed state and atomic operations (Lua scripting)
- Modular algorithm implementations

## Academic Analysis
### Big-O Comparison
- Count-Min Sketch: $O(\log N)$ space for $N$ users
- Hash Map: $O(N)$ space for $N$ users

### Why Count-Min Sketch?
Provides frequency estimates with error bound $\epsilon$ and probability $1-\delta$ using multiple hash functions.

## Project Structure
- service/: API logic (gRPC)
- algorithms/: Core DAA implementations
- redis/: Redis integration and Lua scripts
- ui/: Web dashboard for visualization
- clients/: Example scripts for testing

## Portfolio Framing
Sentinel-Stream: A distributed rate-limiting engine utilizing Count-Min Sketches for sub-linear space frequency estimation. Built to handle $100k+$ RPS with $O(1)$ amortized lookup time, ensuring system stability via a Hierarchical Token Bucket scheduling algorithm.

---

## Getting Started
### Local Docker Setup
1. Install Docker and Docker Compose
2. Clone this repo
3. Run `docker-compose up --build` from the project root
4. Access the web dashboard at http://localhost:8080
5. Redis will be available at localhost:6379
6. Use example client scripts in `/clients` to test API endpoints

### Manual Setup (for development)
1. Install Go and Redis
2. Clone this repo
3. Run `go mod init` and install dependencies
4. Start the service
5. Access the web dashboard at `/ui` for visualization
6. Use example client scripts in `/clients` to test API endpoints

## License
MIT
