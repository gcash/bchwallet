pymtproto
========

[![Build Status](https://travis-ci.org/gcash/bchwallet.png?branch=master)]
(https://travis-ci.org/gcash/bchwallet)

Package pymtproto provides functions for downloading [Bip0070](https://github.com/bitcoin/bips/blob/master/bip-0070.mediawiki) 
payment requests and POSTing payments back to the merchant server.

Example Usage:
```go
client := NewPaymentProtocolClient(&chaincfg.MainnetParams, nil)
paymentRequest, err := client.DownloadBip0070PaymentRequest("bitcoincash:?r=https://test.bitpay.com/i/KqSWvRBKC58CgdpfsttzBC")
```

Package pymtproto is licensed under the [copyfree](http://copyfree.org) ISC
License.