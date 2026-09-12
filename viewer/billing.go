package main

import "strings"

// billingService is the paid 4K tier: it decides whether crypto payment is
// configured at all, and (from the next phase on) mints invoices and watches the
// chains for them.
//
// It is nil-safe and reports enabled() == false when no rail is configured, for
// the same reason googleClient is: resolutionLock consults it on every watch
// page, and an unconfigured deploy must degrade to the pre-billing site rather
// than strand every visitor behind a paywall they cannot pay. That makes
// "unset the env vars" the clean rollback.
//
// Payments are identified by a unique exact amount against ONE receive address
// per chain family, never by a per-invoice derived address, so this service
// holds no key material at all — not a seed, not even an xpub. Keep it that way:
// it is the property that makes watching chains from the web tier safe.
type billingService struct {
	// evmAddress is the single 0x receive address. The same one is valid on
	// every EVM chain, which is why there is one field rather than one per chain.
	evmAddress string
	// evmChains are the enabled EVM chains, e.g. ["base", "arbitrum", "bsc"].
	evmChains []string
	// tronAddress is the T... receive address for USDT-TRC20.
	tronAddress string
}

func newBillingService(cfg Config) *billingService {
	b := &billingService{
		evmAddress:  strings.TrimSpace(cfg.BillingEVMAddress),
		tronAddress: strings.TrimSpace(cfg.BillingTronAddress),
	}
	for _, c := range strings.Split(cfg.BillingEVMChains, ",") {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
			b.evmChains = append(b.evmChains, c)
		}
	}
	return b
}

// enabled reports whether at least one crypto rail is usable. An EVM rail needs
// both an address and at least one chain — either alone can take no money.
func (b *billingService) enabled() bool {
	if b == nil {
		return false
	}
	return (b.evmAddress != "" && len(b.evmChains) > 0) || b.tronAddress != ""
}

// chains lists the enabled rails, for the plans / invoice pages.
func (b *billingService) chains() []string {
	if b == nil {
		return nil
	}
	out := append([]string(nil), b.evmChains...)
	if b.tronAddress != "" {
		out = append(out, "tron")
	}
	return out
}
