import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  fetchAutoTopUp,
  updateAutoTopUp,
  createAutoTopUpSetupIntent,
  confirmAutoTopUpCard,
} from "@/lib/api";

// Auto Top-Up client tests. These cover that each API client function calls
// the right proxy URL with the right body, and that error responses — both the
// structured { error: { message } } shape and the proxy's { error: "<json>" }
// wrapping — are surfaced cleanly so the UI can show a toast.

function jsonResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
    text: () => Promise.resolve(JSON.stringify(body)),
    headers: new Headers(),
  } as unknown as Response;
}

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  localStorage.clear();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("fetchAutoTopUp", () => {
  it("GETs /api/payments/auto-topup", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ enabled: true, has_saved_card: true, card_brand: "visa", card_last4: "4242" })
    );
    const c = await fetchAutoTopUp();
    expect(fetchMock).toHaveBeenCalledWith("/api/payments/auto-topup", expect.any(Object));
    expect(c.card_last4).toBe("4242");
  });

  it("throws on non-2xx responses", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({}, 500));
    await expect(fetchAutoTopUp()).rejects.toThrow(/500/);
  });
});

describe("updateAutoTopUp", () => {
  it("PUTs the config to /api/payments/auto-topup", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ enabled: true, has_saved_card: true }));
    await updateAutoTopUp({
      enabled: true,
      payment_method: "stripe",
      threshold_micro_usd: 5_000_000,
      amount_micro_usd: 10_000_000,
    });
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/payments/auto-topup");
    expect(opts.method).toBe("PUT");
    expect(JSON.parse(opts.body)).toEqual({
      enabled: true,
      payment_method: "stripe",
      threshold_micro_usd: 5_000_000,
      amount_micro_usd: 10_000_000,
    });
  });

  it("surfaces a structured { error: { message } } validation error", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: { type: "no_card", message: "add a card before enabling" } }, 400)
    );
    await expect(
      updateAutoTopUp({ enabled: true, payment_method: "stripe" })
    ).rejects.toThrow(/add a card before enabling/);
  });

  it("unwraps the proxy's { error: '<json string>' } wrapping", async () => {
    // The Next proxy forwards the upstream body verbatim as a string.
    fetchMock.mockResolvedValueOnce(
      jsonResponse(
        { error: JSON.stringify({ error: { type: "no_card", message: "no saved card on file" } }) },
        400
      )
    );
    await expect(
      updateAutoTopUp({ enabled: true, payment_method: "stripe" })
    ).rejects.toThrow(/no saved card on file/);
  });
});

describe("createAutoTopUpSetupIntent", () => {
  it("POSTs to /api/payments/auto-topup/setup-intent", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ setup_intent_id: "seti_1", client_secret: "seti_1_secret", customer_id: "cus_1" })
    );
    const intent = await createAutoTopUpSetupIntent();
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/payments/auto-topup/setup-intent");
    expect(opts.method).toBe("POST");
    expect(intent.client_secret).toBe("seti_1_secret");
  });
});

describe("confirmAutoTopUpCard", () => {
  it("POSTs setup_intent_id to /api/payments/auto-topup/confirm", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ enabled: false, has_saved_card: true }));
    const c = await confirmAutoTopUpCard("seti_1");
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/payments/auto-topup/confirm");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual({ setup_intent_id: "seti_1" });
    expect(c.has_saved_card).toBe(true);
  });
});
