package main

import "context"

// observedPayment is one incoming transfer a watcher saw on-chain, already
// normalised out of the token's raw integer units.
//
// Amount is a decimal string in HUMAN units ("5.000123"), scaled by the token's
// own decimals by the watcher that produced it — which is the whole reason this
// type exists. USDT is 6 decimals on Ethereum and Tron but 18 on BSC, and that
// difference must die at the edge: everything above this line compares amounts
// as exact decimals and would otherwise be off by a factor of 10^12.
type observedPayment struct {
	Chain  string
	Token  string
	Amount string
	TxHash string
}

// chainWatcher is one crypto rail. The seam mirrors admin's SubtitleProvider:
// implementations are registered only when configured, and an unconfigured rail
// is simply absent rather than failing.
//
// Note what is NOT here: minting an invoice's amount. Reserving a unique amount
// needs the database's unique index to arbitrate between concurrent callers, so
// it belongs to billingService. A watcher only reports where to pay, which
// namespace it shares, and what it has seen.
type chainWatcher interface {
	// Name is the stable identifier stored in payment_invoices.chain and used as
	// the billing_chain_cursors key ("base", "bsc", "tron", …).
	Name() string
	// Label is the Vietnamese display name for the chain picker.
	Label() string
	// LockNamespace groups rails whose invoices share one amount-reservation
	// space. Every EVM chain returns "evm": one receive address serves all of
	// them, so no two open EVM invoices may reserve the same amount — which is
	// exactly what lets a payment be credited no matter which EVM chain the user
	// actually paid on.
	LockNamespace() string
	// PayTo is the single receive address for this rail.
	PayTo() string
	// Scan reports transfers observed since cursor, and returns the cursor to
	// resume from. It returns at most one chunk of work per call, so a cold start
	// catches up over successive ticks instead of blocking one for minutes.
	//
	// wantPayments is false when no invoice in this namespace is currently
	// payable. The watcher must then still advance the cursor (so the next
	// invoice does not trigger a huge rescan) but may skip the expensive log
	// query. This is safe because an invoice can only be paid after it exists,
	// and it exists before the tick that observes its payment.
	Scan(ctx context.Context, cursor string, wantPayments bool) (obs []observedPayment, next string, err error)
}
