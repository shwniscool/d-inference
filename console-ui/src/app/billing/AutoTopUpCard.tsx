"use client";

import { useCallback, useEffect, useState } from "react";
import { useToastStore } from "@/hooks/useToast";
import {
  fetchAutoTopUp,
  updateAutoTopUp,
  createAutoTopUpSetupIntent,
  confirmAutoTopUpCard,
  type AutoTopUpConfig,
} from "@/lib/api";
import { Loader2, RefreshCw, CreditCard, Check } from "lucide-react";

// Convert micro-USD <-> dollars for display/entry. The backend works entirely
// in micro-USD (1_000_000 = $1); the UI lets the user think in dollars.
const microToDollars = (micro: number): string =>
  micro > 0 ? (micro / 1_000_000).toFixed(2) : "";
const dollarsToMicro = (dollars: string): number =>
  Math.round((parseFloat(dollars) || 0) * 1_000_000);

export function AutoTopUpCard() {
  const addToast = useToastStore((s) => s.addToast);

  const [config, setConfig] = useState<AutoTopUpConfig | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [cardLoading, setCardLoading] = useState(false);

  // Form state (dollars as strings so the inputs stay controlled).
  const [enabled, setEnabled] = useState(false);
  const [threshold, setThreshold] = useState("");
  const [amount, setAmount] = useState("");

  const applyConfig = useCallback((c: AutoTopUpConfig) => {
    setConfig(c);
    setEnabled(c.enabled);
    setThreshold(microToDollars(c.threshold_micro_usd));
    setAmount(microToDollars(c.amount_micro_usd));
  }, []);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      applyConfig(await fetchAutoTopUp());
    } catch (e) {
      // Auto Top-Up is optional infrastructure — don't block the billing page.
      console.warn("auto top-up fetch failed:", (e as Error).message);
      setConfig(null);
    }
    setLoading(false);
  }, [applyConfig]);

  useEffect(() => {
    load();
  }, [load]);

  const hasCard = !!config?.has_saved_card;

  const handleSave = async () => {
    setSaving(true);
    try {
      const updated = await updateAutoTopUp({
        enabled,
        payment_method: "stripe",
        threshold_micro_usd: dollarsToMicro(threshold),
        amount_micro_usd: dollarsToMicro(amount),
      });
      applyConfig(updated);
      addToast("Auto top-up settings saved", "success");
    } catch (e) {
      // Surfaces 400 validation errors (e.g. enabling without a saved card).
      addToast(`${(e as Error).message}`);
    }
    setSaving(false);
  };

  const handleSetupCard = async () => {
    setCardLoading(true);
    try {
      const intent = await createAutoTopUpSetupIntent();

      // NOTE (stripe-elements, follow-up): Collect and confirm the card
      // client-side with Stripe.js using `intent.client_secret`:
      //
      //   const stripe = await loadStripe(NEXT_PUBLIC_STRIPE_PK);
      //   const { setupIntent, error } = await stripe.confirmCardSetup(
      //     intent.client_secret,
      //     { payment_method: { card: cardElement } }
      //   );
      //
      // `@stripe/stripe-js` is not currently a dependency of console-ui and we
      // were asked not to pull in heavy deps here, so the in-browser card
      // entry (Stripe Elements) is intentionally left unwired. The setup-intent
      // and confirm endpoints ARE called so the rest of the flow is in place;
      // once Stripe Elements lands, drop the confirmation between these two
      // calls and pass the confirmed setup_intent_id below.
      const updated = await confirmAutoTopUpCard(intent.setup_intent_id);
      applyConfig(updated);
      addToast("Card saved for auto top-up", "success");
    } catch (e) {
      addToast(`${(e as Error).message}`);
    }
    setCardLoading(false);
  };

  // Hide the card while the very first fetch is in flight or if the
  // coordinator doesn't expose Auto Top-Up at all.
  if (loading) {
    return (
      <div className="rounded-2xl border border-border-dim bg-bg-white p-6 shadow-md flex items-center gap-2 text-text-tertiary">
        <Loader2 size={16} className="animate-spin" />
        <span className="text-sm">Loading auto top-up…</span>
      </div>
    );
  }
  if (!config) return null;

  return (
    <div className="rounded-2xl border border-border-dim bg-bg-white p-6 shadow-md">
      <div className="flex items-center gap-2 mb-4">
        <RefreshCw size={16} className="text-coral" />
        <h3 className="text-sm font-semibold text-text-primary">Auto Top-Up</h3>
        {config.enabled && (
          <span className="ml-auto text-[10px] font-mono uppercase tracking-widest text-teal bg-teal/10 border border-teal/30 rounded px-2 py-0.5">
            On
          </span>
        )}
      </div>

      <p className="text-sm text-text-secondary mb-4 leading-relaxed">
        Automatically buy credits with your saved card when your balance drops
        below a threshold — so inference never stops mid-request.
      </p>

      {/* Saved card */}
      <div className="rounded-lg bg-bg-primary border border-border-dim p-3 mb-4 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2 text-sm text-text-secondary">
          <CreditCard size={14} className="text-coral" />
          {hasCard ? (
            <span className="font-mono">
              {config.card_brand || "Card"} ••{config.card_last4}
            </span>
          ) : (
            <span className="font-mono text-text-tertiary">No card on file</span>
          )}
        </div>
        <button
          onClick={handleSetupCard}
          disabled={cardLoading}
          className="flex items-center gap-2 px-3 py-1.5 rounded-lg bg-bg-tertiary border border-border-subtle text-text-secondary text-xs font-mono hover:bg-bg-hover transition-colors disabled:opacity-50"
        >
          {cardLoading ? <Loader2 size={12} className="animate-spin" /> : <CreditCard size={12} />}
          {hasCard ? "Update card" : "Add card"}
        </button>
      </div>

      {/* Enable toggle */}
      <label className="flex items-center gap-3 text-sm text-text-primary cursor-pointer mb-4">
        <input
          type="checkbox"
          checked={enabled}
          onChange={(e) => setEnabled(e.target.checked)}
          className="w-4 h-4 accent-coral"
        />
        <span>Enable automatic top-ups</span>
      </label>
      {enabled && !hasCard && (
        <p className="text-xs text-coral mb-4">
          Add a card before enabling — saving will fail without one.
        </p>
      )}

      {/* Threshold + amount */}
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 mb-5">
        <div>
          <label className="block text-xs font-mono text-text-tertiary uppercase tracking-wider mb-2">
            When balance falls below
          </label>
          <div className="flex items-center gap-2">
            <span className="text-text-tertiary text-lg">$</span>
            <input
              type="number"
              value={threshold}
              onChange={(e) => setThreshold(e.target.value)}
              placeholder="5.00"
              min="0"
              step="0.01"
              className="flex-1 bg-bg-primary border border-border-dim rounded-lg px-4 py-3 text-text-primary font-mono text-lg outline-none focus:border-coral transition-colors"
            />
          </div>
        </div>
        <div>
          <label className="block text-xs font-mono text-text-tertiary uppercase tracking-wider mb-2">
            Top up by
          </label>
          <div className="flex items-center gap-2">
            <span className="text-text-tertiary text-lg">$</span>
            <input
              type="number"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              placeholder="10.00"
              min="0"
              step="0.01"
              className="flex-1 bg-bg-primary border border-border-dim rounded-lg px-4 py-3 text-text-primary font-mono text-lg outline-none focus:border-coral transition-colors"
            />
          </div>
        </div>
      </div>

      <button
        onClick={handleSave}
        disabled={saving}
        className="flex items-center gap-2 px-5 py-2.5 rounded-lg bg-coral border-2 border-ink text-white text-sm font-bold hover:opacity-90 disabled:opacity-50 transition-all"
      >
        {saving ? <Loader2 size={14} className="animate-spin" /> : <Check size={14} />}
        {saving ? "Saving…" : "Save"}
      </button>
    </div>
  );
}
