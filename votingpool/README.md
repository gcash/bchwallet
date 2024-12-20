# votingpool

[![Build Status](https://github.com/gcash/bchwallet/actions/workflows/main.yml/badge.svg?branch=master)](https://github.com/gcash/bchwallet/actions/workflows/main.yml)

Package votingpool provides voting pool functionality for bchwallet as
described here:
[Voting Pools](http://opentransactions.org/wiki/index.php?title=Category:Voting_Pools).

A suite of tests is provided to ensure proper functionality. See
`test_coverage.txt` for the gocov coverage report. Alternatively, if you are
running a POSIX OS, you can run the `cov_report.sh` script for a real-time
report. Package votingpool is licensed under the liberal ISC license.

Note that this is still a work in progress.

## Feature Overview

- Create/Load pools
- Create series
- Replace series
- Create deposit addresses
- Comprehensive test coverage

## Documentation

[![GoDoc](https://godoc.org/github.com/gcash/bchwallet/votingpool?status.png)]
(http://godoc.org/github.com/gcash/bchwallet/votingpool)

Full `go doc` style documentation for the project can be viewed online without
installing this package by using the GoDoc site here:
http://godoc.org/github.com/gcash/bchwallet/votingpool

You can also view the documentation locally once the package is installed with
the `godoc` tool by running `godoc -http=":6060"` and pointing your browser to
http://localhost:6060/pkg/github.com/gcash/bchwallet/votingpool

Package votingpool is licensed under the [copyfree](http://copyfree.org) ISC
License.
