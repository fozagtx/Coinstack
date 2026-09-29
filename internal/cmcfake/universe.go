package cmcfake

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Fixture asset ids. They are present in every universe regardless of seed
// or size, so tests and demos can rely on them.
const (
	// IDStepnGMT is STEPN (GMT), ranked near 250. Together with
	// IDGoMiningGMT it forms a truly ambiguous ticker: two unrelated assets
	// with symbol GMT and close ranks.
	IDStepnGMT int64 = 18069
	// IDGoMiningGMT is GoMining (GMT), ranked near 400.
	IDGoMiningGMT int64 = 10087
	// IDWormholeETH is a bridged clone with symbol ETH, ranked near 2300.
	IDWormholeETH int64 = 29901
	// IDPortalETH is an unranked bridged clone with symbol ETH.
	IDPortalETH int64 = 29902
	// IDWormholeSOL is a bridged clone with symbol SOL, ranked near 2600.
	IDWormholeSOL int64 = 29903
	// IDPortalBTC is an unranked bridged clone with symbol BTC.
	IDPortalBTC int64 = 29904
	// IDWormholeUNI is a bridged clone with symbol UNI, ranked near 2800.
	IDWormholeUNI int64 = 29905
	// IDUnicornInu is an unranked meme token with symbol UNI.
	IDUnicornInu int64 = 29906
)

// NewListingIDs returns the ids of the fixture assets that were added
// between 1 and 10 days before the server was created, newest first.
func NewListingIDs() []int64 {
	var out []*fixture
	for i := range fixtures {
		if fixtures[i].daysAgo > 0 {
			out = append(out, &fixtures[i])
		}
	}
	slices.SortFunc(out, func(a, b *fixture) int { return cmpFloat(a.daysAgo, b.daysAgo) })
	ids := make([]int64, len(out))
	for i, f := range out {
		ids[i] = f.id
	}
	return ids
}

// MinAssets is the smallest universe the fake builds: the real assets plus
// the fixtures. Smaller Options.Assets values are raised to it.
var MinAssets = len(realAssets) + len(fixtures)

// platform is the parent chain of a token.
type platform struct {
	id     int64
	name   string
	symbol string
	slug   string
}

var (
	platEthereum = &platform{1027, "Ethereum", "ETH", "ethereum"}
	platBSC      = &platform{1839, "BNB Smart Chain (BEP20)", "BNB", "bnb"}
	platSolana   = &platform{5426, "Solana", "SOL", "solana"}
	platArbitrum = &platform{11841, "Arbitrum", "ARB", "arbitrum"}
	platOptimism = &platform{11840, "Optimism", "OP", "optimism-ethereum"}
	platPolygon  = &platform{3890, "Polygon", "MATIC", "polygon"}
)

// coin is the identity and metadata of one asset. Published market
// snapshots share it, so it never changes after the universe is built.
type coin struct {
	id           int64
	name         string
	symbol       string
	slug         string
	plat         *platform
	tokenAddress string
	dateAdded    time.Time
	dateLaunched time.Time // zero when unknown
	tags         []string
	website      string
	description  string
	circulating  float64 // 0 for unranked assets
	selfReported float64 // self-reported circulating supply (unranked assets)
	total        float64
	maxSupply    float64 // 0 when uncapped or unknown
	infinite     bool
	ranked       bool
	pairs        int
}

// sim is the mutable market state of one asset. Only the simulator touches
// it, under Server.simMu.
type sim struct {
	c          *coin
	price      float64
	basePrice  float64
	ref1h      float64 // price roughly one hour ago
	ref24h     float64
	ref7d      float64
	ref30d     float64
	volume     float64
	baseVolume float64
	volRef     float64 // volume roughly 24 hours ago
	sigma      float64 // log-price volatility per tick
	stable     bool
	peg        *sim // bridged clones follow the price of the asset they wrap
}

type realAsset struct {
	id     int64
	symbol string
	name   string
	slug   string
	price  float64
	circ   float64
	total  float64
	max    float64
	plat   *platform
	site   string
	tags   []string
}

var btcTags = []string{
	"mineable", "pow", "sha-256", "store-of-value", "state-channel",
	"coinbase-ventures-portfolio", "three-arrows-capital-portfolio",
	"polychain-capital-portfolio", "binance-labs-portfolio",
	"blockchain-capital-portfolio", "boostvc-portfolio", "cms-holdings-portfolio",
	"dcg-portfolio", "dragonfly-capital-portfolio", "electric-capital-portfolio",
	"fabric-ventures-portfolio", "framework-ventures-portfolio",
	"galaxy-digital-portfolio", "huobi-capital-portfolio",
	"alameda-research-portfolio", "a16z-portfolio", "1confirmation-portfolio",
	"winklevoss-capital-portfolio", "usv-portfolio",
	"placeholder-ventures-portfolio", "pantera-capital-portfolio",
	"multicoin-capital-portfolio", "paradigm-portfolio", "bitcoin-ecosystem",
	"layer-1",
}

var ethTags = []string{
	"pos", "smart-contracts", "ethereum-ecosystem", "coinbase-ventures-portfolio",
	"three-arrows-capital-portfolio", "polychain-capital-portfolio",
	"binance-labs-portfolio", "blockchain-capital-portfolio", "boostvc-portfolio",
	"cms-holdings-portfolio", "dcg-portfolio", "dragonfly-capital-portfolio",
	"electric-capital-portfolio", "fabric-ventures-portfolio",
	"framework-ventures-portfolio", "hashkey-capital-portfolio",
	"kenetic-capital-portfolio", "huobi-capital-portfolio",
	"alameda-research-portfolio", "a16z-portfolio", "1confirmation-portfolio",
	"winklevoss-capital-portfolio", "usv-portfolio", "layer-1", "rollups",
}

// realAssets are real top assets with their real CMC ids, symbols, names
// and slugs. Prices and supplies are plausible, not live.
var realAssets = []realAsset{
	{1, "BTC", "Bitcoin", "bitcoin", 112000, 19.93e6, 19.93e6, 21e6, nil, "https://bitcoin.org/", btcTags},
	{1027, "ETH", "Ethereum", "ethereum", 4150, 120.7e6, 120.7e6, 0, nil, "https://www.ethereum.org/", ethTags},
	{825, "USDT", "Tether USDt", "tether", 1.0002, 172e9, 175e9, 0, platEthereum, "https://tether.to/", []string{"stablecoin", "asset-backed-stablecoin", "usd-stablecoin", "ethereum-ecosystem"}},
	{52, "XRP", "XRP", "xrp", 2.86, 59.8e9, 99.98e9, 100e9, nil, "https://xrpl.org/", []string{"medium-of-exchange", "enterprise-solutions", "xrp-ecosystem", "layer-1"}},
	{1839, "BNB", "BNB", "bnb", 985, 139.2e6, 139.2e6, 200e6, nil, "https://www.bnbchain.org/", []string{"marketplace", "centralized-exchange", "smart-contracts", "bnb-chain-ecosystem", "layer-1"}},
	{5426, "SOL", "Solana", "solana", 212, 543e6, 608e6, 0, nil, "https://solana.com/", []string{"pos", "platform", "solana-ecosystem", "layer-1"}},
	{3408, "USDC", "USDC", "usd-coin", 0.9999, 73.5e9, 73.5e9, 0, platEthereum, "https://www.circle.com/en/usdc", []string{"stablecoin", "asset-backed-stablecoin", "usd-stablecoin", "ethereum-ecosystem"}},
	{74, "DOGE", "Dogecoin", "dogecoin", 0.238, 150.9e9, 150.9e9, 0, nil, "http://dogecoin.com/", []string{"mineable", "pow", "scrypt", "medium-of-exchange", "memes", "doggone-doggerel"}},
	{1958, "TRX", "TRON", "tron", 0.338, 94.7e9, 94.7e9, 0, nil, "https://tron.network/", []string{"media", "payments", "tron-ecosystem", "layer-1"}},
	{2010, "ADA", "Cardano", "cardano", 0.82, 36.5e9, 45e9, 45e9, nil, "https://www.cardano.org/", []string{"dpos", "pos", "platform", "research", "smart-contracts", "cardano-ecosystem", "layer-1"}},
	{5805, "AVAX", "Avalanche", "avalanche", 30.2, 422e6, 458e6, 715.7e6, nil, "https://avax.network/", []string{"defi", "smart-contracts", "avalanche-ecosystem", "layer-1"}},
	{1975, "LINK", "Chainlink", "chainlink", 21.4, 678e6, 1e9, 1e9, platEthereum, "https://chain.link/", []string{"platform", "defi", "oracles", "smart-contracts", "ethereum-ecosystem"}},
	{6636, "DOT", "Polkadot", "polkadot-new", 4.08, 1.62e9, 1.62e9, 0, nil, "https://polkadot.network/", []string{"substrate", "polkadot", "binance-chain", "polkadot-ecosystem", "layer-0"}},
	{2, "LTC", "Litecoin", "litecoin", 106, 76.3e6, 84e6, 84e6, nil, "https://litecoin.org/", []string{"mineable", "pow", "scrypt", "medium-of-exchange", "layer-1"}},
	{7083, "UNI", "Uniswap", "uniswap", 7.85, 630e6, 1e9, 1e9, platEthereum, "https://uniswap.org/", []string{"decentralized-exchange-dex-token", "defi", "dao", "amm", "ethereum-ecosystem"}},
	{1831, "BCH", "Bitcoin Cash", "bitcoin-cash", 560, 19.92e6, 19.92e6, 21e6, nil, "https://bch.info/", []string{"mineable", "pow", "sha-256", "marketplace", "medium-of-exchange", "layer-1"}},
	{11419, "TON", "Toncoin", "toncoin", 2.82, 2.58e9, 5.14e9, 0, nil, "https://ton.org/", []string{"pos", "toncoin-ecosystem", "layer-1"}},
	{5994, "SHIB", "Shiba Inu", "shiba-inu", 0.0000125, 589.2e12, 589.5e12, 0, platEthereum, "https://shibatoken.com/", []string{"memes", "ethereum-ecosystem", "doggone-doggerel"}},
	{3957, "LEO", "UNUS SED LEO", "unus-sed-leo", 9.52, 922e6, 985e6, 0, platEthereum, "https://www.bitfinex.com/", []string{"centralized-exchange", "discount-token", "ethereum-ecosystem"}},
	{4943, "DAI", "Dai", "multi-collateral-dai", 1.0001, 5.36e9, 5.36e9, 0, platEthereum, "https://makerdao.com/", []string{"defi", "stablecoin", "algorithmic-stablecoin", "usd-stablecoin", "ethereum-ecosystem"}},
	{512, "XLM", "Stellar", "stellar", 0.372, 32.1e9, 50e9, 50e9, nil, "https://www.stellar.org/", []string{"medium-of-exchange", "enterprise-solutions", "layer-1"}},
	{3794, "ATOM", "Cosmos", "cosmos", 4.42, 474e6, 474e6, 0, nil, "https://cosmos.network/", []string{"platform", "cosmos-ecosystem", "interoperability", "layer-1"}},
	{1321, "ETC", "Ethereum Classic", "ethereum-classic", 19.6, 153.4e6, 153.4e6, 210.7e6, nil, "https://ethereumclassic.org/", []string{"mineable", "pow", "ethash", "platform", "smart-contracts", "layer-1"}},
	{328, "XMR", "Monero", "monero", 292, 18.45e6, 18.45e6, 0, nil, "https://getmonero.org/", []string{"mineable", "pow", "medium-of-exchange", "privacy", "ringct", "layer-1"}},
	{20947, "SUI", "Sui", "sui", 3.31, 3.55e9, 10e9, 10e9, nil, "https://sui.io/", []string{"binance-launchpool", "coinbase-ventures-portfolio", "layer-1", "sui-ecosystem"}},
	{3635, "CRO", "Cronos", "cronos", 0.205, 34.1e9, 100e9, 100e9, nil, "https://cronos.org/", []string{"medium-of-exchange", "cosmos-ecosystem", "centralized-exchange", "layer-1"}},
	{6535, "NEAR", "NEAR Protocol", "near-protocol", 2.71, 1.27e9, 1.27e9, 0, nil, "https://near.org/", []string{"platform", "ai-big-data", "near-protocol-ecosystem", "layer-1"}},
	{21794, "APT", "Aptos", "aptos", 4.33, 690e6, 1.18e9, 0, nil, "https://aptosfoundation.org/", []string{"binance-launchpool", "layer-1", "aptos-ecosystem"}},
	{11841, "ARB", "Arbitrum", "arbitrum", 0.463, 5.4e9, 10e9, 10e9, platArbitrum, "https://arbitrum.foundation/", []string{"dao", "arbitrum-ecosystem", "layer-2", "rollups"}},
	{11840, "OP", "Optimism", "optimism-ethereum", 0.724, 1.76e9, 4.29e9, 4.29e9, platOptimism, "https://www.optimism.io/", []string{"layer-2", "rollups", "optimism-ecosystem"}},
	{2280, "FIL", "Filecoin", "filecoin", 2.31, 700e6, 1.96e9, 1.96e9, nil, "https://filecoin.io/", []string{"mineable", "distributed-computing", "filesharing", "storage", "depin"}},
	{4642, "HBAR", "Hedera", "hedera", 0.221, 42.4e9, 50e9, 50e9, nil, "https://hedera.com/", []string{"dag", "marketplace", "enterprise-solutions", "layer-1"}},
	{3077, "VET", "VeChain", "vechain", 0.0241, 86e9, 86.7e9, 86.7e9, nil, "https://www.vechain.org/", []string{"logistics", "data-provenance", "iot", "smart-contracts", "layer-1"}},
	{7278, "AAVE", "Aave", "aave", 281, 15.2e6, 16e6, 16e6, platEthereum, "https://aave.com/", []string{"defi", "dao", "lending-borowing", "ethereum-ecosystem"}},
	{8916, "ICP", "Internet Computer", "internet-computer", 4.61, 537e6, 537e6, 0, nil, "https://internetcomputer.org/", []string{"platform", "distributed-computing", "ai-big-data", "layer-1"}},
	{24478, "PEPE", "Pepe", "pepe", 0.0000098, 420.69e12, 420.69e12, 420.69e12, platEthereum, "https://www.pepe.vip/", []string{"memes", "ethereum-ecosystem", "frog-themed"}},
	{28321, "POL", "POL (prev. MATIC)", "polygon-ecosystem-token", 0.243, 10.5e9, 10.5e9, 0, platEthereum, "https://polygon.technology/", []string{"pos", "platform", "polygon-ecosystem", "layer-2"}},
	{6210, "SAND", "The Sandbox", "the-sandbox", 0.272, 2.5e9, 3e9, 3e9, platEthereum, "https://www.sandbox.game/", []string{"gaming", "metaverse", "play-to-earn", "ethereum-ecosystem"}},
	{6719, "GRT", "The Graph", "the-graph", 0.0856, 10.6e9, 10.8e9, 0, platEthereum, "https://thegraph.com/", []string{"ai-big-data", "enterprise-solutions", "ethereum-ecosystem"}},
	{4030, "ALGO", "Algorand", "algorand", 0.221, 8.7e9, 8.7e9, 10e9, nil, "https://algorandtechnologies.com/", []string{"pos", "platform", "research", "smart-contracts", "layer-1"}},
	{1966, "MANA", "Decentraland", "decentraland", 0.293, 1.97e9, 2.19e9, 2.19e9, platEthereum, "https://decentraland.org/", []string{"platform", "gaming", "metaverse", "ethereum-ecosystem"}},
	{3155, "QNT", "Quant", "quant", 101, 14.5e6, 14.6e6, 14.6e6, platEthereum, "https://quant.network/", []string{"platform", "interoperability", "ethereum-ecosystem"}},
	{2011, "XTZ", "Tezos", "tezos", 0.702, 1.05e9, 1.05e9, 0, nil, "https://tezos.com/", []string{"pos", "platform", "smart-contracts", "layer-1"}},
	{1518, "MKR", "Maker", "maker", 1720, 870e3, 870e3, 1.005e6, platEthereum, "https://makerdao.com/", []string{"defi", "dao", "lending-borowing", "ethereum-ecosystem"}},
	{23095, "BONK", "Bonk", "bonk", 0.0000212, 79e12, 88e12, 88e12, platSolana, "https://bonkcoin.com/", []string{"memes", "solana-ecosystem", "doggone-doggerel"}},
	{18876, "APE", "ApeCoin", "apecoin", 0.552, 800e6, 1e9, 1e9, platEthereum, "https://apecoin.com/", []string{"collectibles-nfts", "gaming", "metaverse", "ethereum-ecosystem"}},
	{28752, "WIF", "dogwifhat", "dogwifhat", 0.81, 999e6, 999e6, 999e6, platSolana, "https://dogwifcoin.org/", []string{"memes", "solana-ecosystem", "doggone-doggerel"}},
	{6783, "AXS", "Axie Infinity", "axie-infinity", 2.41, 170e6, 270e6, 270e6, platEthereum, "https://axieinfinity.com/", []string{"collectibles-nfts", "gaming", "play-to-earn", "ethereum-ecosystem"}},
	{10603, "IMX", "Immutable", "immutable-x", 0.551, 1.93e9, 2e9, 2e9, platEthereum, "https://www.immutable.com/", []string{"collectibles-nfts", "gaming", "layer-2", "ethereum-ecosystem"}},
	{22861, "TIA", "Celestia", "celestia", 1.62, 780e6, 1.1e9, 0, nil, "https://celestia.org/", []string{"modular-blockchain", "cosmos-ecosystem", "layer-1"}},
	{29210, "JUP", "Jupiter", "jupiter-ag", 0.452, 3.13e9, 7e9, 10e9, platSolana, "https://jup.ag/", []string{"defi", "decentralized-exchange-dex-token", "solana-ecosystem"}},
}

type fixture struct {
	id      int64
	symbol  string
	name    string
	slug    string
	plat    *platform
	rank    int     // target rank; 0 = unranked
	price   float64 // ignored for pegged clones
	peg     int64   // id of the asset a bridged clone tracks
	stable  bool
	daysAgo float64 // > 0: added this many days before the server started
	volume  float64 // 0 = derived from market cap
	tags    []string
}

// fixtures are the deliberate duplicate tickers and the recent listings.
var fixtures = []fixture{
	{id: IDStepnGMT, symbol: "GMT", name: "STEPN", slug: "green-metaverse-token", plat: platSolana, rank: 250, price: 0.045, tags: []string{"move-to-earn", "gaming", "solana-ecosystem"}},
	{id: IDGoMiningGMT, symbol: "GMT", name: "GoMining", slug: "gomining-token", plat: platEthereum, rank: 400, price: 0.31, tags: []string{"mining", "collectibles-nfts", "ethereum-ecosystem"}},
	{id: IDWormholeETH, symbol: "ETH", name: "Ethereum (Wormhole)", slug: "ethereum-wormhole", plat: platSolana, rank: 2300, peg: 1027, tags: []string{"wrapped-tokens", "solana-ecosystem"}},
	{id: IDPortalETH, symbol: "ETH", name: "Ether (Portal Bridge)", slug: "ether-portal-bridge", plat: platBSC, peg: 1027, tags: []string{"wrapped-tokens", "bnb-chain-ecosystem"}},
	{id: IDWormholeSOL, symbol: "SOL", name: "Wrapped SOL (Wormhole)", slug: "wrapped-sol-wormhole", plat: platEthereum, rank: 2600, peg: 5426, tags: []string{"wrapped-tokens", "ethereum-ecosystem"}},
	{id: IDPortalBTC, symbol: "BTC", name: "Bitcoin (Portal Bridge)", slug: "bitcoin-portal-bridge", plat: platSolana, peg: 1, tags: []string{"wrapped-tokens", "solana-ecosystem"}},
	{id: IDWormholeUNI, symbol: "UNI", name: "Uniswap (Wormhole)", slug: "uniswap-wormhole", plat: platSolana, rank: 2800, peg: 7083, tags: []string{"wrapped-tokens", "solana-ecosystem"}},
	{id: IDUnicornInu, symbol: "UNI", name: "Unicorn Inu", slug: "unicorn-inu", plat: platBSC, price: 0.00000042, tags: []string{"memes", "bnb-chain-ecosystem"}},

	{id: 40008, symbol: "KSTRL", name: "Kestrel AI", slug: "kestrel-ai", plat: platSolana, rank: 1100, price: 0.084, daysAgo: 1.1, volume: 2.4e6, tags: []string{"ai-big-data", "solana-ecosystem"}},
	{id: 40007, symbol: "MVAL", name: "Moonvale", slug: "moonvale", plat: platBSC, rank: 1600, price: 0.0122, daysAgo: 2.3, volume: 850e3, tags: []string{"gaming", "bnb-chain-ecosystem"}},
	{id: 40006, symbol: "OBSR", name: "Obsidian Restake", slug: "obsidian-restake", plat: platEthereum, rank: 2100, price: 1.37, daysAgo: 3.2, volume: 310e3, tags: []string{"defi", "restaking", "ethereum-ecosystem"}},
	{id: 40005, symbol: "POTTR", name: "Pixel Otter", slug: "pixel-otter", plat: platSolana, price: 0.0000031, daysAgo: 4.4, volume: 3200, tags: []string{"memes", "solana-ecosystem"}},
	{id: 40004, symbol: "LDPN", name: "Lumen DePIN", slug: "lumen-depin", plat: platEthereum, rank: 2500, price: 0.19, daysAgo: 5.1, volume: 120e3, tags: []string{"depin", "ethereum-ecosystem"}},
	{id: 40003, symbol: "TIDE", name: "Tidal Perps", slug: "tidal-perps", plat: platArbitrum, rank: 1400, price: 0.66, daysAgo: 6.2, volume: 1.1e6, tags: []string{"defi", "derivatives", "arbitrum-ecosystem"}},
	{id: 40002, symbol: "CNDR", name: "Cinder Coin", slug: "cinder-coin", rank: 2900, price: 0.0045, daysAgo: 8.4, volume: 45e3, tags: []string{"pow", "layer-1"}},
	{id: 40001, symbol: "FJUSD", name: "Fjord USD", slug: "fjord-usd", plat: platEthereum, rank: 1900, price: 1, stable: true, daysAgo: 9.7, volume: 600e3, tags: []string{"stablecoin", "usd-stablecoin", "ethereum-ecosystem"}},
}

// capCurve is a plausible market cap for each rank; marketCapAt
// interpolates it on a log-log scale.
var capCurve = []struct{ rank, cap float64 }{
	{1, 2.2e12}, {10, 2.5e10}, {50, 3.0e9}, {100, 1.1e9}, {250, 2.5e8},
	{400, 1.1e8}, {1000, 1.5e7}, {2000, 2.0e6}, {3000, 3.0e5}, {5000, 3.0e4},
	{10000, 1.0e3},
}

func marketCapAt(rank float64) float64 {
	if rank <= capCurve[0].rank {
		return capCurve[0].cap
	}
	for i := 1; i < len(capCurve); i++ {
		a, b := capCurve[i-1], capCurve[i]
		if rank <= b.rank || i == len(capCurve)-1 {
			t := (math.Log(rank) - math.Log(a.rank)) / (math.Log(b.rank) - math.Log(a.rank))
			return max(10, math.Exp(math.Log(a.cap)+t*(math.Log(b.cap)-math.Log(a.cap))))
		}
	}
	return 10
}

// idDates maps CMC ids to roughly when assets with such ids were added.
var idDates = []struct {
	id   int64
	date time.Time
}{
	{1, day("2013-04-28")}, {1027, day("2015-08-07")}, {1839, day("2017-07-25")},
	{3408, day("2018-10-08")}, {5426, day("2020-04-10")}, {7083, day("2020-09-17")},
	{11419, day("2021-08-26")}, {20947, day("2022-07-12")}, {24478, day("2023-04-17")},
	{29210, day("2024-01-31")}, {33000, day("2024-09-01")}, {36000, day("2025-03-01")},
	{39000, day("2025-10-01")}, {60000, day("2026-03-01")},
}

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func dateForID(id int64) time.Time {
	if id <= idDates[0].id {
		return idDates[0].date
	}
	for i := 1; i < len(idDates); i++ {
		a, b := idDates[i-1], idDates[i]
		if id <= b.id {
			f := float64(id-a.id) / float64(b.id-a.id)
			return a.date.Add(time.Duration(f * float64(b.date.Sub(a.date))))
		}
	}
	return idDates[len(idDates)-1].date
}

var (
	nameFirst = []string{
		"Aether", "Alpine", "Amber", "Apex", "Arbor", "Astra", "Atlas", "Aurora", "Axon", "Azure",
		"Basalt", "Beacon", "Birch", "Blaze", "Bolt", "Boreal", "Cairn", "Canyon", "Cedar", "Cobalt",
		"Comet", "Coral", "Crest", "Crystal", "Cypher", "Delta", "Drift", "Dune", "Echo", "Ember",
		"Epoch", "Falcon", "Fern", "Flare", "Flux", "Forge", "Frost", "Galaxy", "Glacier", "Granite",
		"Harbor", "Helix", "Horizon", "Ion", "Iris", "Jade", "Juniper", "Karma", "Kinetic", "Lattice",
		"Lotus", "Lunar", "Maple", "Meridian", "Mesa", "Meteor", "Mosaic", "Nebula", "Nimbus", "Noble",
		"Nova", "Oasis", "Onyx", "Orbit", "Pebble", "Photon", "Pine", "Prism", "Pulse", "Quartz",
		"Quasar", "Radiant", "Raven", "Reef", "Sable", "Sage", "Sierra", "Solstice", "Sparrow", "Spire",
		"Summit", "Tangent", "Tempest", "Thunder", "Topaz", "Trident", "Tundra", "Umbra", "Vector", "Velvet",
		"Vertex", "Vortex", "Willow", "Zenith", "Zephyr", "Kraken", "Lynx", "Obelisk", "Paragon", "Rune",
	}
	nameSecond = []string{
		"AI", "Bridge", "Cash", "Chain", "Coin", "DAO", "DeFi", "Dex", "Exchange", "Finance",
		"Fund", "Games", "Gold", "Grid", "Hub", "Inu", "Labs", "Ledger", "Link", "Loop",
		"Market", "Mesh", "Meta", "Mint", "Money", "Network", "Node", "Oracle", "Pay", "Protocol",
		"Quest", "Rewards", "Shares", "Stake", "Swap", "Token", "Vault", "Verse", "Wallet", "Yield",
		"X", "Zone",
	}
	tagPool = []string{
		"defi", "gaming", "memes", "ai-big-data", "layer-2", "decentralized-exchange-dex-token",
		"collectibles-nfts", "depin", "real-world-assets", "dao", "yield-farming", "metaverse",
		"privacy", "oracles", "storage", "payments", "governance", "staking", "lending-borowing",
		"social-money",
	}
)

// builder generates a universe deterministically from one RNG.
type builder struct {
	rng     *rand.Rand
	now     time.Time
	sims    []*sim
	byID    map[int64]*sim
	symbols map[string]bool
	slugs   map[string]bool
	names   map[string]bool
}

// generate builds a universe of n assets (at least MinAssets).
func generate(n int, seed int64, now time.Time) []*sim {
	b := &builder{
		rng:     rand.New(rand.NewPCG(uint64(seed), 0x636d6366616b6531)),
		now:     now,
		byID:    map[int64]*sim{},
		symbols: map[string]bool{},
		slugs:   map[string]bool{},
		names:   map[string]bool{},
	}
	for _, r := range realAssets {
		b.addReal(r)
	}
	for _, f := range fixtures {
		b.addFixture(f)
	}
	b.addSynthetic(n - len(b.sims))
	return b.sims
}

func (b *builder) add(s *sim) {
	c := s.c
	if _, dup := b.byID[c.id]; dup {
		panic(fmt.Sprintf("cmcfake: duplicate id %d", c.id))
	}
	b.byID[c.id] = s
	b.symbols[c.symbol] = true
	b.slugs[c.slug] = true
	b.names[strings.ToLower(c.name)] = true
	b.sims = append(b.sims, s)
}

func (b *builder) addReal(r realAsset) {
	c := &coin{
		id: r.id, name: r.name, symbol: r.symbol, slug: r.slug, plat: r.plat,
		dateAdded: dateForID(r.id), tags: r.tags, website: r.site,
		circulating: r.circ, total: r.total, maxSupply: r.max, infinite: r.max == 0,
		ranked: true, pairs: 400 + b.rng.IntN(12000),
	}
	if r.id == 1 {
		c.dateAdded = day("2010-07-13")
	}
	c.dateLaunched = c.dateAdded
	c.tokenAddress = b.address(c.plat)
	c.description = describe(c)
	s := &sim{c: c, price: r.price, basePrice: r.price, sigma: 0.0022}
	scale := 1.0
	switch {
	case slices.Contains(r.tags, "stablecoin"):
		s.stable, scale, c.infinite = true, 0.02, false
	case r.id == 1:
		s.sigma, scale = 0.0010, 0.6
	case r.id == 1027:
		s.sigma, scale = 0.0013, 0.8
	case slices.Contains(r.tags, "memes"):
		s.sigma, scale = 0.0035, 1.6
	}
	mcap := r.price * r.circ
	s.volume = mcap * math.Pow(10, -1.4+0.3*b.rng.NormFloat64())
	if s.stable {
		s.volume = mcap * 0.4
	}
	s.baseVolume = s.volume
	b.initChanges(s, scale)
	b.add(s)
}

func (b *builder) addFixture(f fixture) {
	c := &coin{
		id: f.id, name: f.name, symbol: f.symbol, slug: f.slug, plat: f.plat,
		tags: f.tags, website: "https://" + f.slug + ".example/", ranked: f.rank > 0,
		pairs: 2 + b.rng.IntN(30),
	}
	c.tokenAddress = b.address(c.plat)
	c.dateAdded = dateForID(f.id)
	if f.daysAgo > 0 {
		c.dateAdded = b.now.Truncate(time.Hour).Add(-time.Duration(f.daysAgo * float64(24*time.Hour)))
	}
	c.description = describe(c)
	price := f.price
	var peg *sim
	if f.peg != 0 {
		peg = b.byID[f.peg]
		price = peg.price * (1 + 0.001*b.rng.NormFloat64())
	}
	s := &sim{c: c, price: price, basePrice: price, sigma: 0.004, stable: f.stable, peg: peg}
	if f.rank > 0 {
		c.circulating = marketCapAt(float64(f.rank)) / price
		c.total = c.circulating * (1 + 0.3*b.rng.Float64())
	} else {
		c.total = math.Pow(10, 4+3*b.rng.Float64()) / price
		c.selfReported = c.total * 0.5
	}
	if peg == nil && !f.stable {
		c.maxSupply = c.total * 1.5
	}
	s.volume = f.volume
	if s.volume == 0 {
		s.volume = price * max(c.circulating, c.total*0.05) * math.Pow(10, -1.5+0.3*b.rng.NormFloat64())
	}
	s.baseVolume = s.volume
	scale := 2.0
	if f.stable {
		scale = 0.02
	}
	b.initChanges(s, scale)
	if peg != nil {
		s.ref1h, s.ref24h, s.ref7d, s.ref30d = peg.ref1h, peg.ref24h, peg.ref7d, peg.ref30d
	}
	b.add(s)
}

func (b *builder) addSynthetic(count int) {
	if count <= 0 {
		return
	}
	ids := b.sampleIDs(count)
	slots := b.rng.Perm(count)
	for i, id := range ids {
		rankTarget := float64(slots[i] + 1 + len(realAssets))
		name, symbol, slug := b.identity()
		c := &coin{
			id: id, name: name, symbol: symbol, slug: slug,
			ranked: !(rankTarget > 300 && b.rng.Float64() < 0.03),
		}
		if b.rng.Float64() < 0.7 {
			c.plat = b.pickPlatform()
		}
		c.tokenAddress = b.address(c.plat)
		c.tags = b.pickTags(name, c.plat)
		c.website = "https://" + slug + ".example/"
		jitter := time.Duration(b.rng.IntN(40*24)-20*24) * time.Hour
		c.dateAdded = dateForID(id).Add(jitter)
		if latest := b.now.Add(-30 * 24 * time.Hour); c.dateAdded.After(latest) {
			c.dateAdded = latest.Add(-time.Duration(b.rng.IntN(24*365)) * time.Hour)
		}
		if b.rng.Float64() < 0.6 {
			c.dateLaunched = c.dateAdded.Add(-time.Duration(b.rng.IntN(24*120)) * time.Hour)
		}
		c.pairs = 1 + int(3000/math.Pow(rankTarget, 0.8)) + b.rng.IntN(5)
		c.description = describe(c)

		scale := 3.0
		switch {
		case rankTarget < 100:
			scale = 1.2
		case rankTarget < 500:
			scale = 1.6
		case rankTarget < 1500:
			scale = 2.2
		}
		price := math.Pow(10, -5.5+7.5*b.rng.Float64())
		s := &sim{c: c, price: price, basePrice: price, sigma: 0.0015 * scale * (1 + b.rng.Float64())}
		if c.ranked {
			mcap := marketCapAt(rankTarget) * (0.98 + 0.04*b.rng.Float64())
			c.circulating = mcap / price
			c.total = c.circulating * (1 + 0.5*b.rng.Float64())
			s.volume = mcap * math.Pow(10, -1.3+0.5*b.rng.NormFloat64())
			if rankTarget > 1500 {
				s.volume *= math.Pow(10, -2*b.rng.Float64())
			}
		} else {
			c.total = math.Pow(10, 4+3.5*b.rng.Float64()) / price
			c.selfReported = c.total * (0.3 + 0.6*b.rng.Float64())
			s.volume = math.Pow(10, 1+3*b.rng.Float64())
		}
		if b.rng.Float64() < 0.55 {
			c.maxSupply = c.total * (1 + 2*b.rng.Float64())
		}
		s.volume = max(s.volume, 5)
		s.baseVolume = s.volume
		b.initChanges(s, scale)
		if c.ranked && rankTarget > 1000 && b.rng.Float64() < 0.012 {
			// An illiquid pump: a huge move on almost no volume, the kind
			// of asset a min_volume filter exists to hide.
			s.ref1h = s.price / (1 + (5+35*b.rng.Float64())/100)
			s.ref24h = s.price / (1 + (60+340*b.rng.Float64())/100)
			s.volume = 100 + 4900*b.rng.Float64()
			s.baseVolume, s.volRef = s.volume, s.volume*0.2
		}
		b.add(s)
	}
}

// initChanges sets the reference prices so that the initial percent
// changes look plausible for an asset of the given volatility scale.
func (b *builder) initChanges(s *sim, scale float64) {
	ref := func(sd float64) float64 {
		pct := max(-90, sd*scale*b.rng.NormFloat64())
		return s.price / (1 + pct/100)
	}
	s.ref1h, s.ref24h, s.ref7d, s.ref30d = ref(0.6), ref(3.5), ref(9), ref(18)
	s.volRef = s.volume / max(0.2, 1+0.25*b.rng.NormFloat64())
}

// sampleIDs picks count distinct unused ids, mostly below 39000 like real
// CMC ids, sorted ascending.
func (b *builder) sampleIDs(count int) []int64 {
	var avail []int64
	for id := int64(3); id < 39000; id++ {
		if _, used := b.byID[id]; !used {
			avail = append(avail, id)
		}
	}
	var ids []int64
	if count <= len(avail) {
		for _, i := range b.rng.Perm(len(avail))[:count] {
			ids = append(ids, avail[i])
		}
	} else {
		ids = avail
		for id := int64(41001); len(ids) < count; id++ {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func (b *builder) identity() (name, symbol, slug string) {
	first := nameFirst[b.rng.IntN(len(nameFirst))]
	second := nameSecond[b.rng.IntN(len(nameSecond))]
	name = first + " " + second
	for k := 2; b.names[strings.ToLower(name)]; k++ {
		if k < 8 {
			first = nameFirst[b.rng.IntN(len(nameFirst))]
			second = nameSecond[b.rng.IntN(len(nameSecond))]
			name = first + " " + second
			continue
		}
		name = first + " " + second + " " + strconv.Itoa(k)
	}
	f, s := strings.ToUpper(first), strings.ToUpper(second)
	candidates := []string{
		prefix(f, 3) + prefix(s, 1), prefix(f, 2) + prefix(s, 2), prefix(f, 4),
		prefix(f, 3) + prefix(s, 2), prefix(f, 1) + prefix(s, 4), prefix(f, 5),
	}
	symbol = ""
	for _, cand := range candidates {
		if !b.symbols[cand] {
			symbol = cand
			break
		}
	}
	for n := 2; symbol == ""; n++ {
		if cand := prefix(f, 3) + strconv.Itoa(n); !b.symbols[cand] {
			symbol = cand
		}
	}
	slug = strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	for n := 2; b.slugs[slug]; n++ {
		slug = strings.ToLower(strings.ReplaceAll(name, " ", "-")) + "-" + strconv.Itoa(n)
	}
	return name, symbol, slug
}

func prefix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func (b *builder) pickPlatform() *platform {
	switch x := b.rng.Float64(); {
	case x < 0.5:
		return platEthereum
	case x < 0.7:
		return platBSC
	case x < 0.85:
		return platSolana
	case x < 0.92:
		return platArbitrum
	default:
		return platPolygon
	}
}

func (b *builder) pickTags(name string, p *platform) []string {
	var tags []string
	for _, kw := range []struct{ word, tag string }{
		{" AI", "ai-big-data"}, {" Inu", "memes"}, {" DeFi", "defi"}, {" Dex", "decentralized-exchange-dex-token"},
		{" Swap", "decentralized-exchange-dex-token"}, {" Games", "gaming"}, {" DAO", "dao"}, {" Yield", "yield-farming"},
	} {
		if strings.Contains(name, kw.word) {
			tags = append(tags, kw.tag)
		}
	}
	for range b.rng.IntN(4) {
		if t := tagPool[b.rng.IntN(len(tagPool))]; !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	if p != nil {
		eco := map[int64]string{1027: "ethereum-ecosystem", 1839: "bnb-chain-ecosystem", 5426: "solana-ecosystem", 11841: "arbitrum-ecosystem", 3890: "polygon-ecosystem"}[p.id]
		if eco != "" {
			tags = append(tags, eco)
		}
	}
	return tags
}

const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// address returns a made-up contract address in the style of the platform.
func (b *builder) address(p *platform) string {
	if p == nil {
		return ""
	}
	var sb strings.Builder
	if p == platSolana {
		for range 44 {
			sb.WriteByte(base58[b.rng.IntN(len(base58))])
		}
		return sb.String()
	}
	sb.WriteString("0x")
	for range 40 {
		sb.WriteByte("0123456789abcdef"[b.rng.IntN(16)])
	}
	return sb.String()
}

func describe(c *coin) string {
	kind := "coin"
	if c.plat != nil {
		kind = "token on " + c.plat.name
	}
	return fmt.Sprintf("%s (%s) is a %s. FAKE DATA: this description was generated by fakecmc for local development and is not real market information.", c.name, c.symbol, kind)
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
