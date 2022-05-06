module github.com/gcash/bchwallet

go 1.16

require (
	github.com/btcsuite/golangcrypto v0.0.0-20150304025918-53f62d9b43e8
	github.com/btcsuite/websocket v0.0.0-20150119174127-31079b680792
	github.com/cenkalti/backoff/v4 v4.1.3 // indirect
	github.com/davecgh/go-spew v1.1.1
	github.com/dchest/siphash v1.2.3 // indirect
	github.com/gcash/bchd v0.19.0
	github.com/gcash/bchlog v0.0.0-20180913005452-b4f036f92fa6
	github.com/gcash/bchutil v0.0.0-20210113190856-6ea28dff4000
	github.com/gcash/bchwallet/walletdb v0.0.0-20210524114850-4837f9798568
	github.com/gcash/neutrino v0.0.0-20210524114821-3b1878290cf9
	github.com/golang/protobuf v1.5.2
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.10.0 // indirect
	github.com/improbable-eng/grpc-web v0.15.0 // indirect
	github.com/jarcoal/httpmock v1.0.8
	github.com/jessevdk/go-flags v1.5.0
	github.com/jrick/logrotate v1.0.0
	github.com/klauspost/compress v1.15.2 // indirect
	github.com/lightninglabs/gozmq v0.0.0-20191113021534-d20a764486bf
	github.com/miekg/dns v1.1.48
	github.com/prometheus/common v0.34.0 // indirect
	github.com/rs/cors v1.8.2 // indirect
	github.com/simpleledgerinc/goslp v0.0.0-20210423125905-3c2e5f2ef33f // indirect
	github.com/tyler-smith/go-bip39 v1.1.0
	go.etcd.io/bbolt v1.3.6 // indirect
	golang.org/x/crypto v0.0.0-20220427172511-eb4f295cb31f
	golang.org/x/net v0.0.0-20220425223048-2871e0cb64e4
	golang.org/x/sys v0.0.0-20220503163025-988cb79eb6c6 // indirect
	golang.org/x/term v0.0.0-20220411215600-e5f449aeb171 // indirect
	golang.org/x/tools v0.1.10 // indirect
	golang.org/x/xerrors v0.0.0-20220411194840-2f41105eb62f // indirect
	google.golang.org/genproto v0.0.0-20220505152158-f39f71e6c8f3 // indirect
	google.golang.org/grpc v1.46.0
	nhooyr.io/websocket v1.8.7 // indirect
)

replace github.com/gcash/bchwallet/walletdb => ./walletdb
