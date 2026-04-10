module sketchgate-ui

go 1.24.0

require (
	github.com/kranthi0003/SketchGate v0.0.0
	github.com/redis/go-redis/v9 v9.18.0
	google.golang.org/grpc v1.80.0
)

replace github.com/kranthi0003/SketchGate => ../

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/net v0.49.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/text v0.33.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260120221211-b8f7ae30c516 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
