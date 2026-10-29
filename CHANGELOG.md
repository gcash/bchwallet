# bchwallet changelog

All notable user-visible changes to [bchwallet](https://github.com/gcash/bchwallet), a Bitcoin Cash wallet daemon written in Go, are listed here, newest first. Each entry summarizes the wallet, SPV, RPC/gRPC, network upgrade, and packaging changes that landed in a release.

bchwallet forked from [btcwallet](https://github.com/btcsuite/btcwallet) in October 2018, at upstream commit 29ee944 ([btcsuite/btcwallet#551](https://github.com/btcsuite/btcwallet/pull/551)). The original btcwallet changelog (0.1.0 through 0.7.0) is preserved unchanged at the bottom of this file under **Legacy btcwallet changelog (pre-fork)**.

## Unreleased

This release keeps the wallet from spending an output again that a transaction in the same block already spent, repairs wallets that did, keeps other people's transactions out of the wallet, and checks BIP70 payment requests that a client has as a file.

### Wallet

- A transaction that spends an output of another transaction in the same block marks that output spent, also when the block lists the spending transaction first. Since the canonical transaction ordering upgrade, blocks list their transactions by txid, so this happens to about half of such pairs. The wallet kept the output unspent: it counted it in the balance and could spend it again, in a transaction that never confirmed and that the wallet broadcast again after every rescan.
- Opening a wallet marks spent the outputs it kept unspent that way, and removes the unconfirmed transactions that spent them again, along with the transactions that spend their outputs, so the balance shows what the wallet holds and those transactions are no longer broadcast. This upgrades the transaction store to version 3, which older versions of bchwallet can't open.
- With a bchd backend too, a transaction that spends an output of another transaction in the same block marks that output spent when the block lists the spending transaction first. bchd notifies a block's transactions one at a time, and the wallet recorded each as it came, so it kept the output unspent. The RPC client now hands over a block's transactions together, as the SPV client does.
- The wallet records only the transactions that pay it, spend its outputs or that it published. Once it publishes a transaction, the backend also watches the addresses the transaction pays, until the wallet restarts, so it reported the recipients' later transactions too, such as their spend of what the wallet paid them, and the wallet listed those among its own, with no debits or credits.

### RPC & gRPC

- `ParsePaymentRequest` checks a BIP70 payment request that a client has as a file, as a merchant can hand one over with the `application/bitcoincash-paymentrequest` type, as `DownloadPaymentRequest` checks one that it downloads, and returns the same response. The gRPC API version is 2.2.0.
- A BIP70 payment request larger than 50,000 bytes, the limit that BIP70 sets, is refused. `DownloadPaymentRequest` read all of one, however large.
- An unsigned BIP70 payment request, which BIP70 has wallets accept, is accepted with an empty `pay_to_name`, as a request that names no merchant and is no more trustworthy than a plain address. One signed with `x509+sha1` is still refused, since SHA-1 signatures can be forged.

## 0.13.2 (2026-10-27)

This release moves to bchd 0.22.4, lets an app run the wallet again in the same process and start it when targeting current Android, lets the wallet hold and send CashTokens and shows them in the gRPC API, pays BitPay invoices with the JSON Payment Protocol, makes SPV wallets reconnect to their peer after a restart or a network change, and adds testnet4 and chipnet.

### Wallet

- Outputs that carry CashTokens are only spent to send their tokens. They are left out of coin selection, `SweepAccount` and the spendable balance, so that paying BCH doesn't burn their tokens, and still count toward the total balance. The wallet spent them like any other output, and the network rejected each transaction that did, so `SweepAccount` failed for as long as the wallet held one.
- The wallet's signatures cover the CashTokens of the outputs they spend, as the token rules require, and the transactions it creates, signs and publishes are checked with the token rules. `SignTransaction` takes the outputs a transaction spends from the wallet's records when it has them, since the caller's values and scripts can't carry tokens. It signed without the tokens, and checked its signatures without the token rules, so it passed invalid ones.
- `SignTransaction` refuses to sign a transaction that would burn tokens of the wallet's outputs it spends, unless burns are allowed: fungible tokens of a category that its outputs carry fewer of, an immutable NFT that none of them carries with its commitment, a mutable NFT that leaves no NFT of its category, or the capability of a minting NFT. NFTs are told apart by their commitments, so another input's NFT, or one that a minting NFT mints, doesn't stand in for one of the wallet's. The legacy `signrawtransaction`, which can't allow burns, always refuses.
- Signing an input that already carries signatures keeps a P2PKH or P2PK signature only if it is valid, and multisig signatures only if the network accepts their hash type, and signing a P2SH input again no longer makes an invalid script (bchd 0.22.4).
- An unmined transaction is removed only when the network rejects it: an SPV peer's rejection, or a node's answer that rejects it, can't decode it, or finds its inputs missing or spent. A broadcast that failed for any other reason, such as a backend that is stopping, can't be reached or is warming up, removed a valid transaction, so the wallet could spend its inputs again in another. The wallet now keeps such a transaction and broadcasts it again after the next block or reconnection, and publishing it succeeds, also when the backend can't watch the transaction's outputs, since a caller that created it again would pay twice.
- Stopping the wallet no longer hangs when a relevant transaction arrives, or one is published, just as it stops. Passing the transaction on to the recovery, whose handler had already returned, blocked forever, and the shutdown waited for it: the daemon did not exit, and in an app `StopWallet` gave up after 30 seconds, and the wallet could not be opened again until the app restarted.
- Once the wallet has stopped, the methods that go through its own goroutines, such as `Unlock`, the passphrase changes and `CreateSimpleTx`, return `ErrWalletShuttingDown`, and `Locked` reports the wallet locked. They blocked forever, so a gRPC call such as `SignTransaction` after `CloseWallet` never returned.
- With a bchd chain backend, each start of the backend makes one connection attempt, rather than trying until bchd answers, so that the wallet shuts down while bchd can't be reached. A client that connected but failed to start, as when bchd runs another network, is stopped. It reconnected in the background, and the wallet leaked one on every try.
- The chain backend no longer starts a wallet again after the shutdown unloaded it. When the backend's client dropped just as the wallet shut down, the backend restarted the wallet's goroutines on its closed database, where they stayed.

### Mobile

- `StopWallet` no longer exits the process 3 seconds after asking the wallet to stop. It returns once the wallet has shut down and closed its databases, after at most 30 seconds, also when it comes while the wallet is starting, and `StartWallet` can then run the wallet again in the same process. On Android the exit killed the app, which showed as a crash without any log when the app was reopened quickly ([gcash/android-neutrino#14](https://github.com/gcash/android-neutrino/issues/14)).
- `StartWallet` does nothing while the wallet runs and waits for a shutdown in progress before starting, unless `StopWallet` is called meanwhile, since stopping the wallet was then asked for last. The new `IsRunning` and `LastError` report whether the wallet runs and why it last failed to start or stopped, such as an invalid config file or an RPC port that is in use.
- The wallet starts in apps targeting Android API 30 or newer. The RPC TLS key pair is only loaded or generated when a server uses TLS, since generating it reads the network interface addresses, which Android denies to such apps, so with `--noservertls` the wallet failed to start with "route ip+net: netlinkrib: permission denied".
- With TLS on, the wallet starts in such apps too: its RPC TLS key pair is generated without the network interface addresses they can't read (latest bchutil).
- The wallet is configured only by its config file and leaves the app's signals alone: the command line of the app's process is no longer parsed.
- A wallet that can't sync with its chain backend, because checking its birthday block or syncing to the chain fails, stops with that error, which `LastError` reports, also one that a client closed and opened again over gRPC. It panicked, which killed the app.
- Each run of the wallet chooses its network from its own config, so a process that runs the wallet again with a config for another network, or for mainnet, no longer keeps the network of the earlier run.
- The wallet no longer exits the app's process. A log directory it can't create, a misused `createtemp`, `debuglevel=show` or a finished `--create` end the run instead, with an error for the first two, which `LastError` reports.
- The wallet doesn't prompt on the app's console. Opening a wallet whose upgrade needs its seed or private passphrase, or creating one with `--create`, fails with an error that says so, where a prompt failed on the app's empty input with a confusing error, or waited for input that would never come.

### SPV

- When the wallet shuts down, the neutrino ChainService is stopped and its database closed. They kept running until the process exited.
- The block and filter header files are closed when the wallet stops, so each run of the wallet in the same process no longer leaves two file handles open, and a goroutine leak in the SPV backend's peers is fixed (latest neutrino).
- The chain backend waits 5 seconds before trying again after it failed to start, instead of retrying right away, and no longer starts again while the wallet shuts down.
- A restarted wallet connects to the peer it used last instead of first waiting minutes on addresses that can't be reached, a peer that can't be created no longer panics, and picking a peer no longer takes seconds when addresses have failed many times (latest neutrino and bchd 0.22.4).
- A wallet that lost its peer to a network change connects to it again instead of waiting for peers until it's restarted, and a dial that gets no answer gives up after 30 seconds (latest neutrino and bchd 0.22.4).

### New options

- `profile` takes a host and port, `host:port`, as well as a port. A port alone now listens on localhost: the profile server shows the command line of the process, with any passwords given there, and listened on every interface. Each run of the wallet in a process has its own profile server, where the second run panicked.
- `--testnet4` and `--chipnet` run the wallet on testnet4 and on chipnet, the test network that activates upgrades early. They use the default ports of testnet3, as bchd does, and keep their wallets in the `testnet4` and `chipnet` directories of the application data directory.

### RPC & gRPC

- Each gRPC server gets services of its own, so a later run of the wallet in the same process starts with fresh ones, and stopping the server waits for its calls, for at most 10 seconds, after the shutdown released them from waiting on the wallet, the chain backend or a merchant. Starting the services a second time panicked.
- A wallet that a client closes and opens again over gRPC is the one `WalletService` serves, and while no wallet is open its calls fail with `FailedPrecondition`. The service kept serving the closed wallet.
- A legacy address for another network is refused: `ValidateAddress` reports it invalid, and `CreateTransaction`, `SweepAccount` and `PostPayment` return an error. A testnet address was accepted on mainnet, and a transaction paid mainnet coins to its hash (latest bchutil).
- `DownloadPaymentRequest` accepts a BIP70 payment request only when its certificate chains to a certificate authority the system trusts. It trusted the second certificate in the request itself, so anyone could sign a request that named any merchant.
- `DownloadPaymentRequest` also refuses a payment request for another network, and checks the request's signature with SHA-256 and the algorithm of the merchant's key, where it used the algorithm of the certificate authority's signature and refused some valid requests. A merchant whose certificate has no common name is named by its first DNS name. A BIP70 request with an output that doesn't pay one address is refused: an OP_RETURN output crashed the wallet, and the wallet gave the first key of a bare multisig output as the address to pay.
- `CreateTransaction` and `SweepAccount` return `InvalidArgument` for an address that is not for the wallet's network or is a public key, an amount below the dust limit, a fee rate below the minimum relay fee of 1000 satoshis per kB, or a fee above 0.1 BCH, the default `--maxtxfee` of bchd and BCHN. They built transactions that nodes refused, and paid whatever fee rate they were given. A fee rate of 0 now stands for the minimum relay fee.
- `SweepAccount` returns `FailedPrecondition` when the account has nothing to sweep, or too little to pay the fee and keep the output above the dust limit, where it built a transaction with a negative or dust output. `ValidateAddress` reports a public key invalid, and `PostPayment` without a refund output returns `InvalidArgument` instead of crashing the wallet.
- A gRPC call whose handler panics fails with `Internal`, and the panic is logged with its stack, where it crashed the process. A missing or wrong authentication token is refused with `Unauthenticated` instead of `Unknown`, and the token is compared in constant time.
- `Network` also returns `peer_count` and `peer_best_height`, how many peers the SPV backend is connected to and the highest block height they announced, and `chain_current`, whether the chain backend considers itself synced, so a client can tell a wallet without peers from one that is syncing, and show how far along it is. Without a chain backend it returns `FailedPrecondition` instead of `Unknown`, and so does `Rescan`, which waited for one until the wallet stopped.
- `DownloadPaymentRequest` speaks version 2 of BitPay's JSON Payment Protocol, which BitPay uses for BCH, and falls back to BIP70 for merchants without it. The merchant's responses must be signed by one of BitPay's keys, pinned as BitPay's own wallet pins them, for the merchant's domain and the wallet's network. The response gives the protocol and the lowest fee rate the merchant accepts. `PostPayment` takes the protocol back, or for a client that predates the field, uses the protocol of the request downloaded for the payment URL, and with the JSON Payment Protocol the merchant verifies the unsigned transactions before it gets the signed ones.
- `PostPayment` records the payment's transactions in the wallet once the merchant accepts them, which also broadcasts them. The wallet treated their inputs as unspent until it saw them on the network, and could spend them again.
- `TokenBalances` returns the CashTokens in an account's unspent outputs by category: their fungible amount and their NFTs. `TransactionDetails` gives the tokens of the wallet's outputs and of the outputs its inputs spend, `NextAddress` and `CurrentAddress` return an address in its token-aware form on request, and `ValidateAddress` tells whether an address is in that form.
- `CreateTransaction` sends CashTokens: an output can send a fungible amount of a category and one of the account's NFTs to a token-aware address, with 0 satoshis standing for its dust limit. The wallet spends the outputs that hold the tokens, also those that hold fungible tokens with an NFT, which it returns as it is, and returns the tokens left over. `SignTransaction` takes `allow_token_burn`, and without it refuses a transaction that would burn tokens with `FailedPrecondition`.
- The legacy `getunconfirmedbalance` counts only the outputs without confirmations that the wallet would spend, and no longer immature coinbase rewards, or outputs with tokens, which aren't spendable. The legacy `listunspent` lists outputs with tokens, which it can't show the tokens of, as not spendable, so that `sweepaccount` and other clients don't spend them for their value; `signrawtransaction` refuses to.
- `TransactionDetails` lists the outputs that don't pay the wallet in `other_outputs`, with the address each pays, so a client can show whom a transaction paid without decoding it. An output that pays no single address by its hash, such as OP_RETURN data, has no address.
- The gRPC API version is 2.1.0.

### Build & Packaging

- Update to the bchd 0.22.4 libraries, whose signing functions cover the CashTokens of the outputs that inputs spend, and to the latest neutrino, bchutil and grpc.
- Track `golang.org/x/mobile/cmd/gobind` as a go tool, which `gomobile bind` requires to bind the mobile package from the module.
- The mobile README covers NDK r28 or newer, whose 64-bit libraries are aligned for the 16 KB pages Google Play requires, and how to check the alignment.

### Upgrade notes

- The mobile package's `StopWallet` blocks until the wallet has shut down, so call it off an app's UI thread.
- Programs that embed the wallet can use `boot.WalletMainContext`, which runs the wallet until its context is done, and set `boot.HandleSignals` and `boot.Args` to keep their own signal handling and command line, and `boot.ConsolePrompts` to keep the wallet from prompting on the console. The daemon exits with status 1 for a misused `createtemp`, which exited with status 0.
- In `rpcserver`, each server that `RegisterServices` registers services with has its own. `StartWalletService` and the new `StopWalletService` start and stop its wallet service, also for a wallet loaded again, without panicking. `StopServices(server)` drops the services and stops the consensus RPC client of `StartConsensusRPC`. Interceptors check that a service is ready with `ServiceReadyFor`, given the service implementation of the call; `ServiceReady` is deprecated.
- `wallet.Loader` has `OnLoad` and `OnUnload`, which run a function for every wallet the loader loads and before every unload, unlike the one-shot `RunAfterLoad`, and `RestartWallet`, which stops and starts the loaded wallet while it holds the loader, so that the wallet can't be unloaded in between.
- `wtxmgr.Credit` has a `TokenData` field and a `HasTokens` method, and `wallet.OutputSelectionPolicy` an `IncludeTokens` field that selects outputs carrying tokens too.
- `wallet.Wallet.CreateUnsignedTx` takes the NFT each output sends, and its errors about tokens wrap `ErrTokenSend`. `SignTransaction` takes `allowTokenBurn`, and its error for a burn wraps `ErrTokenBurn`. `TokenBalances` returns an account's tokens, `TransactionSummaryInput` and `TransactionSummaryOutput` have the tokens of their outputs, `TransactionSummary` has `OtherOutputs`, and `Balances` has `Unconfirmed`. The wallet's methods that go through its goroutines return `ErrWalletShuttingDown` once it has stopped. In `txauthor`, `AuthoredTx` has `PrevTokens` and `AddAllInputScriptsWithTokens` signs inputs that spend tokens, and in `txrules`, `OutputDustThreshold` counts an output's tokens.
- gRPC clients of `CreateTransaction` and `SweepAccount` must send a fee rate of at least 1000 satoshis per kB, or 0 for that minimum, and amounts of at least the dust limit.
- `wallet.Wallet.SetFatalErrorHandler` sets a function the wallet calls with an error it can't go on from, instead of panicking. `boot` sets one, so `WalletMain` returns such an error after a clean shutdown, and the daemon exits with status 1.
- `rpc/api.proto` has the `TokenBalances` method and new fields, and the API version is 2.1.0. Clients built from an older copy of the file, such as the Android app's, keep working without them. `rpc/walletrpc/api.pb.go` is still generated by `rpc/regen.sh` with `github.com/golang/protobuf/protoc-gen-go` v1.3.2, whose `plugins=grpc` newer versions dropped.
- `PostPaymentRequest` has a `protocol` field, which should be the `protocol` that `DownloadPaymentRequest` returned. Without it, a payment goes with the protocol of the request the wallet downloaded for its payment URL, until the request expires, and otherwise with BIP70, as before. `refund_output` is only needed with BIP70. In `pymtproto`, `DownloadPaymentRequest` tries both protocols, `PaymentRequest` and `Payment` have a `Protocol` field, and the client's methods take a context, which ends their requests once it is done.
- The legacy `listunspent` reports outputs with CashTokens as not spendable, and `getunconfirmedbalance` no longer counts immature coinbase rewards.

## 0.13.1 (2026-10-23)

This release moves to bchd 0.22.3 and the latest neutrino, fixes a rescan bug that could stop the wallet from recording new blocks until the rescan finished, adds an option to keep downloaded filters on disk, and gets CI passing again.

### Wallet

- The address recovery scan at startup no longer announces recovery mode on every start. It logs only when the wallet has blocks to catch up on, and says which blocks it is scanning.
- `importprivkey` in SPV mode now finds transactions from before the wallet was created. Neutrino skipped the filters of blocks older than the wallet's birthday, so the import's rescan from genesis missed them (#73). The import has no start height, so in SPV mode this rescan checks the filter of every block and can take a long time.
- Creating or restoring a wallet, with `--create` or the gRPC `CreateWallet` method, checks the mnemonic seed against the BIP39 wordlist and checksum, so a mistyped or empty seed is rejected instead of silently creating a different wallet. Extra spaces between words no longer change the seed (#70).

### SPV

- Use neutrino's filtered block notifications instead of the deprecated `OnBlockConnected` and `OnBlockDisconnected` callbacks, which neutrino no longer sends.
- Rescans that start before the wallet's birthday send their progress notification at the birthday again. Since 0.13.0 it was skipped, which could stop the wallet from recording the blocks that followed until the rescan finished.
- Transaction broadcasts give slow peers, such as those on Tor, up to 5 seconds instead of half a second to ask for the transaction, and finish about a second after every peer has asked for it (latest neutrino).
- A broadcast transaction is reported as invalid once at least 60% of the peers that replied reject it as invalid, instead of only when every one of them rejects it (latest neutrino).
- Peers are only used once their version handshake completes (latest neutrino).

### New options

- `--persistfilters` saves downloaded compact filters to disk in SPV mode so they aren't fetched again after a restart. It is off by default.

### RPC & gRPC

- The `lockunspent` and `listlockunspent` help now documents the optional `sequence` field that bchd 0.22.3 added to transaction inputs. The wallet ignores it.

### Build & Packaging

- Update to the bchd 0.22.3 libraries.
- Fix the staticcheck warnings that were failing CI: the payment protocol client now dials with `DialContext`, and the bchd RPC backend's legacy rescan notifications are marked as intentionally kept.
- Run gofmt across the tree.
- Tag the `walletdb` module as `walletdb/v1.0.0`, so projects that use it can depend on a version instead of a pseudo-version, and update it to bbolt 1.5.0 (#79).

### Upgrade notes

- SPV wallets on regtest now connect only to the peers given with `--connect` or `--addpeer`, and no longer pick up other peers from address announcements.

## 0.13.0 (2026-04-23)

### Wallet

- Fix rescans started by `importprivkey` that never finished (#73).

### Build & Packaging

- Update to the bchd 0.22.0 libraries and the latest neutrino.
- Clean up errcheck, ineffassign, staticcheck, and unused warnings.

## 0.12.0 (2025-05-13)

### Consensus & Network

- Update to the bchd 0.21 libraries, which implement the May 2025 network upgrade (VM limits and high-precision arithmetic), and the latest neutrino.

### Build & Packaging

- Use the latest golangci-lint in CI.

## 0.11.0 (2025-01-14)

This release catches the wallet up with the 2023 and 2024 network upgrades by moving to the bchd 0.20 libraries. The wallet does not support CashTokens. Release candidates 0.11.0-rc1 and 0.11.0-rc2 were published on 2024-12-20.

### Consensus & Network

- Update to the bchd 0.20.0 libraries and the latest neutrino, including the CashTokens-aware transaction format. Outputs the wallet creates carry no token data.
- Fix peering and syncing problems with BCHN nodes (rc2).

### Build & Packaging

- Document installing with `go install`, and remove the `replace` directive for `walletdb` so `go install` works.
- Remove the Travis CI configuration.

## 0.10.0 (2022-05-09)

### Consensus & Network

- Support the May 15, 2022 network upgrade by moving to the bchd 0.19 libraries, including the script engine change needed to check signed inputs (#77).

## 0.9.0 (2021-05-24)

### Consensus & Network

- Support the ASERT difficulty adjustment algorithm from the November 2020 network upgrade through the latest neutrino (#75).
- Update to the bchd 0.18.1 libraries (#74).

### Wallet

- `getnewaddress` returns addresses with their cashaddr prefix (#53).
- Signing a raw transaction requires an amount for every input and looks up missing amounts itself, since Bitcoin Cash signatures commit to input values (#66).
- Always create the network data directory (#71).
- Backports from btcwallet: batched filter fetches for the neutrino backend, waiting for the chain to sync before neutrino recovery, fixes for transactions that confirm after being broadcast, and a larger recovery batch size (#72).

### Build & Packaging

- `walletdb` is now its own Go module and uses bbolt instead of bolt (#72).
- Remove `SecretKeyGenerator` and make the test suite more reliable (#57).
- Update dependencies, including a fix for the zmq library's API change (#55, #62, #64).
- Fix the Makefile's cross-compile targets.

### Other

- `dropwtxmgr` refers to bchwallet instead of btcwallet (#56).

## 0.8.2 (2019-10-19)

This release backports a large set of fixes from btcwallet and updates the neutrino backend for the new filter format.

### Wallet

- Initial sync waits for the chain backend to be current and finds the birthday block without scanning from genesis, and recovery starts from the wallet's best height (#47).
- Keep only the block hashes needed to handle reorgs, with a migration for existing wallets (#47).
- Fix a deadlock while sending the rescan finished notification with the neutrino backend, and stop adding already confirmed transactions back as unconfirmed (#47).
- Recovery can be interrupted and sends progress notifications (#48).

### RPC & gRPC

- Fix a panic in `signrawtransaction` when the backend returns no result (#44).
- `signrawtransaction` returns an error if an input is already spent (#45).
- The gRPC network response includes the wallet's synced height (#48).

### SPV

- Update neutrino and bchutil for the new filter format (#51).

### Build & Packaging

- Switch from dep to Go modules (#46).
- Add golangci-lint and fix its warnings.
- Fix a certificate error in the payment protocol tests (#50).

## 0.8.1 (2019-05-26)

### Consensus & Network

- Sign transactions with Schnorr signatures, which the May 2019 network upgrade enabled (#42).

## 0.8.0 (2019-05-03)

This release adds a round of gRPC API features, BIP70 payment protocol support, a mobile package, and regtest support, and backports many fixes from btcwallet. The biggest change is the compact filter format used by the neutrino backend, which now includes outpoints (#39).

### SPV

- Compact filters include outpoints (#39).
- Sort relevant transactions into dependency order before adding them to the wallet (#40).
- The neutrino backend honors `--proxy` and uses its own DNS resolver (#26).

### Wallet

- Create wallets with a custom birthday, and start rescans from the gRPC API (#38).
- Track a birthday block and sanity check it before syncing, with migrations for existing wallets (#19).
- Rework the initial sync to the birthday and wallet recovery (#31).
- Remove transactions that fail to broadcast instead of keeping them as unmined (#34).
- Fix the fee calculation in the `CreateTransaction` gRPC method.

### RPC & gRPC

- New gRPC methods and fields: `CurrentAddress`, `DownloadPaymentRequest`, `Rescan`, the `RescanNotifications` stream, the payment address in transaction details, and the block hash and height in the network response (#27, #33, #37, #38).
- gRPC client authentication tokens, and `GenerateMnemonicSeed` moves to the wallet loader service (#29).
- `PublishTransaction` and `CreateTransaction` return the transaction ID (#32).
- The gRPC server can run without TLS with `--noservertls` (#27).

### New options

- `--regtest` selects the regression test network (#23).

### Other

- Add BIP70 payment protocol support in the new `pymtproto` package (#33).
- Add a `mobile` package for embedding the wallet in mobile apps (#24).
- Refactor address generation (#30).
- Expand the getting started documentation and document the BIP44 derivation paths (#21, #22).

## 0.7.0-beta (2018-11-17)

The first release of bchwallet, forked from btcwallet and rebranded for Bitcoin Cash.

### Wallet

- Rebrand for Bitcoin Cash and build on the bchd libraries.
- New wallets get a BIP39 mnemonic seed instead of a hex seed (#5).
- Use the network's BIP44 coin type, 145 on mainnet (#15, #16).
- Fix a bug in `signrawtransaction` (#17).

### Consensus & Network

- Handle blocks whose transactions are not in topological order, for the canonical transaction ordering added in the November 2018 network upgrade (#8).

### New options

- `--blocksonly` stops the SPV backend from downloading unconfirmed transactions (#14).

### RPC & gRPC

- Register all gRPC services before serving, as gRPC 1.15.0 requires (#9).

### Build & Packaging

- Add a release build system (#10) and security disclosure information (#11).
- Build with the latest dependencies (#13).

---

# Legacy btcwallet changelog (pre-fork)

> The entries below predate the Bitcoin Cash fork. They are the upstream
> [btcwallet](https://github.com/btcsuite/btcwallet) changelog (0.1.0 through
> 0.7.0), preserved verbatim for historical reference. btcwallet stopped
> updating it after 0.7.0, and bchwallet forked from btcwallet's master in
> October 2018; see the **0.7.0-beta** entry above for the start of
> bchwallet's own history.

Changes in 0.7.0 (Mon Nov 23 2015)

- New features:
  - Wallet will now detect network inactivity and reconnect to the btcd
    RPC server if the connection was lost (#320)

- Bug fixes:
  - Removed data races in the RPC server (#292) and waddrmgr package
    (#293)
  - Corrected handling of btcutil.AddressPubKey addresses when querying
    for a ManagedAddress from the address manager (#313)
  - Fixed signmessage and verifymessage algorithm to match the equivalent
    algorithms used by Core (#324)

- Notable developer-related changes:
  - Added support for AppVeyor continuous integration (#299)
  - Take advantage of optimized zeroing from the Go 1.5 release (#286)
  - Added IsError function to waddrmgr to check that an error is a
    ManagerError and contains a matching error code (#289). Simplified
    error handling in the wallet package and RPC server with this function
    (#290).
  - Switched to using a more space efficient data structure for the
    wtxmgr CreditRecord type (#295)
  - Incorporated latest updates to the votingpool package (#315)

- Miscellaneous:
  - Updated websocket notification handlers to latest API required by
    btcd (#294)
  - Enabled the logging subsystem of the rpcclient package (#328)

- Contributors (alphabetical order):
  - Alex Yocom-Piatt
  - cjepson
  - Dave Collins
  - John C. Vernaleo
  - Josh Rickmar
  - Rune T. Aune

Changes in 0.6.0 (Wed May 27 2015)

- New features:
  - Add initial account support (#155):
    - Add account names for each account number
    - Create initial account with the "default" name
    - Create new accounts using the createnewaccount RPC
    - All accounts (with the exception of the imported account) may be
      renamed using the renameaccount RPC
    - RPC requests with an unspecified account that default to the unnamed
      account in Bitcoin Core Wallet default to "default", the name of the
      initial account
    - Several RPCs with account parameters do not work with btcwallet
      accounts due to concerns over expectations of API compatibility with
      Bitcoin Core Wallet. A new RPC API is being planned to rectify this
      (#220).
  - Store transactions, transaction history, and spend tracking in the
    database (#217, #234)
    - A full rescan is required when updating from previous wallet
      versions to rebuild the transaction history
  - Add utility (cmd/dropwtxmgr) to drop transaction history and force a
    rescan (#234)
  - Implement the help RPC to return single line usages of all wallet and
    btcd server requests as well as detailed usage for a single request

- Bug fixes:
  - Handle chain reorgs by unconfirming transactions from removed blocks
    (#248)
  - Rollback all transaction history when none of the saved recently seen
    block hashes are known to btcd (#234, #281)
  - Prevent the situation where the default account was renamed but cannot
    be renamed back to "" or "default" by removing the special case naming
    policy for the default account (#253)
  - Create the initial account address if needed when calling the
    getaccountaddress RPC (#238)
  - Prevent listsinceblock RPC from including all listtransactions result
    objects for all transactions since the genesis block (fix included in
    #227)
  - Add missing fields to listtransactions and gettransaction RPC results
    (#265)
  - Remove target confirmations limit on listsinceblock results (#266)
  - Add JSON array to report errors creating input signature for
    signrawtransaction RPC (#267)
  - Use negative fees with listtransactions result types (#272)
  - Prevent duplicate wallet lock attempt after timeout if explicitly
    locked (#275)
  - Use correct RPC server JSON-RPC error code for incorrect passphrases
    with a walletpassphrase request (#284)

- Regressions:
  - Inserting transactions and marking outputs as controlled by wallet in
    the new transaction database is extremely slow compared to the previous
    in-memory implementation. Later versions may improve this performance
    regression by using write-ahead logging (WAL) and performing more
    updates at a time under a single database transaction.

- Notable developer-related changes:
  - Relicense all code to the btcsuite developers (#258)
  - Replace txstore package with wtxmgr, the walletdb-based transaction
    store (#217, #234)
  - Add Cursor API to walletdb for forwards and backwards iteration over
    a bucket (included in #234)
  - Factor out much of main's wallet.go into a wallet package (#213,
    #276, #255)
  - Convert RPC server and client to btcjson v2 API (#233, #227)
    - Help text and single line usages for the help RPC are pregenerated
      from descriptions in the internal/rpchelp package and saved as
      globals in main. Help text must be regenerated (using `go generate`)
      each time the btcjson struct tags change or the help definitions are
      modified.
  - Add additional features to the votingpool package:
    - Implement StartWithdrawal API to begin an Open Transactions
      withdrawal (#178)
    - Add internal APIs to store withdrawal transactions in the wallet's
      transaction database (#221)
  - Addresses marked as used after appearing publicly on the blockchain or
    in mempool; required for future single-use address support (#207)
  - Modified waddrmgr APIs to use ForEach functions to iterate over
    address strings and managed addresses to improve scability (#216)
  - Move legacy directory under internal directory to prevent importing
    of unmaintained packages (enforced since Go 1.5) (#285)
  - Improve test coverage in the waddrmgr and wtxmgr packages (#239, #217)

- Contributors (alphabetical order):
  - Dave Collins
  - Guilherme Salgado
  - Javed Khan
  - Josh Rickmar
  - Manan Patel

Changes in 0.5.1 (Fri Mar 06 2015)

- New features:
  - Add flag (--createtemp) to create a temporary simnet wallet

- Bug fixes:
  - Mark newly received transactions confirmed when the wallet is initially
    created or opened with no addresses

- Notable developer-related changes:
  - Refactor the address manager database upgrade paths for easier future
    upgrades
  - Private key zeroing functions consolidated into the internal zero package
    and optimized

Changes in 0.5.0 (Tue Mar 03 2015)

- New features:
  - Add a new address manager package (waddrmgr) to replace the previous
    wallet/keystore package:
    - BIP0032 hierarchical deterministic keys
    - BIP0043/BIP0044 multi-account hierarchy
    - Strong focus on security:
      - Wallet master encryption keys protected by scrypt PBKDF
      - NaCl-based secretbox cryptography (XSalsa20 and Poly1305)
      - Mandatory encryption of private keys and P2SH redeeming scripts
      - Optional encryption of public data, including extended public keys
        and addresses
      - Different crypto keys for redeeming scripts to mitigate cryptanalysis
      - Hardened against memory scraping through the use of actively clearing
        private material from memory when locked
      - Different crypto keys used for public, private, and script data
      - Ability for different passphrases for public and private data
    - Multi-tier scalable key design to allow instant password changes
      regardless of the number of addresses stored
    - Import WIF keys
    - Import pay-to-script-hash scripts for things such as multi-signature
      transactions
    - Ability to export a watching-only version which does not contain any
      private key material
    - Programmatically detectable errors, including encapsulation of errors
      from packages it relies on
    - Address synchronization capabilities
  - Add a new namespaced database package (walletdb):
    - Key/value store
    - Namespace support
    - Allows multiple packages to have their own area in the database without
      worrying about conflicts
    - Read-only and read-write transactions with both manual and managed modes
    - Nested buckets
    - Supports registration of backend databases
    - Comprehensive test coverage
  - Replace the createencryptedwallet RPC with a wizard-style prompt
    (--create) to create a new walletdb-backed wallet file and import keys
    from the old Armory wallet file (if any)
  - Transaction creation changes:
    - Drop default transaction fee to 0.00001 BTC per kB
    - Use standard script flags provided by the txscript package for
      transaction creation and sanity checking
    - Randomize change output index
    - Includes amounts (total spendable, total needed, and fee) in all
      insufficient funds errors
  - Add support for simnet, the private simulation test network
  - Implement the following Bitcoin Core RPCs:
    - listreceivedbyaddress (#53)
    - lockunspent, listlockunspent (#50, #55)
    - getreceivedbyaddress
    - listreceivedbyaccount
  - Reimplement btcd RPCs which return the best block to use the block most
    recently processed by wallet to avoid confirmation races:
    - getbestblockhash
    - getblockcount
  - Perform clean shutdown on interrupt or when a stop RPC is received (#69)
  - Throttle the number of connected HTTP POST and websocket client
    connections (tunable using the rpcmaxclients and rpcmaxwebsockets config
    options)
  - Provide the ability to disable TLS when connecting to a localhost btcd or
    serving localhost clients

- Rescan improvements:
  - Add a rescan notification for when the rescan has completed and no more
    rescan notifications are expected (#99)
  - Use the most recent partial sync height from a rescan progress
    notification when a rescan is restarted after the btcd connection is lost
  - Force a rescan if the transaction store cannot be opened (due to a
    missing file or if the deserialization failed)

- RPC compatibility improvements:
  - Allow the use of the `*` account name to refer to all accounts
  - Make the account parameter optional for the getbalance and
    listalltransactions requests
  - Add iswatchonly field to the validateaddress response result
  - Check address equivalence in verifymessage by comparing pubkeys and pubkey
    hashes rather than requiring the address being verified to be one
    controlled by the wallet and using its private key for verification

- Bug fixes:
  - Prevent an out-of-bounds panic when handling a gettransaction RPC.
  - Prevent a panic on client disconnect (#110).
  - Prevent double spending coins when creating multiple transactions at once
    by serializing access to the transaction creation logic (#120)
  - Mark unconfirmed transaction credits as spent when another unconfirmed
    transaction spends one (#91)
  - Exclude immature coinbase outputs from listunspent results (#103)
  - Fix several data and logic races during sync with btcd (#101)
  - Avoid a memory issue from incorrect slice usage which caused both
    duplicate and missing blocks in the transaction store when middle
    inserting transactions from a new block
  - Only spend P2PKH outputs when creating sendfrom/sendmany/sendtoaddress
    transactions (#89)
  - Return the correct UTXO set when fetching all wallet UTXOs by fixing an
    incorrect slice append
  - Remove a deadlock caused by filling the btcd notification channel (#100)
  - Avoid a confirmation race by using the most recently processed block in
    RPC handlers, rather than using the most recently notified block by btcd
  - Marshal empty JSON arrays as `[]` instead of the JSON `null` by using
    empty, non-nil Go slices
  - Flush logs and run all deferred functions before main returns and the
    process exits
  - Sync temporary transaction store flat file before closing and renaming
  - Accept hex strings with an odd number of characters

- Notable developer-related changes:
  - Switch from the go.net websocket package to gorilla websockets
  - Refactor the RPC server:
    - Move several global variables to the rpcServer struct
    - Dynamically look up appropriate handlers for the current btcd connection
      status and wallet sync state
    - Begin creating websocket notifications by sending to one of many
      notification channels in the rpcServer struct, which are in turn
      marshalled and broadcast to each websocket client
  - Separate the RPC client code into the chain package:
    - Uses rpcclient for a btcd websocket RPC client
    - Converts all notification callbacks to typed messages sent over channels
    - Uses an unbounded queue for waiting notifications
  - Import a new voting pool package (votingpool):
    - Create and fetch voting pools and series from a walletdb namespace
    - Generate deposit addresses utilizing m-of-n multisig P2SH scripts
  - Improve transaction creation readability by splitting a monolithic
    function into several smaller ones
  - Check and handle all errors in some way, or explicitly comment why a
    particular error was left unchecked
  - Simplify RPC error handling by wrapping specific errors in unique types to
    create an appropriate btcjson error before the response is marshalled
  - Add a map of unspent outputs (keyed by outpoint) to the transaction store
    for quick lookup of any UTXO and access to the full wallet UTXO set
    without iterating over many transactions looking for unspent credits
  - Modify several data structures and function signatures have been modified
    to reduce the number of needed allocations and be more cache friendly

- Miscellaneous:
  - Rewrite paths relative to the data directory when an alternate data
    directory is provided on the command line
  - Switch the websocket endpoint to `ws` to match btcd
  - Remove the getaddressbalance extension RPC to discourage address reuse and
    encourage watching for expected payments by using listunspent
  - Increase transaction creation performance by moving the sorting of
    transaction outputs by their amount out of an inner loop
  - Add additional logging to the transaction store:
    - Log each transaction added to the store
    - Log each previously unconfirmed transaction that is mined
    - [debug] Log which previous outputs are marked spent by a newly inserted
      debiting transaction
    - [debug] Log each transaction that is removed in a rollback
  - Only log rollbacks if transactions are reorged out of the old chain
  - Save logs to network-specific directories
    (e.g. ~/.btcwallet/logs/testnet3) to match btcd behavior (#114)

Changes in 0.4.0 (Sun May 25 2014)

- Implement the following standard bitcoin server RPC requests:
  - signmessage (https://github.com/conformal/btcwallet/issues/58)
  - verifymessage (https://github.com/conformal/btcwallet/issues/61)
  - listunspent (https://github.com/conformal/btcwallet/issues/54)
  - validateaddress (https://github.com/conformal/btcwallet/issues/60)
  - addressmultisig (https://github.com/conformal/btcwallet/issues/37)
  - createmultisig (https://github.com/conformal/btcwallet/issues/37)
  - signrawtransaction (https://github.com/conformal/btcwallet/issues/59)

- Add authenticate extension RPC request to authenticate a websocket
  session without requiring the use of the HTTP Authorization header

- Add btcdusername and btcdpassword options to allow separate
  authentication credentials from wallet clients when authenticating to a
  btcd websocket RPC server

- Fix RPC response passthrough: JSON unmarshaling and marshaling is now
  delayed until necessary and JSON result objects from btcd are sent to
  clients directly without an extra decode+encode that may change the
  representation of large integer values

- Fix several websocket client connection issues:
  - Disconnect clients are cleanly removed without hanging on any final
    sends
  - Set deadline for websocket client sends to prevent hanging on
    misbehaving clients or clients with a bad connection

- Fix return result for dumprivkey by always padding the private key bytes
  to a length of 32

- Fix rescan for transaction history for imported addresses
  (https://github.com/conformal/btcwallet/issues/74)

- Fix listsinceblock request handler to consider the minimum confirmation
  parameter (https://github.com/conformal/btcwallet/issues/80)

- Fix several RPC handlers which require an unlocked wallet to check
  for an unlocked wallet before continuing
  (https://github.com/conformal/btcwallet/issues/65)

- Fix handling for block rewards (coinbase transactions):
  - Update listtransactions results to use "generate" category for
    coinbase outputs
  - Prevent inclusion of immature coinbase outputs for newly created
    transactions

- Rewrite the transaction store to handle several issues regarding
  transation malleability and performance issues
  - The new transaction store is written to disk in a different format
    then before, and upgrades will require a rescan to rebuild the
    transaction history

- Improve rescan:
  - Begin rescan with known UTXO set at start height
  - Serialize executation of all rescan requests
  - Merge waiting rescan jobs so all jobs can be handled with a single
    rescan
  - Support parially synced addresses in the keystore and incrementally
    mark rescan progress. If a rescan is unable to continue (wallet
    closes, btcd disconnects, etc.) a new rescan can start at the last
    synced chain height

- Notify (with an unsolicited notification) websocket clients of btcd
  connection state

- Improve logging:
  - Log reason for disconnecting a websocket client

- Updates for btcd websocket API changes

- Stability fixes, internal API changes, general code cleanup, and comment
  corrections

Changes in 0.3.0 (Mon Feb 10 2014)

- Use correct hash algorithm for chained addresses (fixes a bug where
  address chaining was still deterministic, but forked from Armory and
  previous btcwallet implementations)

- Change websocket endpoint to connect to btcd 0.6.0-alpha

- Redo server implementation to serialize handling of client requests

- Redo account locking to greatly reduce btcwallet lockups caused by
  incorrect mutex usage

- Open all accounts, rather than just the default account, at startup

- Generate new addresses using pubkey chaining if keypool is depleted and
  wallet is locked

- Make maximum keypool size a configuration option (keypoolsize)

- Add disallowfree configuration option (default false) to force adding
  the minimum fee to all outbound transactions

- Implement the following standard bitcoin server RPC requests:
  - getinfo (https://github.com/conformal/btcwallet/issues/63)
  - getrawchangeaddress (https://github.com/conformal/btcwallet/issues/41)
  - getreceivedbyaccount (https://github.com/conformal/btcwallet/issues/42)
  - gettransaction (https://github.com/conformal/btcwallet/issues/44)
  - keypoolrefill (https://github.com/conformal/btcwallet/issues/48)
  - listsinceblock (https://github.com/conformal/btcwallet/issues/52)
  - sendtoaddress (https://github.com/conformal/btcwallet/issues/56)

- Add empty (unimplemented) handlers for the following RPC requests so
  requests are not passed down to btcd:
  - getblocktemplate
  - getwork
  - stop

- Add RPC extension request, exportwatchingwallet, to export an account
  with a watching-only wallet from an account with a hot wallet that
  may be used by a separate btcwallet instance

- Require all account wallets to share the same passphrase

- Change walletlock and walletpassphrase RPC requests to lock or unlock
  all account wallets

- Allow opening accounts with watching-only wallets

- Return txid for sendfrom RPC requests
  (https://github.com/conformal/btcwallet/issues/64)

- Rescan imported private keys in background
  (https://github.com/conformal/btcwallet/issues/34)

- Do not import duplicate private keys
  (https://github.com/conformal/btcwallet/issues/35)

- Write all three account files for a new account, rather than just
  the wallet (https://github.com/conformal/btcwallet/issues/30)

- Create any missing directories before writing autogenerated certificate
  pair

- Fix rescanning of a new account's root address

- Fix error in the wallet file serialization causing duplicate address
  encryption attempts

- Fix issue calculating eligible transaction inputs caused by a bad
  confirmation check

- Fix file locking issue on Windows caused by not closing files before
  renaming

- Fix typos in README file

Changes in 0.2.1 (Thu Jan 10 2014)

- Fix a mutex issue which caused btcwallet to lockup on all
  RPC requests needing to read or write an account

Changes in 0.2.0 (Thu Jan 09 2014)

- Enable mainnet support (disabled by default, use --mainnet to enable)

- Don't hardcode localhost btcd connections. Instead, add a --connect
  option to specify the hostname or address and port of a local or
  remote btcd instance
  (https://github.com/conformal/btcwallet/issues/1)

- Remove --serverport port and replace with --listen. This option works
  just like btcd's --rpclisten and allows to specify the interfaces to
  listen for RPC connections

- Require TLS and Basic HTTP authentication before wallet can be
  controlled over RPC

- Refill keypool if wallet is unlocked and keypool is emptied

- Detect and rollback saved tx/utxo info after btcd performs blockchain
  reorganizations while btcwallet was disconnected

- Add support for the following standard bitcoin JSON-RPC calls:
  - dumpprivkey (https://github.com/conformal/btcwallet/issues/9)
  - getaccount
  - getaccountaddress
  - importprivkey (https://github.com/conformal/btcwallet/issues/2)
  - listtransactions (https://github.com/conformal/btcwallet/issues/12)

- Add several extension RPC calls for websocket connections:
  - getaddressbalance: get the balance associated with a single address
  - getunconfirmedbalance: get total balance for unconfirmed transactions
  - listaddresstransactions: list transactions for a single address
    (https://github.com/conformal/btcwallet/issues/27)
  - listalltransactions: lists all transactions without specifying a range

- Make RPC extensions available only to websocket connections, with the
  exception of createencryptedwallet

- Add dummy handlers for unimplemented wallet RPC calls
  (https://github.com/conformal/btcwallet/issues/29)

- Add socks5/tor proxy support

- Calculate and add minimum transaction fee to created transactions

- Use OS-specific rename calls to provide atomic file renames which
  can replace a currently-existing file
  (https://github.com/conformal/btcwallet/issues/20)

- Move account files to a single directory per bitcoin network to
  prevent a future scaling issue
  (https://github.com/conformal/btcwallet/issues/16)

- Fix several data races and mutex mishandling

- Fix a bug where the RPC server hung on requests requiring btcd
  when a btcd connection was never established

- Fix a bug where creating account files did not create all necessary
  directories (https://github.com/conformal/btcwallet/issues/15)

- Fix a bug where '~' did not expand to a home or user directory
  (https://github.com/conformal/btcwallet/issues/17)

- Fix a bug where returning account names as strings did not remove
  trailing ending 0s

- Fix a bug where help usage was displayed twice using the -h or --help
  flag

- Fix sample listening address in sample configuration file

- Update sample configuration file with all available options with
  descriptions and defaults for each

Initial Release 0.1.0 (Wed Nov 13 2013)

- Initial release
