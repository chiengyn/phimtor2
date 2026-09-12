package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"
)

// Amount-reservation namespaces. Every EVM chain shares one, because one address
// serves them all (see chains.go).
const (
	lockNSEVM  = "evm"
	lockNSTron = "tron"
)

// HOW AN INVOICE IS IDENTIFIED
//
// Not by a per-invoice address — by a unique EXACT AMOUNT against one shared
// receive address. The price is nudged by a few units of dust, and the database's
// UNIQUE(lock_ns, amount_lock) index arbitrates so no two open invoices in a
// namespace can ever claim the same amount.
//
// amountDecimals is 6 because that is the NARROWEST precision among the tokens
// we watch (USDT/USDC are 6 decimals; BSC-USD is 18). An amount at 6 decimals is
// exactly representable on all of them, which is what allows one amount to
// identify an invoice across chains whose tokens disagree about decimals.
//
// maxDust bounds the surcharge: 10000 steps of 1e-6 is at most $0.009999, i.e.
// under one US cent. That is the price of not holding private keys.
const (
	amountDecimals = 6
	amountScale    = 1_000_000 // 10^amountDecimals
	maxDust        = 10_000
	reserveTries   = 12
)

// stableUSDRate is the USD price of one unit of every token we accept. All of
// them are USD stablecoins, so it is 1 and there is no price feed to fail, no
// API key, and no rate-move arbitrage window.
//
// This is the seam where a real feed goes when a non-stable asset (native ETH,
// BTC, XMR) is added — and note what else must arrive with it: the rate would
// then have to be SNAPSHOTTED onto the invoice row (rate_usd already exists for
// this) so a price move mid-payment cannot change what was owed.
const stableUSDRate = 1.0

// plan is one purchasable product.
type plan struct {
	Code  string
	Name  string
	Kind  string // "pass" | "title"
	Days  int    // pass only
	Cents int
}

type billingService struct {
	store         *Store
	watcherByName map[string]chainWatcher
	order         []string // stable display order
	ttlMin        int
	poll          time.Duration
	vndRate       int
	plans         []plan
}

func newBillingService(store *Store, cfg Config) *billingService {
	b := &billingService{
		store:         store,
		watcherByName: map[string]chainWatcher{},
		ttlMin:        cfg.BillingInvoiceTTLMin,
		poll:          time.Duration(cfg.BillingPollIntervalSec) * time.Second,
		vndRate:       cfg.BillingVNDPerUSD,
		plans: []plan{
			{Code: "pass30", Name: "Gói 30 ngày", Kind: "pass", Days: 30, Cents: cfg.BillingPass30Cents},
			{Code: "pass365", Name: "Gói 1 năm", Kind: "pass", Days: 365, Cents: cfg.BillingPass365Cents},
			{Code: "title", Name: "Mở khoá vĩnh viễn phim này", Kind: "title", Cents: cfg.BillingTitleCents},
		},
	}

	evmAddr := strings.TrimSpace(cfg.BillingEVMAddress)
	if evmAddr != "" {
		for _, name := range strings.Split(cfg.BillingEVMChains, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			def, ok := evmChainDefs[name]
			if !ok {
				log.Printf("billing: unknown EVM chain %q in BILLING_EVM_CHAINS — skipping", name)
				continue
			}
			b.watcherByName[name] = newEVMWatcher(name, def, evmAddr)
			b.order = append(b.order, name)
		}
	}
	return b
}

func (b *billingService) enabled() bool { return b != nil && len(b.watcherByName) > 0 }

// chains lists the enabled rails in display order.
func (b *billingService) chains() []string {
	if b == nil {
		return nil
	}
	return append([]string(nil), b.order...)
}

func (b *billingService) watcher(name string) (chainWatcher, bool) {
	if b == nil {
		return nil, false
	}
	w, ok := b.watcherByName[name]
	return w, ok
}

func (b *billingService) planByCode(code string) (plan, bool) {
	for _, p := range b.plans {
		if p.Code == code {
			return p, true
		}
	}
	return plan{}, false
}

// vnd converts US cents to whole VND, for display beside the USD price.
func (b *billingService) vnd(cents int) int {
	if b == nil || b.vndRate <= 0 {
		return 0
	}
	return cents * b.vndRate / 100
}

// createInvoice reserves an amount and writes the invoice row.
//
// The reservation is a retry loop, not a lock: it picks random dust and lets the
// UNIQUE(lock_ns, amount_lock) index reject a clash. Under concurrency a
// duplicate is EXPECTED, not an error — it simply means another invoice holds
// that amount right now, so we roll different dust and try again.
func (b *billingService) createInvoice(ctx context.Context, userID int64, p plan, titleID *int64, chain string) (*Invoice, error) {
	w, ok := b.watcher(chain)
	if !ok {
		return nil, fmt.Errorf("unknown chain %q", chain)
	}
	if p.Cents <= 0 {
		return nil, fmt.Errorf("plan %q has no price", p.Code)
	}

	for try := 0; try < reserveTries; try++ {
		dust, err := randInt(maxDust)
		if err != nil {
			return nil, err
		}
		amount := formatAmount(p.Cents, dust)
		inv := &Invoice{
			Ref:            newRef(),
			UserID:         userID,
			Kind:           p.Kind,
			PlanCode:       p.Code,
			TitleID:        titleID,
			AmountUSDCents: p.Cents,
			Chain:          w.Name(),
			PayTo:          w.PayTo(),
			PayAmount:      amount,
			Token:          "USD stablecoin",
			Status:         "pending",
		}
		err = b.store.CreateInvoice(ctx, inv, w.LockNamespace(), b.ttlMin)
		if err == nil {
			return inv, nil
		}
		if !isDuplicateKey(err) {
			return nil, err
		}
	}
	// Every attempt collided. With 10k slots this means the namespace is
	// saturated with open invoices, which is a capacity problem, not a bug.
	return nil, fmt.Errorf("could not reserve a unique amount after %d tries", reserveTries)
}

// run is the poll loop: expire what has timed out, then scan each chain and
// credit what matches. Shaped like the admin's torrent harvester — a ticker,
// disabled when the interval is <= 0, started from main.
func (b *billingService) run(ctx context.Context) {
	if !b.enabled() || b.poll <= 0 {
		log.Printf("billing: poller disabled")
		return
	}
	log.Printf("billing: polling %s every %s", strings.Join(b.order, ", "), b.poll)
	t := time.NewTicker(b.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.tick(ctx)
		}
	}
}

func (b *billingService) tick(ctx context.Context) {
	if n, err := b.store.ExpireInvoices(ctx); err != nil {
		log.Printf("billing: expire: %v", err)
	} else if n > 0 {
		log.Printf("billing: expired %d invoice(s)", n)
	}
	for _, name := range b.order {
		if err := b.scanChain(ctx, b.watcherByName[name]); err != nil {
			// A dead RPC endpoint must not kill the loop: the next tick retries,
			// and the cursor was not advanced past anything unread.
			log.Printf("billing: scan %s: %v", name, err)
		}
	}
}

func (b *billingService) scanChain(ctx context.Context, w chainWatcher) error {
	ns := w.LockNamespace()
	pending, err := b.store.CountPendingInvoices(ctx, ns)
	if err != nil {
		return err
	}
	cursor, err := b.store.ChainCursor(ctx, w.Name())
	if err != nil {
		return err
	}
	obs, next, err := w.Scan(ctx, cursor, pending > 0)
	if err != nil {
		return err
	}
	for _, ob := range obs {
		inv, err := b.store.MatchPendingInvoice(ctx, ns, ob.Amount)
		if err != nil {
			return err
		}
		if inv == nil {
			// Nothing owes this exact amount: a stray transfer, a double payment,
			// or a mistyped amount. It is real money, so say so loudly — the
			// operator resolves it from the admin payments view.
			log.Printf("billing: UNMATCHED %s %s on %s (tx %s) — needs manual handling",
				ob.Amount, ob.Token, ob.Chain, ob.TxHash)
			continue
		}
		if err := b.settle(ctx, inv, ob); err != nil {
			return err
		}
	}
	if next != cursor {
		return b.store.SetChainCursor(ctx, w.Name(), next)
	}
	return nil
}

// settle credits a paid invoice and grants the entitlement, atomically.
// Idempotency lives in the store's `AND status='pending'` guard, so a repeated
// observation, a restart mid-settle, or a rewound cursor all collapse to a no-op.
func (b *billingService) settle(ctx context.Context, inv *Invoice, ob observedPayment) error {
	days := 0
	if p, ok := b.planByCode(inv.PlanCode); ok {
		days = p.Days
	}
	ok, err := b.store.SettleInvoice(ctx, inv, ob, days)
	if err != nil {
		return err
	}
	if !ok {
		return nil // already settled by an earlier observation
	}
	log.Printf("billing: settled invoice %s (user %d, %s) with %s %s on %s tx %s",
		inv.Ref, inv.UserID, inv.PlanCode, ob.Amount, ob.Token, ob.Chain, ob.TxHash)
	return nil
}

// formatAmount renders price+dust as an exact decimal string at amountDecimals.
// All integer arithmetic: 1 US cent is 10_000 units of 1e-6.
func formatAmount(cents, dust int) string {
	units := int64(cents)*(amountScale/100) + int64(dust)
	whole := units / amountScale
	frac := units % amountScale
	s := fmt.Sprintf("%d.%0*d", whole, amountDecimals, frac)
	return s
}

func randInt(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}

// newRef is the invoice's public id. Random rather than sequential so an invoice
// URL cannot be guessed or enumerated (the handler still checks ownership).
func newRef() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
