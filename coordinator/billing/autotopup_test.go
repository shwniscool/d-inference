package billing

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eigeninference/d-inference/coordinator/store"
)

// newAutoTopupFixture builds a MemoryStore with a single user who has a saved
// Stripe card, plus an httptest server standing in for the Stripe API. The
// handler returns a succeeded PaymentIntent unless overridden.
func newAutoTopupFixture(t *testing.T, stripeHandler http.HandlerFunc) (*AutoTopupManager, store.Store) {
	t.Helper()

	st := store.NewMemory(store.Config{})
	if err := st.CreateUser(&store.User{AccountID: "acct-1", PrivyUserID: "did:privy:1", Email: "u@example.com"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.SetUserStripeCustomer("acct-1", "cus_123", "pm_123", "visa", "4242"); err != nil {
		t.Fatalf("set customer: %v", err)
	}

	if stripeHandler == nil {
		stripeHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"pi_test_123","status":"succeeded"}`))
		}
	}
	srv := httptest.NewServer(stripeHandler)
	t.Cleanup(srv.Close)

	prev := stripeAPIBase
	stripeAPIBase = srv.URL
	t.Cleanup(func() { stripeAPIBase = prev })

	proc := NewStripeProcessor("sk_test", "whsec_test", "", "", silentLogger())
	mgr := NewAutoTopupManager(st, proc, silentLogger())
	return mgr, st
}

// enableAutoTopup writes an enabled config with the given threshold/amount.
func enableAutoTopup(t *testing.T, st store.Store, threshold, amount int64) {
	t.Helper()
	cfg := &store.AutoTopupConfig{
		AccountID:         "acct-1",
		Enabled:           true,
		ThresholdMicroUSD: threshold,
		AmountMicroUSD:    amount,
		PaymentMethod:     "stripe",
	}
	ApplyAutoTopupDefaults(cfg)
	if err := st.SetAutoTopupConfig(cfg); err != nil {
		t.Fatalf("set config: %v", err)
	}
}

func TestAutoTopup_ChargesAndCredits(t *testing.T) {
	mgr, st := newAutoTopupFixture(t, nil)
	enableAutoTopup(t, st, 1_000_000, 10_000_000) // threshold $1, amount $10

	// Balance below threshold: $0.50.
	if err := st.Credit("acct-1", 500_000, store.LedgerDeposit, ""); err != nil {
		t.Fatal(err)
	}

	if err := mgr.triggerAutoTopup("acct-1"); err != nil {
		t.Fatalf("triggerAutoTopup: %v", err)
	}

	if got := st.GetBalance("acct-1"); got != 10_500_000 {
		t.Fatalf("balance = %d, want 10_500_000 (0.5 + 10)", got)
	}
	last, _ := st.LastAutoTopupEvent("acct-1")
	if last == nil || last.Status != store.AutoTopupSucceeded {
		t.Fatalf("last event = %+v, want succeeded", last)
	}
	if last.ExternalID != "pi_test_123" {
		t.Fatalf("external id = %q, want pi_test_123", last.ExternalID)
	}
}

func TestAutoTopup_SkipsWhenAboveThreshold(t *testing.T) {
	mgr, st := newAutoTopupFixture(t, nil)
	enableAutoTopup(t, st, 1_000_000, 10_000_000)
	if err := st.Credit("acct-1", 5_000_000, store.LedgerDeposit, ""); err != nil { // $5 > $1
		t.Fatal(err)
	}

	err := mgr.triggerAutoTopup("acct-1")
	if !errors.Is(err, ErrAutoTopupSkipped) {
		t.Fatalf("err = %v, want ErrAutoTopupSkipped", err)
	}
	if got := st.GetBalance("acct-1"); got != 5_000_000 {
		t.Fatalf("balance changed to %d, want untouched 5_000_000", got)
	}
}

func TestAutoTopup_SkipsWhenDisabled(t *testing.T) {
	mgr, _ := newAutoTopupFixture(t, nil)
	// No config at all.
	err := mgr.triggerAutoTopup("acct-1")
	if !errors.Is(err, ErrAutoTopupSkipped) {
		t.Fatalf("err = %v, want ErrAutoTopupSkipped", err)
	}
}

func TestAutoTopup_RespectsCooldown(t *testing.T) {
	mgr, st := newAutoTopupFixture(t, nil)
	enableAutoTopup(t, st, 1_000_000, 10_000_000)
	// A recent prior event within the 60s default cooldown.
	if _, err := st.RecordAutoTopupEvent(&store.AutoTopupEvent{
		AccountID: "acct-1", AmountMicroUSD: 10_000_000, Status: store.AutoTopupSucceeded,
	}); err != nil {
		t.Fatal(err)
	}

	err := mgr.triggerAutoTopup("acct-1")
	if !errors.Is(err, ErrAutoTopupSkipped) {
		t.Fatalf("err = %v, want skipped (cooldown)", err)
	}
}

func TestAutoTopup_Enforces24hCap(t *testing.T) {
	mgr, st := newAutoTopupFixture(t, nil)
	// amount $10, but 24h cap only $15 with $10 already charged → would hit $20.
	cfg := &store.AutoTopupConfig{
		AccountID: "acct-1", Enabled: true,
		ThresholdMicroUSD: 1_000_000, AmountMicroUSD: 10_000_000,
		PaymentMethod: "stripe", MaxPer24hMicroUSD: 15_000_000,
		MaxSingleMicroUSD: DefaultMaxSingleMicroUSD, CooldownSeconds: 0,
	}
	if err := st.SetAutoTopupConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// Prior succeeded top-up of $10, dated outside the cooldown but inside 24h.
	if _, err := st.RecordAutoTopupEvent(&store.AutoTopupEvent{
		AccountID: "acct-1", AmountMicroUSD: 10_000_000, Status: store.AutoTopupSucceeded,
		CreatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	err := mgr.triggerAutoTopup("acct-1")
	if !errors.Is(err, ErrAutoTopupSkipped) {
		t.Fatalf("err = %v, want skipped (24h cap)", err)
	}
}

func TestAutoTopup_SkipsWithoutSavedCard(t *testing.T) {
	mgr, st := newAutoTopupFixture(t, nil)
	// Clear the saved card.
	if err := st.SetUserStripeCustomer("acct-1", "cus_123", "", "", ""); err != nil {
		t.Fatal(err)
	}
	enableAutoTopup(t, st, 1_000_000, 10_000_000)

	err := mgr.triggerAutoTopup("acct-1")
	if !errors.Is(err, ErrAutoTopupSkipped) {
		t.Fatalf("err = %v, want skipped (no card)", err)
	}
}

func TestAutoTopup_ChargeDeclinedMarksFailed(t *testing.T) {
	mgr, st := newAutoTopupFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"type":"card_error","code":"card_declined","decline_code":"insufficient_funds","message":"Your card was declined."}}`))
	})
	enableAutoTopup(t, st, 1_000_000, 10_000_000)
	if err := st.Credit("acct-1", 500_000, store.LedgerDeposit, ""); err != nil {
		t.Fatal(err)
	}

	err := mgr.triggerAutoTopup("acct-1")
	if err == nil || errors.Is(err, ErrAutoTopupSkipped) {
		t.Fatalf("err = %v, want a real charge error", err)
	}
	// Balance must NOT be credited on a declined charge.
	if got := st.GetBalance("acct-1"); got != 500_000 {
		t.Fatalf("balance = %d, want untouched 500_000", got)
	}
	last, _ := st.LastAutoTopupEvent("acct-1")
	if last == nil || last.Status != store.AutoTopupFailed {
		t.Fatalf("last event = %+v, want failed", last)
	}
}

func TestAutoTopup_AmountCappedToMaxSingle(t *testing.T) {
	var capturedAmount atomic.Int64
	mgr, st := newAutoTopupFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		// amount is in cents.
		if v := r.FormValue("amount"); v != "" {
			var cents int64
			for _, c := range v {
				cents = cents*10 + int64(c-'0')
			}
			capturedAmount.Store(cents)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"pi_capped","status":"succeeded"}`))
	})
	// amount $80 but max single $50 → charge should be capped at $50 (5000 cents).
	cfg := &store.AutoTopupConfig{
		AccountID: "acct-1", Enabled: true,
		ThresholdMicroUSD: 1_000_000, AmountMicroUSD: 80_000_000,
		PaymentMethod: "stripe", MaxPer24hMicroUSD: 100_000_000,
		MaxSingleMicroUSD: 50_000_000, CooldownSeconds: 0,
	}
	if err := st.SetAutoTopupConfig(cfg); err != nil {
		t.Fatal(err)
	}

	if err := mgr.triggerAutoTopup("acct-1"); err != nil {
		t.Fatalf("triggerAutoTopup: %v", err)
	}
	if got := capturedAmount.Load(); got != 5000 {
		t.Fatalf("charged %d cents, want 5000 ($50 cap)", got)
	}
	if got := st.GetBalance("acct-1"); got != 50_000_000 {
		t.Fatalf("credited %d, want 50_000_000", got)
	}
}

func TestAutoTopup_InFlightGuardPreventsConcurrentCharges(t *testing.T) {
	var charges atomic.Int64
	release := make(chan struct{})
	mgr, st := newAutoTopupFixture(t, func(w http.ResponseWriter, r *http.Request) {
		charges.Add(1)
		<-release // hold the charge open so both goroutines overlap
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"pi_concurrent","status":"succeeded"}`))
	})
	enableAutoTopup(t, st, 1_000_000, 10_000_000)
	if err := st.Credit("acct-1", 500_000, store.LedgerDeposit, ""); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); mgr.MaybeAutoTopup("acct-1") }()
	}
	// Give the goroutines time to contend on the in-flight guard, then release.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := charges.Load(); got != 1 {
		t.Fatalf("Stripe charged %d times, want exactly 1 (in-flight guard)", got)
	}
}

func TestValidateAutoTopupConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     store.AutoTopupConfig
		wantErr bool
	}{
		{"disabled is always valid", store.AutoTopupConfig{Enabled: false}, false},
		{"valid", store.AutoTopupConfig{Enabled: true, PaymentMethod: "stripe", AmountMicroUSD: 10_000_000, MaxSingleMicroUSD: 100_000_000, MaxPer24hMicroUSD: 50_000_000}, false},
		{"amount below min", store.AutoTopupConfig{Enabled: true, PaymentMethod: "stripe", AmountMicroUSD: 100, MaxSingleMicroUSD: 100_000_000, MaxPer24hMicroUSD: 50_000_000}, true},
		{"amount exceeds single cap", store.AutoTopupConfig{Enabled: true, PaymentMethod: "stripe", AmountMicroUSD: 200_000_000, MaxSingleMicroUSD: 100_000_000, MaxPer24hMicroUSD: 500_000_000}, true},
		{"24h cap below amount", store.AutoTopupConfig{Enabled: true, PaymentMethod: "stripe", AmountMicroUSD: 10_000_000, MaxSingleMicroUSD: 100_000_000, MaxPer24hMicroUSD: 5_000_000}, true},
		{"non-stripe method", store.AutoTopupConfig{Enabled: true, PaymentMethod: "solana", AmountMicroUSD: 10_000_000, MaxSingleMicroUSD: 100_000_000, MaxPer24hMicroUSD: 50_000_000}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			err := ValidateAutoTopupConfig(&cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateAutoTopupConfig_PlatformCeilings(t *testing.T) {
	base := func() store.AutoTopupConfig {
		return store.AutoTopupConfig{Enabled: true, PaymentMethod: "stripe", AmountMicroUSD: 10_000_000,
			MaxSingleMicroUSD: 100_000_000, MaxPer24hMicroUSD: 50_000_000}
	}
	// Single cap above platform ceiling rejected.
	c := base()
	c.MaxSingleMicroUSD = PlatformMaxSingleMicroUSD + 1
	if err := ValidateAutoTopupConfig(&c); err == nil {
		t.Error("expected rejection of single cap above platform ceiling")
	}
	// 24h cap above platform ceiling rejected.
	c = base()
	c.MaxPer24hMicroUSD = PlatformMax24hMicroUSD + 1
	if err := ValidateAutoTopupConfig(&c); err == nil {
		t.Error("expected rejection of 24h cap above platform ceiling")
	}
	// Sub-cent amount rejected.
	c = base()
	c.AmountMicroUSD = 10_000_001
	if err := ValidateAutoTopupConfig(&c); err == nil {
		t.Error("expected rejection of sub-cent amount")
	}
}

func TestValidateNotifyWebhookURL(t *testing.T) {
	tests := []struct {
		url     string
		wantErr bool
	}{
		{"", false},
		{"http://example.com/hook", true},        // not https
		{"https://localhost/hook", true},         // loopback
		{"https://127.0.0.1/hook", true},         // loopback IP
		{"https://169.254.169.254/latest", true}, // cloud metadata (link-local)
		{"https://10.0.0.5/hook", true},          // RFC1918 private
		{"https://192.168.1.1/hook", true},       // RFC1918 private
		{"not a url at all ::::", true},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			err := validateNotifyWebhookURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateNotifyWebhookURL(%q) err = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestIsDisallowedDialIP(t *testing.T) {
	disallowed := []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.0.1", "169.254.169.254", "0.0.0.0"}
	for _, s := range disallowed {
		if !isDisallowedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be disallowed", s)
		}
	}
	if isDisallowedDialIP(net.ParseIP("8.8.8.8")) {
		t.Error("8.8.8.8 (public) should be allowed")
	}
}

func TestApplyAutoTopupDefaults(t *testing.T) {
	cfg := &store.AutoTopupConfig{}
	ApplyAutoTopupDefaults(cfg)
	if cfg.PaymentMethod != "stripe" {
		t.Errorf("payment method = %q, want stripe", cfg.PaymentMethod)
	}
	if cfg.MaxPer24hMicroUSD != DefaultMaxPer24hMicroUSD {
		t.Errorf("24h cap = %d, want %d", cfg.MaxPer24hMicroUSD, DefaultMaxPer24hMicroUSD)
	}
	if cfg.MaxSingleMicroUSD != DefaultMaxSingleMicroUSD {
		t.Errorf("single cap = %d, want %d", cfg.MaxSingleMicroUSD, DefaultMaxSingleMicroUSD)
	}
	if cfg.CooldownSeconds != DefaultCooldownSeconds {
		t.Errorf("cooldown = %d, want %d", cfg.CooldownSeconds, DefaultCooldownSeconds)
	}
}
