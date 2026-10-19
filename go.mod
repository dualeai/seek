module github.com/dualeai/seek

go 1.27.0

toolchain go1.27.2

require (
	github.com/cespare/xxhash/v2 v2.3.0
	github.com/daulet/tokenizers v1.27.0
	github.com/grafana/regexp v0.0.0-20250905093917-f7b3be9d1853
	github.com/klauspost/compress v1.20.3
	github.com/sourcegraph/go-ctags v0.0.0-20260626114452-5ec1554485f9
	github.com/sourcegraph/zoekt v0.0.0-20261016105519-0e68f41329d9
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/yalue/onnxruntime_go v1.36.0
	go.uber.org/goleak v1.3.0
	golang.org/x/sync v0.24.0
	golang.org/x/sys v0.49.0
	golang.org/x/term v0.47.0
)

require (
	github.com/RoaringBitmap/roaring/v2 v2.29.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bits-and-blooms/bitset v1.26.0 // indirect
	github.com/bmatcuk/doublestar/v4 v4.10.3 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/go-enry/go-enry/v2 v2.9.6 // indirect
	github.com/go-enry/go-oniguruma v1.2.1 // indirect
	// Hold at v0.2.3: v1.0.0 removes glob.Glob, which zoekt/ignore uses.
	// After `make upgrade`, run `go get github.com/gobwas/glob@v0.2.3`.
	// Remove this hold when zoekt requires gobwas/glob v1.
	github.com/gobwas/glob v0.2.3 // indirect
	github.com/grpc-ecosystem/go-grpc-middleware/v2 v2.3.4 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mschoch/smat v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opentracing/opentracing-go v1.2.0 // indirect
	github.com/prometheus/client_golang v1.25.0 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.1 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/wasilibs/go-re2 v1.13.0 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261012220534-4341edf3f941 // indirect
	google.golang.org/grpc v1.84.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
