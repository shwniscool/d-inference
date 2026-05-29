package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"time"

	"github.com/eigeninference/d-inference/coordinator/store"
)

// Auto top-up automatically replenishes a consumer's balance by charging a
// saved Stripe card off-session when the balance drops below a configured
// threshold, so active workloads are never interrupted by 402s.
//
// Trigger: the API layer calls Service.MaybeAutoTopup(accountID) asynchronously
// after a balance-reducing debit (e.g. the pre-flight reservation). The check
// is best-effort and never blocks or fails the inference request.
//
// Safety rails (issue #231):
//   - rolling 24h cap on total auto top-ups (default $50)
//   - per-charge cap (default $100)
//   - cooldown between top-ups (default 60s)
//   - per-account in-flight guard to prevent rapid-fire concurrent charges
//   - notification (webhook + structured log) on every attempt
//   - user can disable at any time
const (
	// DefaultMaxPer24hMicroUSD caps total auto top-ups in any rolling 24h
	// window. $50.
	DefaultMaxPer24hMicroUSD int64 = 50_000_000
	// DefaultMaxSingleMicroUSD caps a single auto top-up. $100.
	DefaultMaxSingleMicroUSD int64 = 100_000_000
	// DefaultCooldownSeconds is the minimum gap between successive top-ups.
	DefaultCooldownSeconds int64 = 60

	// MinTopupMicroUSD is the floor for a single top-up amount. Stripe's
	// minimum charge is $0.50, so anything below that can never settle.
	MinTopupMicroUSD int64 = 500_000

	// PlatformMaxSingleMicroUSD and PlatformMax24hMicroUSD are hard ceilings
	// the platform enforces regardless of the per-account configuration, so a
	// misconfigured or compromised account cannot authorize an unbounded
	// off-session charge. $500 per charge, $2,000 per rolling 24h.
	PlatformMaxSingleMicroUSD int64 = 500_000_000
	PlatformMax24hMicroUSD    int64 = 2_000_000_000
)

// ErrAutoTopupSkipped is returned by triggerAutoTopup when a precondition
// (disabled, above threshold, cooling down, cap reached, in flight) means no
// charge should be attempted. It is not an error condition — callers log it at
// debug level.
var ErrAutoTopupSkipped = errors.New("auto top-up skipped")

// AutoTopupManager orchestrates automatic balance replenishment.
type AutoTopupManager struct {
	store  store.Store
	stripe *StripeProcessor
	logger *slog.Logger

	httpClient *http.Client

	// inflight claims an account while a top-up is being processed so a burst
	// of debits can't fire multiple concurrent charges.
	//
	// NOTE: this guard is process-local. In a multi-replica coordinator
	// deployment two replicas could each pass the cap re-check and charge
	// concurrently (the record-pending → charge sequence is not transactional
	// across replicas). The rolling 24h cap still bounds the total exposure.
	// A DB advisory lock (or a conditional INSERT gated on the cap) would close
	// the cross-replica window if auto top-up is ever run multi-replica.
	inflight sync.Map // accountID -> struct{}
}

// NewAutoTopupManager constructs a manager. stripe may be nil, in which case
// auto top-up is inert (Enabled() reports false).
func NewAutoTopupManager(st store.Store, stripe *StripeProcessor, logger *slog.Logger) *AutoTopupManager {
	// The webhook HTTP client blocks connections to non-public IPs at dial
	// time (defends against SSRF + DNS rebinding via user-supplied URLs) and
	// re-validates redirect targets the same way.
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
			Control: ssrfSafeControl,
		}).DialContext,
	}
	return &AutoTopupManager{
		store:  st,
		stripe: stripe,
		logger: logger,
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return validateNotifyWebhookURL(req.URL.String())
			},
		},
	}
}

// Available reports whether auto top-up can run in this deployment (Stripe
// configured). Per-account enablement is checked separately from config.
func (m *AutoTopupManager) Available() bool { return m != nil && m.stripe != nil }

// ApplyAutoTopupDefaults fills zero-valued safety rails with their defaults.
// Mutates and returns cfg for convenience.
func ApplyAutoTopupDefaults(cfg *store.AutoTopupConfig) *store.AutoTopupConfig {
	if cfg.PaymentMethod == "" {
		cfg.PaymentMethod = string(MethodStripe)
	}
	if cfg.MaxPer24hMicroUSD == 0 {
		cfg.MaxPer24hMicroUSD = DefaultMaxPer24hMicroUSD
	}
	if cfg.MaxSingleMicroUSD == 0 {
		cfg.MaxSingleMicroUSD = DefaultMaxSingleMicroUSD
	}
	if cfg.CooldownSeconds == 0 {
		cfg.CooldownSeconds = DefaultCooldownSeconds
	}
	return cfg
}

// ValidateAutoTopupConfig checks an incoming config for internal consistency.
// It assumes defaults have already been applied. Only Stripe is supported.
func ValidateAutoTopupConfig(cfg *store.AutoTopupConfig) error {
	if !cfg.Enabled {
		return nil // a disabled config needs no further validation
	}
	if cfg.PaymentMethod != string(MethodStripe) {
		return fmt.Errorf("auto top-up only supports the %q payment method", MethodStripe)
	}
	if cfg.ThresholdMicroUSD < 0 {
		return errors.New("threshold must be non-negative")
	}
	if cfg.AmountMicroUSD < MinTopupMicroUSD {
		return fmt.Errorf("top-up amount must be at least $%.2f", float64(MinTopupMicroUSD)/1e6)
	}
	if cfg.AmountMicroUSD%10_000 != 0 {
		return errors.New("top-up amount must be a whole number of cents")
	}
	if cfg.MaxSingleMicroUSD < MinTopupMicroUSD {
		return fmt.Errorf("max single top-up must be at least $%.2f", float64(MinTopupMicroUSD)/1e6)
	}
	if cfg.AmountMicroUSD > cfg.MaxSingleMicroUSD {
		return errors.New("top-up amount exceeds the max single top-up cap")
	}
	if cfg.MaxPer24hMicroUSD < cfg.AmountMicroUSD {
		return errors.New("24h cap cannot be smaller than a single top-up amount")
	}
	if cfg.CooldownSeconds < 0 {
		return errors.New("cooldown must be non-negative")
	}
	// Platform-enforced hard ceilings — these bound the per-account caps so the
	// account-controlled "safety rails" can't be widened without limit.
	if cfg.MaxSingleMicroUSD > PlatformMaxSingleMicroUSD {
		return fmt.Errorf("max single top-up exceeds the platform ceiling of $%.2f", float64(PlatformMaxSingleMicroUSD)/1e6)
	}
	if cfg.MaxPer24hMicroUSD > PlatformMax24hMicroUSD {
		return fmt.Errorf("24h cap exceeds the platform ceiling of $%.2f", float64(PlatformMax24hMicroUSD)/1e6)
	}
	if err := validateNotifyWebhookURL(cfg.NotifyWebhookURL); err != nil {
		return err
	}
	return nil
}

// validateNotifyWebhookURL rejects webhook URLs that could be used for SSRF.
// It requires https and refuses hosts that resolve to loopback, private,
// link-local (incl. cloud metadata 169.254.169.254), or unspecified addresses.
// The dial-time guard in the manager's HTTP client (ssrfSafeControl) is the
// authoritative defense against DNS rebinding; this is the early-rejection UX.
func validateNotifyWebhookURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid notify webhook URL: %w", err)
	}
	if u.Scheme != "https" {
		return errors.New("notify webhook URL must use https")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("notify webhook URL must include a host")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("notify webhook host does not resolve: %w", err)
	}
	for _, ip := range ips {
		if isDisallowedDialIP(ip) {
			return fmt.Errorf("notify webhook host resolves to a disallowed address (%s)", ip)
		}
	}
	return nil
}

// isDisallowedDialIP reports whether connecting to ip would reach a non-public
// address that must be off-limits for user-supplied webhooks.
func isDisallowedDialIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// ssrfSafeControl is a net.Dialer Control hook that blocks connections to
// non-public IPs. Because it inspects the actual resolved dial address, it
// closes the DNS-rebinding gap that validateNotifyWebhookURL alone cannot.
func ssrfSafeControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("auto top-up webhook: unexpected non-IP dial address %q", host)
	}
	if isDisallowedDialIP(ip) {
		return fmt.Errorf("auto top-up webhook: blocked connection to disallowed address %s", ip)
	}
	return nil
}

// MaybeAutoTopup runs a best-effort auto top-up check for an account. It is
// safe to call from a goroutine on the hot path: all preconditions that mean
// "do nothing" return ErrAutoTopupSkipped, and any real error is logged rather
// than propagated to the inference request.
func (m *AutoTopupManager) MaybeAutoTopup(accountID string) {
	if !m.Available() {
		return
	}
	// This runs in a goroutine off the inference hot path. A panic here must
	// never take down the request-serving process.
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("auto top-up: recovered from panic", "account_id", accountID, "panic", r)
		}
	}()
	if err := m.triggerAutoTopup(accountID); err != nil {
		if errors.Is(err, ErrAutoTopupSkipped) {
			m.logger.Debug("auto top-up skipped", "account_id", accountID, "reason", err)
			return
		}
		m.logger.Error("auto top-up failed", "account_id", accountID, "error", err)
	}
}

// triggerAutoTopup performs the full checked top-up. Returns ErrAutoTopupSkipped
// (wrapped) when no charge is warranted, a real error when a charge was
// attempted but failed.
func (m *AutoTopupManager) triggerAutoTopup(accountID string) error {
	cfg, err := m.store.GetAutoTopupConfig(accountID)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil || !cfg.Enabled {
		return fmt.Errorf("%w: not enabled", ErrAutoTopupSkipped)
	}
	ApplyAutoTopupDefaults(cfg)

	// Claim the account. If another goroutine holds it, bail — its check will
	// cover the current low balance.
	if _, loaded := m.inflight.LoadOrStore(accountID, struct{}{}); loaded {
		return fmt.Errorf("%w: already in flight", ErrAutoTopupSkipped)
	}
	defer m.inflight.Delete(accountID)

	// Re-read balance under the claim — a concurrent top-up may have already
	// lifted us above the threshold.
	if bal := m.store.GetBalance(accountID); bal >= cfg.ThresholdMicroUSD {
		return fmt.Errorf("%w: balance %d >= threshold %d", ErrAutoTopupSkipped, bal, cfg.ThresholdMicroUSD)
	}

	amount := cfg.AmountMicroUSD
	if amount > cfg.MaxSingleMicroUSD {
		amount = cfg.MaxSingleMicroUSD
	}
	if amount < MinTopupMicroUSD {
		return fmt.Errorf("%w: amount %d below minimum", ErrAutoTopupSkipped, amount)
	}

	// Cooldown gate.
	last, err := m.store.LastAutoTopupEvent(accountID)
	if err != nil {
		return fmt.Errorf("load last event: %w", err)
	}
	if last != nil && cfg.CooldownSeconds > 0 {
		if elapsed := time.Since(last.CreatedAt); elapsed < time.Duration(cfg.CooldownSeconds)*time.Second {
			return fmt.Errorf("%w: cooldown (%s since last)", ErrAutoTopupSkipped, elapsed.Round(time.Second))
		}
	}

	// Rolling 24h cap.
	charged, err := m.store.AutoTopupChargedSince(accountID, time.Now().Add(-24*time.Hour))
	if err != nil {
		return fmt.Errorf("sum 24h charges: %w", err)
	}
	if charged+amount > cfg.MaxPer24hMicroUSD {
		return fmt.Errorf("%w: 24h cap reached (%d charged + %d > %d)",
			ErrAutoTopupSkipped, charged, amount, cfg.MaxPer24hMicroUSD)
	}

	// Need a saved card.
	user, err := m.store.GetUserByAccountID(accountID)
	if err != nil {
		return fmt.Errorf("load user: %w", err)
	}
	if user.StripeCustomerID == "" || user.StripeDefaultPaymentMethodID == "" {
		return fmt.Errorf("%w: no saved payment method", ErrAutoTopupSkipped)
	}

	// Record the attempt BEFORE charging so it counts toward the cap/cooldown
	// even if we crash mid-charge.
	eventID, err := m.store.RecordAutoTopupEvent(&store.AutoTopupEvent{
		AccountID:      accountID,
		AmountMicroUSD: amount,
		Status:         store.AutoTopupPending,
	})
	if err != nil {
		return fmt.Errorf("record event: %w", err)
	}

	amountCents := amount / 10_000 // micro-USD (1e6/$) → cents (1e2/$)
	// Credit exactly what the card is charged. amountCents truncates any
	// sub-cent remainder Stripe can't charge, so crediting the full micro-USD
	// amount would hand out money the card was never charged for. Validation
	// requires whole-cent amounts, so this is normally an identity.
	creditMicroUSD := amountCents * 10_000

	result, chargeErr := m.stripe.ChargeOffSession(OffSessionChargeRequest{
		CustomerID:      user.StripeCustomerID,
		PaymentMethodID: user.StripeDefaultPaymentMethodID,
		AmountCents:     amountCents,
		IdempotencyKey:  fmt.Sprintf("autotopup:%s:%d", accountID, eventID),
		Metadata: map[string]string{
			"auto_topup":       "true",
			"account_id":       accountID,
			"auto_topup_event": fmt.Sprintf("%d", eventID),
			"reason":           "balance_below_threshold",
		},
	})
	if chargeErr != nil {
		reason := chargeErr.Error()
		_ = m.store.UpdateAutoTopupEvent(eventID, store.AutoTopupFailed, externalIDOf(result), reason)
		m.notify(cfg, user, amount, store.AutoTopupFailed, reason)
		return fmt.Errorf("off-session charge: %w", chargeErr)
	}

	// The card has now been charged. Mark the event succeeded immediately so
	// the charged amount counts toward the rolling 24h cap regardless of what
	// happens to the internal credit below — the cap governs money taken from
	// the card, not money credited to the ledger.
	_ = m.store.UpdateAutoTopupEvent(eventID, store.AutoTopupSucceeded, result.PaymentIntentID, "")

	// Credit the internal balance, retrying transient store failures.
	if err := m.creditWithRetry(accountID, creditMicroUSD, result.PaymentIntentID); err != nil {
		// Charge succeeded but crediting ultimately failed — a money-impacting
		// inconsistency that needs manual reconciliation. The event stays
		// "succeeded" (the charge happened, so it counts toward the cap and is
		// not re-charged on the next pass), but we record the credit failure in
		// the reason and surface it loudly. The PaymentIntent ID is the
		// reconciliation key.
		_ = m.store.UpdateAutoTopupEvent(eventID, store.AutoTopupSucceeded, result.PaymentIntentID,
			"charged but credit failed: "+err.Error())
		m.logger.Error("CRITICAL: auto top-up charged the card but failed to credit the ledger — manual reconciliation required",
			"account_id", accountID,
			"payment_intent", result.PaymentIntentID,
			"amount_micro_usd", creditMicroUSD,
			"error", err,
		)
		m.notify(cfg, user, creditMicroUSD, store.AutoTopupSucceeded, "charged_but_credit_failed")
		return fmt.Errorf("CRITICAL: charged %d but failed to credit balance: %w", creditMicroUSD, err)
	}

	m.logger.Info("auto top-up succeeded",
		"account_id", accountID,
		"amount_micro_usd", creditMicroUSD,
		"payment_intent", result.PaymentIntentID,
	)
	m.notify(cfg, user, creditMicroUSD, store.AutoTopupSucceeded, "")
	return nil
}

// creditWithRetry credits the ledger, retrying a few times to ride out
// transient store errors after a card has already been charged.
func (m *AutoTopupManager) creditWithRetry(accountID string, amountMicroUSD int64, paymentIntentID string) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = m.store.Credit(accountID, amountMicroUSD, store.LedgerAutoTopup, "auto_topup:"+paymentIntentID); err == nil {
			return nil
		}
		m.logger.Warn("auto top-up: credit attempt failed, retrying",
			"account_id", accountID, "attempt", attempt+1, "error", err)
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	return err
}

func externalIDOf(r *OffSessionChargeResult) string {
	if r == nil {
		return ""
	}
	return r.PaymentIntentID
}

// autoTopupNotification is the JSON payload posted to a configured webhook.
type autoTopupNotification struct {
	Event          string `json:"event"` // "auto_topup.succeeded" | "auto_topup.failed"
	AccountID      string `json:"account_id"`
	Email          string `json:"email,omitempty"`
	AmountMicroUSD int64  `json:"amount_micro_usd"`
	AmountUSD      string `json:"amount_usd"`
	Status         string `json:"status"`
	FailureReason  string `json:"failure_reason,omitempty"`
	Timestamp      string `json:"timestamp"`
}

// notify emits a notification for an auto top-up attempt: always a structured
// log line, plus a best-effort webhook POST when the user configured one.
func (m *AutoTopupManager) notify(cfg *store.AutoTopupConfig, user *store.User, amount int64, status, failureReason string) {
	eventName := "auto_topup." + status
	// Log by account_id only — email is PII and is not needed in the log line.
	m.logger.Info("auto top-up notification",
		"event", eventName,
		"account_id", user.AccountID,
		"amount_micro_usd", amount,
		"status", status,
		"failure_reason", failureReason,
	)

	if cfg.NotifyWebhookURL == "" {
		return
	}
	// Re-validate at send time as defense-in-depth (the config may predate a
	// validation change); the dial-time guard is the rebinding-safe backstop.
	if err := validateNotifyWebhookURL(cfg.NotifyWebhookURL); err != nil {
		m.logger.Warn("auto top-up: skipping disallowed notify webhook URL", "error", err)
		return
	}
	payload := autoTopupNotification{
		Event:          eventName,
		AccountID:      user.AccountID,
		Email:          user.Email,
		AmountMicroUSD: amount,
		AmountUSD:      fmt.Sprintf("%.2f", float64(amount)/1e6),
		Status:         status,
		FailureReason:  failureReason,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		m.logger.Warn("auto top-up: marshal webhook payload", "error", err)
		return
	}
	go func() {
		req, err := http.NewRequest(http.MethodPost, cfg.NotifyWebhookURL, bytes.NewReader(body))
		if err != nil {
			m.logger.Warn("auto top-up: build webhook request", "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := m.httpClient.Do(req)
		if err != nil {
			m.logger.Warn("auto top-up: webhook delivery failed", "error", err)
			return
		}
		resp.Body.Close()
	}()
}
