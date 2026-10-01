# pymtproto

[![Build Status](https://github.com/gcash/bchwallet/actions/workflows/main.yml/badge.svg?branch=master)](https://github.com/gcash/bchwallet/actions/workflows/main.yml)

Package pymtproto provides functions for downloading [Bip0070](https://github.com/bitcoin/bips/blob/master/bip-0070.mediawiki)
payment requests and POSTing payments back to the merchant server.

Example Usage:

```go
client := NewPaymentProtocolClient(&chaincfg.MainnetParams, nil)
paymentRequest, err := client.DownloadBip0070PaymentRequest("bitcoincash:?r=https://test.bitpay.com/i/KqSWvRBKC58CgdpfsttzBC")
```

Package pymtproto is licensed under the [copyfree](http://copyfree.org) ISC
License.
