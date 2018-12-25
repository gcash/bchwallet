bchwallet
=========
[![Build Status](https://travis-ci.org/gcash/bchwallet.png?branch=master)](https://travis-ci.org/gcash/bchwallet)
[![Go Report Card](https://goreportcard.com/badge/github.com/gcash/bchwallet)](https://goreportcard.com/report/github.com/gcash/bchwallet)
[![ISC License](http://img.shields.io/badge/license-ISC-blue.svg)](http://copyfree.org)
[![GoDoc](https://img.shields.io/badge/godoc-reference-blue.svg)](http://godoc.org/github.com/gcash/bchwallet)

bchwallet is a daemon handling bitcoin cash wallet functionality for a
single user.  It acts as both an RPC client to bchd and an RPC server
for wallet clients and legacy RPC applications.

Public and private keys are derived using the hierarchical
deterministic format described by
[BIP0032](https://github.com/bitcoin/bips/blob/master/bip-0032.mediawiki).
Unencrypted private keys are not supported and are never written to
disk.  bchwallet uses the
`m/44'/<coin type>'/<account>'/<branch>/<address index>`
HD path for all derived addresses, as described by
[BIP0044](https://github.com/bitcoin/bips/blob/master/bip-0044.mediawiki).
The default general derivation path for a fresh wallet is `m/44'/145'/0'`.

Due to the sensitive nature of public data in a BIP0032 wallet,
bchwallet provides the option of encrypting not just private keys, but
public data as well.  This is intended to thwart privacy risks where a
wallet file is compromised without exposing all current and future
addresses (public keys) managed by the wallet. While access to this
information would not allow an attacker to spend or steal coins, it
does mean they could track all transactions involving your addresses
and therefore know your exact balance.  In a future release, public data
encryption will extend to transactions as well.

bchwallet is not an SPV client and requires connecting to a local or
remote bchd instance for asynchronous blockchain queries and
notifications over websockets.  Full bchd installation instructions
can be found [here](https://github.com/gcash/bchd).  An alternative
SPV mode that is compatible with bchd and Bitcoin Core is planned for
a future release.

Wallet clients can use one of two RPC servers:

  1. A legacy JSON-RPC server mostly compatible with Bitcoin Core

     The JSON-RPC server exists to ease the migration of wallet applications
     from Core, but complete compatibility is not guaranteed.  Some portions of
     the API (and especially accounts) have to work differently due to other
     design decisions (mostly due to BIP0044).  However, if you find a
     compatibility issue and feel that it could be reasonably supported, please
     report an issue.  This server is enabled by default.

  2. An experimental gRPC server

     The gRPC server uses a new API built for bchwallet, but the API is not
     stabilized and the server is feature gated behind a config option
     (`--experimentalrpclisten`).  If you don't mind applications breaking due
     to API changes, don't want to deal with issues of the legacy API, or need
     notifications for changes to the wallet, this is the RPC server to use.
     The gRPC server is documented [here](./rpc/documentation/README.md).

## Installation and updating

### Windows - MSIs Available

Install the latest MSIs available here:

https://github.com/gcash/bchd/releases

https://github.com/gcash/bchwallet/releases

### Windows/Linux/BSD/POSIX - Build from source

Building or updating from source requires the following build dependencies:

- **Go 1.5 or 1.6**

  Installation instructions can be found here: http://golang.org/doc/install.
  It is recommended to add `$GOPATH/bin` to your `PATH` at this point.

  **Note:** If you are using Go 1.5, you must manually enable the vendor
    experiment by setting the `GO15VENDOREXPERIMENT` environment variable to
    `1`.  This step is not required for Go 1.6.

- **Dep**

  Dep is used to manage project dependencies.
  To install:

  `$ curl https://raw.githubusercontent.com/golang/dep/master/install.sh | sh`

**Getting the source**:

```
go get github.com/gcash/bchwallet
```

**Building/Installing**:

The `go` tool is used to build or install (to `GOPATH`) the project.  Some
example build instructions are provided below (all must run from the `bchwallet`
project directory).

To build and install `bchwallet` and all helper commands (in the `cmd`
directory) to `$GOPATH/bin/`, as well as installing all compiled packages to
`$GOPATH/pkg/` (**use this if you are unsure which command to run**):

```
go install . ./cmd/...
```

To build a `bchwallet` executable and install it to `$GOPATH/bin/`:

```
go install
```

To build a `bchwallet` executable and place it in the current directory:

```
go build
```

## Getting Started

The following instructions detail how to get started with bchwallet connecting
to a localhost bchd.  Commands should be run in `cmd.exe` or PowerShell on
Windows, or any terminal emulator on *nix.

- Run the following command to start bchd:

```
bchd -u rpcuser -P rpcpass
```

- Run the following command to create a wallet:

```
bchwallet -u rpcuser -P rpcpass --create
```

- Run the following command to start bchwallet:

```
bchwallet -u rpcuser -P rpcpass
```

If everything appears to be working, it is recommended at this point to
copy the sample bchd and bchwallet configurations and update with your
RPC username and password.

PowerShell (Installed from MSI):
```
PS> cp "$env:ProgramFiles\Gcash\Bchd\sample-bchd.conf" $env:LOCALAPPDATA\Btcd\bchd.conf
PS> cp "$env:ProgramFiles\Gcash\Bchwallet\sample-bchwallet.conf" $env:LOCALAPPDATA\Bchwallet\bchwallet.conf
PS> $editor $env:LOCALAPPDATA\Bchd\bchd.conf
PS> $editor $env:LOCALAPPDATA\Bchwallet\bchwallet.conf
```

PowerShell (Installed from source):
```
PS> cp $env:GOPATH\src\github.com\gcash\bchd\sample-bchd.conf $env:LOCALAPPDATA\Bchd\bchd.conf
PS> cp $env:GOPATH\src\github.com\gcash\bchwallet\sample-bchwallet.conf $env:LOCALAPPDATA\Bchwallet\bchwallet.conf
PS> $editor $env:LOCALAPPDATA\Bchd\bchd.conf
PS> $editor $env:LOCALAPPDATA\Bchwallet\bchwallet.conf
```

Linux/BSD/POSIX (Installed from source):
```bash
$ cp $GOPATH/src/github.com/gcash/bchd/sample-bchd.conf ~/.bchd/bchd.conf
$ cp $GOPATH/src/github.com/gcash/bchwallet/sample-bchwallet.conf ~/.bchwallet/bchwallet.conf
$ $EDITOR ~/.bchd/bchd.conf
$ $EDITOR ~/.bchwallet/bchwallet.conf
```

## Issue Tracker

The [integrated github issue tracker](https://github.com/gcash/bchwallet/issues)
is used for this project.

## Security Disclosures

To report security issues please contact:

Chris Pacia (ctpacia@gmail.com) - GPG Fingerprint: 0150 2502 DD3A 928D CE52 8CB9 B895 6DBF EE7C 105C

or

Josh Ellithorpe (quest@mac.com) - GPG Fingerprint: B6DE 3514 E07E 30BB 5F40  8D74 E49B 7E00 0022 8DDD 

## License

bchwallet is licensed under the liberal ISC License.
