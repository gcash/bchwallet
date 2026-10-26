# pymtproto

[![Build Status](https://github.com/gcash/bchwallet/actions/workflows/main.yml/badge.svg?branch=master)](https://github.com/gcash/bchwallet/actions/workflows/main.yml)

Package pymtproto provides functions for downloading payment requests and
POSTing payments back to the merchant server, with version 2 of BitPay's
[JSON Payment Protocol](https://github.com/bitpay/jsonPaymentProtocol/blob/master/v2/specification.md)
or [Bip0070](https://github.com/bitcoin/bips/blob/master/bip-0070.mediawiki).

`DownloadPaymentRequest` uses the JSON Payment Protocol when the merchant
offers it, signed by a key the client trusts for its domain, which are BitPay's
own, and BIP70 otherwise. `PostPayment` sends the payment with the protocol of
the request; with the JSON Payment Protocol the merchant first verifies the
unsigned transactions.

Example Usage:

```go
client := NewPaymentProtocolClient(&chaincfg.MainNetParams, nil)
paymentRequest, err := client.DownloadPaymentRequest("bitcoincash:?r=https://bitpay.com/i/KqSWvRBKC58CgdpfsttzBC")
```

Package pymtproto is licensed under the [copyfree](http://copyfree.org) ISC
License.
