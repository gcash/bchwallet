package bcmr

// reservedSymbolList has the base symbols no token can have for itself, as an
// amount of them reads as money.  The CHIP has wallets keep such a list.  These
// are its sample list of cryptocurrencies, BCH's and BTC's other tickers, and
// the major currencies, but not all of ISO 4217, which has ordinary words such
// as TOP.  A token on the Known list isn't held to it.
var reservedSymbolList = []string{
	"1INCH", "AAVE", "ADA", "ALGO", "APE", "APT", "ATOM", "AUD", "AVAX",
	"AXS", "BAT", "BCH", "BIT", "BNB", "BNX", "BSV", "BTC", "BTT", "BUSD",
	"CAD", "CAKE", "CHF", "CHZ", "CNY", "COMP", "CRO", "CRV", "CSPR", "CVX",
	"DAI", "DASH", "DCR", "DOGE", "DOT", "EGLD", "ENJ", "ENS", "EOS", "ETC",
	"ETH", "ETHW", "EUR", "FEI", "FIL", "FLOW", "FTM", "FTT", "GBP", "GRT",
	"GT", "HBAR", "HNT", "HOT", "HT", "ICP", "JPY", "KAVA", "KCS", "KLAY",
	"LDO", "LEO", "LINK", "LRC", "LTC", "LUNC", "MANA", "MATIC", "MINA",
	"MIOTA", "MKR", "NEAR", "NEO", "NEXO", "OKB", "PAXG", "QNT", "RUNE",
	"RVN", "SAND", "SHIB", "SNX", "SOL", "STX", "THETA", "TON", "TRX", "TUSD",
	"TWT", "UNI", "USD", "USDC", "USDD", "USDN", "USDP", "USDT", "USTC",
	"VET", "WAVES", "WBTC", "XBCH", "XBT", "XCN", "XDC", "XEC", "XLM", "XMR",
	"XRP", "XTZ", "ZEC", "ZIL",
}

// reservedSymbols holds reservedSymbolList for looking up.
var reservedSymbols = func() map[string]bool {
	symbols := make(map[string]bool, len(reservedSymbolList))
	for _, symbol := range reservedSymbolList {
		symbols[symbol] = true
	}
	return symbols
}()
