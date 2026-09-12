package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// One EVM implementation, N chains. Every EVM chain speaks the same JSON-RPC and
// uses the same address format, so a chain is DATA (an evmChainDef row), not
// code — adding Optimism later is a row here plus an entry in BILLING_EVM_CHAINS,
// with no new file and no new interface implementation.
//
// Only ERC-20 stablecoins are watched. A native ETH/BNB transfer emits no log,
// so eth_getLogs cannot see it — finding those needs full block-body scanning or
// a provider-specific trace API. That is a separate change, and stablecoins also
// dodge the price-volatility problem during the payment window.

// transferTopic is keccak256("Transfer(address,address,uint256)"), the topic0 of
// every ERC-20 Transfer. Hardcoded rather than computed so this file needs no
// keccak dependency — it is a fixed, universally published constant.
const transferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// evmScanChunk bounds one Scan call's block range. Public RPC endpoints cap how
// wide an eth_getLogs span may be, and a cold start would otherwise ask for
// millions of blocks and simply error.
//
// Before adding or changing a chain's RPC, test eth_getLogs at THIS width, not
// just eth_blockNumber. Free endpoints commonly serve the cheap call and refuse
// the log query — as an archive-token requirement or a hard range cap (1rpc.io
// allows 50 blocks) — and that failure is invisible from the outside: the cursor
// still advances off eth_blockNumber, so the chain looks alive while crediting
// nothing. BILLING_EVM_RPC_<CHAIN> exists to point a chain at a paid provider.
const evmScanChunk = 2000

type evmToken struct {
	Symbol   string
	Contract string // lowercase 0x…
	Decimals int
}

type evmChainDef struct {
	Label   string
	ChainID int
	RPC     string
	MinConf int64
	Tokens  []evmToken
}

// evmChainDefs is the built-in chain table.
//
// !! THE CONTRACT ADDRESSES BELOW ARE THE SECURITY BOUNDARY. !!
// Scans filter by contract address, so a worthless token that merely calls
// itself "USDT" can never credit an invoice unless its address is listed here.
// The flip side is that a WRONG address means either silently missing every
// payment, or watching a token that is not the one being priced. Verify each
// address against a block explorer before enabling that chain in production.
//
// Decimals matter just as much: USDT is 6 decimals nearly everywhere but 18 on
// BSC. Getting one wrong misreads an amount by a factor of 10^12.
var evmChainDefs = map[string]evmChainDef{
	"ethereum": {
		Label: "Ethereum", ChainID: 1, RPC: "https://ethereum-rpc.publicnode.com", MinConf: 3,
		Tokens: []evmToken{
			{"USDT", "0xdac17f958d2ee523a2206206994597c13d831ec7", 6},
			{"USDC", "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", 6},
		},
	},
	"base": {
		Label: "Base", ChainID: 8453, RPC: "https://base-rpc.publicnode.com", MinConf: 5,
		Tokens: []evmToken{
			{"USDC", "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913", 6},
		},
	},
	"arbitrum": {
		// NOT publicnode: it serves eth_blockNumber fine but answers eth_getLogs
		// with "Archive requests require a personal token" for anything more than
		// ~20 blocks behind the tip. Arbitrum produces blocks roughly 4x a second,
		// so every scan is "archive" by that definition and NO payment would ever
		// be credited — while the chain still looked healthy, because the cursor
		// advanced off eth_blockNumber. Verified in production, 2026-09-12.
		Label: "Arbitrum One", ChainID: 42161, RPC: "https://arb1.arbitrum.io/rpc", MinConf: 5,
		Tokens: []evmToken{
			{"USDT", "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", 6},
			{"USDC", "0xaf88d065e77c8cc2239327c5edb3a432268e5831", 6},
		},
	},
	"bsc": {
		Label: "BNB Smart Chain", ChainID: 56, RPC: "https://bsc-rpc.publicnode.com", MinConf: 15,
		Tokens: []evmToken{
			// BSC-USD. EIGHTEEN decimals, unlike USDT everywhere else.
			{"USDT", "0x55d398326f99059ff775485246999027b3197955", 18},
		},
	},
	"polygon": {
		Label: "Polygon", ChainID: 137, RPC: "https://polygon-bor-rpc.publicnode.com", MinConf: 30,
		Tokens: []evmToken{
			{"USDT", "0xc2132d05d31c914a87c6611c10748aeb04b58e8f", 6},
			{"USDC", "0x3c499c542cef5e3811e1192ce70d8cc03d5c3359", 6},
		},
	},
}

// isEVMAddress reports whether s is a 0x-prefixed, 40-hex-digit address.
//
// This check earns its place because a malformed address does NOT announce
// itself. It would be stored on every invoice, shown to buyers to copy, and
// padded into a log topic that simply never matches — so the poller would scan
// happily, advance its cursors and report no errors, while being structurally
// incapable of crediting anyone.
//
// That is not hypothetical: on 2026-09-12 Kamal's YAML 1.1 parser read the
// unquoted 0x… value in the deploy config as an integer, and production ran with
// the address's decimal expansion. Config will go wrong again; refusing to start
// a rail on a malformed address is what keeps the next time loud.
func isEVMAddress(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for _, r := range s[2:] {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

type evmWatcher struct {
	name  string
	def   evmChainDef
	rpc   string
	payTo string
	http  *http.Client
}

func newEVMWatcher(name string, def evmChainDef, payTo string) *evmWatcher {
	rpc := def.RPC
	if o := evmRPCOverride(name); o != "" {
		rpc = o
	}
	return &evmWatcher{
		name:  name,
		def:   def,
		rpc:   rpc,
		payTo: payTo,
		http:  &http.Client{Timeout: 20 * time.Second},
	}
}

func (w *evmWatcher) Name() string          { return w.name }
func (w *evmWatcher) Label() string         { return w.def.Label }
func (w *evmWatcher) LockNamespace() string { return lockNSEVM }
func (w *evmWatcher) PayTo() string         { return w.payTo }

// Scan reads one chunk of ERC-20 Transfer logs addressed to our receive address.
//
// Confirmations are enforced by never LOOKING at blocks that are not yet deep
// enough: the ceiling is latest-MinConf. That is far simpler than tracking a
// pending set and re-checking it, and it handles reorgs for free — a reorged
// block is gone before it ever enters the scanned range.
func (w *evmWatcher) Scan(ctx context.Context, cursor string, wantPayments bool) ([]observedPayment, string, error) {
	latest, err := w.blockNumber(ctx)
	if err != nil {
		return nil, cursor, fmt.Errorf("%s: eth_blockNumber: %w", w.name, err)
	}
	safeTo := latest - w.def.MinConf
	if safeTo < 0 {
		return nil, cursor, nil
	}

	// An empty cursor means this chain has never been scanned. Start at the
	// current tip rather than at genesis: no invoice can predate this moment, so
	// there is nothing behind us worth reading.
	from, ok := parseBlockCursor(cursor)
	if !ok {
		return nil, formatBlockCursor(safeTo), nil
	}
	from++
	if from > safeTo {
		return nil, cursor, nil // nothing new is deep enough yet
	}
	to := from + evmScanChunk - 1
	if to > safeTo {
		to = safeTo
	}

	// Nothing payable: advance past this range without paying for the log query.
	if !wantPayments {
		return nil, formatBlockCursor(to), nil
	}

	addrs := make([]string, 0, len(w.def.Tokens))
	for _, t := range w.def.Tokens {
		addrs = append(addrs, t.Contract)
	}
	var logs []evmLog
	err = w.call(ctx, "eth_getLogs", []any{map[string]any{
		"fromBlock": hexBlock(from),
		"toBlock":   hexBlock(to),
		"address":   addrs,
		// topic1 (from) is left open; topic2 (to) pins the recipient to us.
		"topics": []any{transferTopic, nil, padAddressTopic(w.payTo)},
	}}, &logs)
	if err != nil {
		return nil, cursor, fmt.Errorf("%s: eth_getLogs %d-%d: %w", w.name, from, to, err)
	}

	var obs []observedPayment
	for _, lg := range logs {
		tok, ok := w.tokenByContract(lg.Address)
		if !ok {
			continue // not a token we price in — ignore rather than guess
		}
		v, ok := new(big.Int).SetString(strings.TrimPrefix(strings.ToLower(lg.Data), "0x"), 16)
		if !ok {
			continue
		}
		obs = append(obs, observedPayment{
			Chain:  w.name,
			Token:  tok.Symbol,
			Amount: scaleDecimal(v, tok.Decimals),
			TxHash: lg.TxHash,
		})
	}
	return obs, formatBlockCursor(to), nil
}

func (w *evmWatcher) tokenByContract(addr string) (evmToken, bool) {
	addr = strings.ToLower(addr)
	for _, t := range w.def.Tokens {
		if t.Contract == addr {
			return t, true
		}
	}
	return evmToken{}, false
}

type evmLog struct {
	Address string `json:"address"`
	Data    string `json:"data"`
	TxHash  string `json:"transactionHash"`
}

func (w *evmWatcher) blockNumber(ctx context.Context) (int64, error) {
	var hex string
	if err := w.call(ctx, "eth_blockNumber", []any{}, &hex); err != nil {
		return 0, err
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(hex, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("bad block number %q", hex)
	}
	return n.Int64(), nil
}

// call is a minimal JSON-RPC 2.0 client. Hand-written net/http, like every other
// external client in this repo (manager.go, googleauth.go, admin's tmdb.go) —
// there is no web3 dependency here and there does not need to be.
func (w *evmWatcher) call(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.rpc, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rpc %s: http %d", method, resp.StatusCode)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return err
	}
	if env.Error != nil {
		return fmt.Errorf("rpc %s: %d %s", method, env.Error.Code, env.Error.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

func hexBlock(n int64) string { return "0x" + big.NewInt(n).Text(16) }

// padAddressTopic renders an address as a 32-byte log topic (left-zero-padded).
func padAddressTopic(addr string) string {
	a := strings.ToLower(strings.TrimPrefix(addr, "0x"))
	return "0x" + strings.Repeat("0", 64-len(a)) + a
}

func parseBlockCursor(c string) (int64, bool) {
	if c == "" {
		return 0, false
	}
	n, ok := new(big.Int).SetString(c, 10)
	if !ok {
		return 0, false
	}
	return n.Int64(), true
}

func formatBlockCursor(n int64) string { return big.NewInt(n).String() }

// scaleDecimal renders a raw token amount as an exact decimal string, dividing
// by 10^decimals through STRING manipulation rather than arithmetic. There is no
// float anywhere in this path on purpose: the result is compared for exact
// equality against a DECIMAL column, and that comparison decides who gets paid.
func scaleDecimal(v *big.Int, decimals int) string {
	s := v.String()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if decimals <= 0 {
		return sign(neg) + s
	}
	if len(s) <= decimals {
		s = strings.Repeat("0", decimals-len(s)+1) + s
	}
	intPart, frac := s[:len(s)-decimals], s[len(s)-decimals:]
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		return sign(neg) + intPart
	}
	return sign(neg) + intPart + "." + frac
}

func sign(neg bool) string {
	if neg {
		return "-"
	}
	return ""
}
