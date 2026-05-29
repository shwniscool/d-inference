import { NextRequest, NextResponse } from "next/server";

// Proxy for POST /v1/billing/auto-topup/setup-intent. Returns a Stripe
// SetupIntent client_secret the browser uses with Stripe.js to collect and
// confirm a card. Privy-only — forwards the privy-token cookie fallback.

const DEFAULT_COORD = process.env.NEXT_PUBLIC_COORDINATOR_URL || "https://api.darkbloom.dev";

export async function POST(req: NextRequest) {
  const coordUrl = req.headers.get("x-coordinator-url") || DEFAULT_COORD;

  let authHeader = req.headers.get("authorization") || "";
  if (!authHeader) {
    const privyToken = req.cookies.get("privy-token")?.value;
    if (privyToken) authHeader = `Bearer ${privyToken}`;
  }

  const res = await fetch(`${coordUrl}/v1/billing/auto-topup/setup-intent`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(authHeader ? { Authorization: authHeader } : {}),
    },
  });
  if (!res.ok) {
    const text = await res.text();
    return NextResponse.json({ error: text }, { status: res.status });
  }
  return NextResponse.json(await res.json().catch(() => ({})));
}
