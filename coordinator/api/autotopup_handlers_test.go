package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAutoTopupStripe returns an httptest server implementing the minimal
// Stripe endpoints the auto top-up card-setup flow touches.
func fakeAutoTopupStripe(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/customers" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"cus_test"}`))
		case r.URL.Path == "/v1/setup_intents" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"seti_test","client_secret":"seti_test_secret","status":"requires_payment_method"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/setup_intents/") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"seti_test","status":"succeeded","customer":"cus_test","payment_method":"pm_test"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/payment_methods/") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"card":{"brand":"visa","last4":"4242"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAutoTopupGetRequiresAuth(t *testing.T) {
	srv, _ := stripePayoutsTestServer(t, true, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/billing/auto-topup", nil)
	w := httptest.NewRecorder()
	srv.handleGetAutoTopup(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", w.Code)
	}
}

func TestAutoTopupGetReturnsEmptyByDefault(t *testing.T) {
	srv, st := stripePayoutsTestServer(t, true, nil)
	user := seedUser(t, st, "acct-at-1", "a@example.com")

	req := withPrivyUser(httptest.NewRequest(http.MethodGet, "/v1/billing/auto-topup", nil), user)
	w := httptest.NewRecorder()
	srv.handleGetAutoTopup(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["enabled"].(bool) {
		t.Errorf("enabled = true, want false by default")
	}
	if resp["has_saved_card"].(bool) {
		t.Errorf("has_saved_card = true, want false")
	}
}

func TestAutoTopupEnableRejectedWithoutCard(t *testing.T) {
	fake := fakeAutoTopupStripe(t)
	srv, st := stripePayoutsTestServer(t, false, fake)
	user := seedUser(t, st, "acct-at-2", "b@example.com")

	body := `{"enabled":true,"threshold_micro_usd":1000000,"amount_micro_usd":10000000,"payment_method":"stripe"}`
	req := withPrivyUser(httptest.NewRequest(http.MethodPut, "/v1/billing/auto-topup", strings.NewReader(body)), user)
	w := httptest.NewRecorder()
	srv.handleSetAutoTopup(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s, want 400 (no saved card)", w.Code, w.Body.String())
	}
}

func TestAutoTopupSetupAndConfirmFlow(t *testing.T) {
	fake := fakeAutoTopupStripe(t)
	srv, st := stripePayoutsTestServer(t, false, fake)
	user := seedUser(t, st, "acct-at-3", "c@example.com")

	// 1. setup-intent → creates a customer and returns a client secret.
	req := withPrivyUser(httptest.NewRequest(http.MethodPost, "/v1/billing/auto-topup/setup-intent", nil), user)
	w := httptest.NewRecorder()
	srv.handleAutoTopupSetupIntent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("setup-intent got %d: %s", w.Code, w.Body.String())
	}
	var setupResp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &setupResp)
	if setupResp["client_secret"] != "seti_test_secret" {
		t.Errorf("client_secret = %v", setupResp["client_secret"])
	}
	if got, _ := st.GetUserByAccountID(user.AccountID); got.StripeCustomerID != "cus_test" {
		t.Errorf("customer id = %q, want cus_test", got.StripeCustomerID)
	}

	// 2. confirm → persists the saved card from the SetupIntent.
	refreshed, _ := st.GetUserByAccountID(user.AccountID)
	confirmReq := withPrivyUser(httptest.NewRequest(http.MethodPost, "/v1/billing/auto-topup/confirm",
		strings.NewReader(`{"setup_intent_id":"seti_test"}`)), refreshed)
	cw := httptest.NewRecorder()
	srv.handleAutoTopupConfirm(cw, confirmReq)
	if cw.Code != http.StatusOK {
		t.Fatalf("confirm got %d: %s", cw.Code, cw.Body.String())
	}
	saved, _ := st.GetUserByAccountID(user.AccountID)
	if saved.StripeDefaultPaymentMethodID != "pm_test" {
		t.Errorf("payment method = %q, want pm_test", saved.StripeDefaultPaymentMethodID)
	}
	if saved.StripePaymentMethodLast4 != "4242" {
		t.Errorf("last4 = %q, want 4242", saved.StripePaymentMethodLast4)
	}

	// 3. now enabling succeeds.
	enableReq := withPrivyUser(httptest.NewRequest(http.MethodPut, "/v1/billing/auto-topup",
		strings.NewReader(`{"enabled":true,"threshold_micro_usd":1000000,"amount_micro_usd":10000000,"payment_method":"stripe"}`)), saved)
	ew := httptest.NewRecorder()
	srv.handleSetAutoTopup(ew, enableReq)
	if ew.Code != http.StatusOK {
		t.Fatalf("enable got %d: %s", ew.Code, ew.Body.String())
	}
	cfg, _ := st.GetAutoTopupConfig(user.AccountID)
	if cfg == nil || !cfg.Enabled {
		t.Fatalf("config = %+v, want enabled", cfg)
	}
}

func TestAutoTopupSetInvalidConfigRejected(t *testing.T) {
	fake := fakeAutoTopupStripe(t)
	srv, st := stripePayoutsTestServer(t, false, fake)
	user := seedUser(t, st, "acct-at-4", "d@example.com")
	// Give them a saved card so the no-card guard doesn't fire first.
	if err := st.SetUserStripeCustomer(user.AccountID, "cus_test", "pm_test", "visa", "4242"); err != nil {
		t.Fatal(err)
	}
	refreshed, _ := st.GetUserByAccountID(user.AccountID)

	// amount ($200) exceeds the max single cap ($100) → 400.
	body := `{"enabled":true,"threshold_micro_usd":1000000,"amount_micro_usd":200000000,"max_single_micro_usd":100000000,"payment_method":"stripe"}`
	req := withPrivyUser(httptest.NewRequest(http.MethodPut, "/v1/billing/auto-topup", strings.NewReader(body)), refreshed)
	w := httptest.NewRecorder()
	srv.handleSetAutoTopup(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s, want 400", w.Code, w.Body.String())
	}
}
