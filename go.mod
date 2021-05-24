module github.com/gcash/bchwallet

go 1.16

require (
	github.com/btcsuite/golangcrypto v0.0.0-20150304025918-53f62d9b43e8
	github.com/btcsuite/websocket v0.0.0-20150119174127-31079b680792
	github.com/davecgh/go-spew v1.1.1
	github.com/gcash/bchd v0.18.1
	github.com/gcash/bchlog v0.0.0-20180913005452-b4f036f92fa6
	github.com/gcash/bchutil v0.0.0-20210113190856-6ea28dff4000
	github.com/gcash/bchwallet/walletdb v0.0.0-20210524044131-61bcca2ae6f9
	github.com/gcash/neutrino v0.0.0-20210524105223-4cec86bbd8a4
	github.com/golang/protobuf v1.5.2
	github.com/improbable-eng/grpc-web v0.14.0 // indirect
	github.com/jarcoal/httpmock v1.0.8
	github.com/jessevdk/go-flags v1.5.0
	github.com/jrick/logrotate v1.0.0
	github.com/klauspost/compress v1.12.2 // indirect
	github.com/lightninglabs/gozmq v0.0.0-20191113021534-d20a764486bf
	github.com/miekg/dns v1.1.42
	github.com/simpleledgerinc/goslp v0.0.0-20210423125905-3c2e5f2ef33f // indirect
	github.com/tyler-smith/go-bip39 v1.1.0
	golang.org/x/crypto v0.0.0-20210513164829-c07d793c2f9a
	golang.org/x/net v0.0.0-20210521195947-fe42d452be8f
	google.golang.org/grpc v1.38.0
	nhooyr.io/websocket v1.8.7 // indirect
)

replace github.com/gcash/bchwallet/walletdb => ./walletdb
