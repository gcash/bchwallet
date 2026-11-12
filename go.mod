module github.com/gcash/bchwallet

go 1.26.0

require (
	github.com/btcsuite/golangcrypto v0.0.0-20150304025918-53f62d9b43e8
	github.com/btcsuite/websocket v0.0.0-20150119174127-31079b680792
	github.com/davecgh/go-spew v1.1.1
	github.com/gcash/bchd v0.22.5-0.20261109025102-38d11412c3d2
	github.com/gcash/bchlog v0.0.0-20261027020323-7b5704c0a60f
	github.com/gcash/bchutil v0.0.0-20261028060706-e320ade87427
	github.com/gcash/bchwallet/walletdb v1.0.0
	github.com/gcash/neutrino v0.0.0-20261109031510-ebe693fb45bd
	github.com/golang/protobuf v1.5.4
	github.com/jarcoal/httpmock v1.0.8
	github.com/jessevdk/go-flags v1.6.1
	github.com/jrick/logrotate v1.1.2
	github.com/lightninglabs/gozmq v0.0.0-20191113021534-d20a764486bf
	github.com/miekg/dns v1.1.73
	github.com/tyler-smith/go-bip39 v1.1.0
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.62.0
	golang.org/x/term v0.47.0
	golang.org/x/text v0.43.0
	google.golang.org/grpc v1.85.0
)

require (
	github.com/aead/siphash v1.0.1 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bluele/gcache v0.0.2 // indirect
	github.com/btcsuite/go-socks v0.0.0-20170105172521-4720035b7bfd // indirect
	github.com/btcsuite/winsvc v1.0.0 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/dchest/siphash v1.2.3 // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1 // indirect
	github.com/desertbit/timer v1.0.1 // indirect
	github.com/gcash/goleveldb v1.1.0 // indirect
	github.com/gcash/goslp v1.0.0 // indirect
	github.com/golang/snappy v1.0.0 // indirect
	github.com/gorilla/mux v1.8.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.33.0 // indirect
	github.com/kkdai/bstream v1.0.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_golang v1.25.0 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.1 // indirect
	github.com/prometheus/procfs v0.23.0 // indirect
	github.com/rs/cors v1.11.1 // indirect
	github.com/traefik/grpc-web v0.18.0 // indirect
	github.com/zquestz/grab v0.0.0-20190224022517-abcee96e61b1 // indirect
	go.etcd.io/bbolt v1.5.0 // indirect
	golang.org/x/mobile v0.0.0-20261011034528-197770444867 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261026165753-2ffab32bce7e // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261026165753-2ffab32bce7e // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	lukechampine.com/uint128 v1.3.0 // indirect
)

tool (
	github.com/gcash/bchd
	golang.org/x/mobile/cmd/gobind
)

replace github.com/gcash/bchd => github.com/CyberAshven/bchd v0.22.1-0.20261112073812-1286d9919d34

replace github.com/gcash/bchutil => github.com/CyberAshven/bchutil v0.0.0-20261112073614-58259c11508c

replace github.com/gcash/neutrino => github.com/CyberAshven/neutrino v0.0.0-20261112170934-b856e9acdc43
