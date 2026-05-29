package api

import (
	"encoding/json"
	"net/http"

	"github.com/eigeninference/d-inference/coordinator/billing"
	"github.com/eigeninference/d-inference/coordinator/store"
)

// Auto top-up HTTP handlers (issue #231).
//
// Endpoints (all Privy-authenticated, financial rate limit on mutations):
//
//	GET  /v1/billing/auto-topup               — current config + saved card
//	PUT  /v1/billing/auto-topup               — enable/disable + thresholds
//	POST /v1/billing/auto-topup/setup-intent  — start saving a card (off-session)
//	POST /v1/billing/auto-topup/confirm        — finalize the saved card
//
// Card setup is a two-step Stripe SetupIntent flow: setup-intent returns a
// client_secret the frontend confirms with Stripe.js, then confirm (or the
// setup_intent.succeeded webhook) persists the resulting payment method.

// autoTopupConfigResponse is the JSON view of an account's auto top-up state.
type autoTopupConfigResponse struct {
	Enabled           bool   `json:"enabled"`
	ThresholdMicroUSD int64  `json:"threshold_micro_usd"`
	AmountMicroUSD    int64  `json:"amount_micro_usd"`
	PaymentMethod     string `json:"payment_method"`
	MaxPer24hMicroUSD int64  `json:"max_per_24h_micro_usd"`
	MaxSingleMicroUSD int64  `json:"max_single_micro_usd"`
	CooldownSeconds   int64  `json:"cooldown_seconds"`
	NotifyWebhookURL  string `json:"notify_webhook_url,omitempty"`

	// Saved card display hints (read-only).
	HasSavedCard bool   `json:"has_saved_card"`
	CardBrand    string `json:"card_brand,omitempty"`
	CardLast4    string `json:"card_last4,omitempty"`
}

func autoTopupResponse(cfg *store.AutoTopupConfig, user *store.User) autoTopupConfigResponse {
	resp := autoTopupConfigResponse{
		HasSavedCard: user.StripeDefaultPaymentMethodID != "",
		CardBrand:    user.StripePaymentMethodBrand,
		CardLast4:    user.StripePaymentMethodLast4,
	}
	if cfg != nil {
		resp.Enabled = cfg.Enabled
		resp.ThresholdMicroUSD = cfg.ThresholdMicroUSD
		resp.AmountMicroUSD = cfg.AmountMicroUSD
		resp.PaymentMethod = cfg.PaymentMethod
		resp.MaxPer24hMicroUSD = cfg.MaxPer24hMicroUSD
		resp.MaxSingleMicroUSD = cfg.MaxSingleMicroUSD
		resp.CooldownSeconds = cfg.CooldownSeconds
		resp.NotifyWebhookURL = cfg.NotifyWebhookURL
	}
	return resp
}

// handleGetAutoTopup handles GET /v1/billing/auto-topup.
func (s *Server) handleGetAutoTopup(w http.ResponseWriter, r *http.Request) {
	user := s.requirePrivyUser(w, r)
	if user == nil {
		return
	}
	if s.billing == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("billing_error", "billing not configured"))
		return
	}

	cfg, err := s.store.GetAutoTopupConfig(user.AccountID)
	if err != nil {
		s.logger.Error("auto top-up: load config failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse("internal_error", "failed to load auto top-up config"))
		return
	}
	writeJSON(w, http.StatusOK, autoTopupResponse(cfg, user))
}

// handleSetAutoTopup handles PUT /v1/billing/auto-topup.
func (s *Server) handleSetAutoTopup(w http.ResponseWriter, r *http.Request) {
	user := s.requirePrivyUser(w, r)
	if user == nil {
		return
	}
	if s.billing == nil || s.billing.AutoTopup() == nil || !s.billing.AutoTopup().Available() {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("billing_error", "auto top-up not available (Stripe not configured)"))
		return
	}

	var req struct {
		Enabled           bool   `json:"enabled"`
		ThresholdMicroUSD *int64 `json:"threshold_micro_usd"`
		AmountMicroUSD    *int64 `json:"amount_micro_usd"`
		PaymentMethod     string `json:"payment_method"`
		MaxPer24hMicroUSD *int64 `json:"max_per_24h_micro_usd"`
		MaxSingleMicroUSD *int64 `json:"max_single_micro_usd"`
		CooldownSeconds   *int64 `json:"cooldown_seconds"`
		NotifyWebhookURL  string `json:"notify_webhook_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid_request_error", "invalid JSON: "+err.Error()))
		return
	}

	// Start from any existing config so a partial update preserves untouched
	// fields, then overlay the request.
	cfg, err := s.store.GetAutoTopupConfig(user.AccountID)
	if err != nil {
		s.logger.Error("auto top-up: load config failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse("internal_error", "failed to load auto top-up config"))
		return
	}
	if cfg == nil {
		cfg = &store.AutoTopupConfig{AccountID: user.AccountID}
	}
	cfg.Enabled = req.Enabled
	if req.PaymentMethod != "" {
		cfg.PaymentMethod = req.PaymentMethod
	}
	if req.ThresholdMicroUSD != nil {
		cfg.ThresholdMicroUSD = *req.ThresholdMicroUSD
	}
	if req.AmountMicroUSD != nil {
		cfg.AmountMicroUSD = *req.AmountMicroUSD
	}
	if req.MaxPer24hMicroUSD != nil {
		cfg.MaxPer24hMicroUSD = *req.MaxPer24hMicroUSD
	}
	if req.MaxSingleMicroUSD != nil {
		cfg.MaxSingleMicroUSD = *req.MaxSingleMicroUSD
	}
	if req.CooldownSeconds != nil {
		cfg.CooldownSeconds = *req.CooldownSeconds
	}
	cfg.NotifyWebhookURL = req.NotifyWebhookURL

	billing.ApplyAutoTopupDefaults(cfg)

	// Enabling requires a saved card to charge.
	if cfg.Enabled && user.StripeDefaultPaymentMethodID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid_request_error",
			"no saved card — call /v1/billing/auto-topup/setup-intent and save a card before enabling auto top-up"))
		return
	}
	if err := billing.ValidateAutoTopupConfig(cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid_request_error", err.Error()))
		return
	}

	if err := s.store.SetAutoTopupConfig(cfg); err != nil {
		s.logger.Error("auto top-up: save config failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse("internal_error", "failed to save auto top-up config"))
		return
	}
	s.logger.Info("auto top-up: config updated", "account_id", user.AccountID, "enabled", cfg.Enabled)
	writeJSON(w, http.StatusOK, autoTopupResponse(cfg, user))
}

// handleAutoTopupSetupIntent handles POST /v1/billing/auto-topup/setup-intent.
// Creates (or reuses) a Stripe customer for the user and returns a SetupIntent
// client secret the frontend uses to collect and save a card off-session.
func (s *Server) handleAutoTopupSetupIntent(w http.ResponseWriter, r *http.Request) {
	user := s.requirePrivyUser(w, r)
	if user == nil {
		return
	}
	if s.billing == nil || s.billing.Stripe() == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("billing_error", "Stripe not configured"))
		return
	}

	customerID, err := s.ensureStripeCustomer(user)
	if err != nil {
		s.logger.Error("auto top-up: ensure customer failed", "error", err)
		writeJSON(w, http.StatusBadGateway, errorResponse("billing_error", "failed to create Stripe customer"))
		return
	}

	si, err := s.billing.Stripe().CreateSetupIntent(customerID)
	if err != nil {
		s.logger.Error("auto top-up: create setup intent failed", "error", err)
		writeJSON(w, http.StatusBadGateway, errorResponse("billing_error", "failed to create setup intent"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"setup_intent_id": si.ID,
		"client_secret":   si.ClientSecret,
		"customer_id":     customerID,
	})
}

// handleAutoTopupConfirm handles POST /v1/billing/auto-topup/confirm.
// Called after the frontend confirms the SetupIntent. Reads the saved payment
// method off the SetupIntent and persists it as the user's default card.
func (s *Server) handleAutoTopupConfirm(w http.ResponseWriter, r *http.Request) {
	user := s.requirePrivyUser(w, r)
	if user == nil {
		return
	}
	if s.billing == nil || s.billing.Stripe() == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("billing_error", "Stripe not configured"))
		return
	}

	var req struct {
		SetupIntentID string `json:"setup_intent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid_request_error", "invalid JSON: "+err.Error()))
		return
	}
	if req.SetupIntentID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid_request_error", "setup_intent_id is required"))
		return
	}

	si, err := s.billing.Stripe().GetSetupIntent(req.SetupIntentID)
	if err != nil {
		s.logger.Error("auto top-up: get setup intent failed", "error", err)
		writeJSON(w, http.StatusBadGateway, errorResponse("billing_error", "failed to read setup intent"))
		return
	}
	if si.Status != "succeeded" || si.PaymentMethod == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid_request_error",
			"setup intent is not completed — confirm the card with Stripe.js first"))
		return
	}
	// Ownership guard. The caller must already have a Stripe customer (created
	// by setup-intent) and the SetupIntent must belong to it. This prevents an
	// account with no customer of its own from confirming someone else's
	// SetupIntent and attaching the victim's card (IDOR).
	if user.StripeCustomerID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid_request_error",
			"no Stripe customer on file — call /v1/billing/auto-topup/setup-intent first"))
		return
	}
	if si.Customer == "" || si.Customer != user.StripeCustomerID {
		writeJSON(w, http.StatusForbidden, errorResponse("invalid_request_error", "setup intent does not belong to this account"))
		return
	}

	if err := s.persistSavedCard(user, user.StripeCustomerID, si.PaymentMethod); err != nil {
		s.logger.Error("auto top-up: persist card failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse("internal_error", "failed to save card"))
		return
	}

	updated, _ := s.store.GetUserByAccountID(user.AccountID)
	if updated == nil {
		updated = user
	}
	cfg, _ := s.store.GetAutoTopupConfig(user.AccountID)
	writeJSON(w, http.StatusOK, autoTopupResponse(cfg, updated))
}

// handleSetupIntentSucceeded persists the saved card from a
// setup_intent.succeeded webhook event. Best-effort: errors are logged, not
// returned, since the webhook is already verified and the synchronous confirm
// path is the primary route.
func (s *Server) handleSetupIntentSucceeded(event *billing.WebhookEvent) {
	var data struct {
		Object struct {
			ID            string `json:"id"`
			Customer      string `json:"customer"`
			PaymentMethod string `json:"payment_method"`
		} `json:"object"`
	}
	if err := json.Unmarshal(event.Data, &data); err != nil {
		s.logger.Error("auto top-up: parse setup_intent webhook failed", "error", err)
		return
	}
	customerID := data.Object.Customer
	pmID := data.Object.PaymentMethod
	if customerID == "" || pmID == "" {
		s.logger.Warn("auto top-up: setup_intent webhook missing customer or payment_method")
		return
	}
	user, err := s.store.GetUserByStripeCustomer(customerID)
	if err != nil {
		s.logger.Warn("auto top-up: setup_intent webhook for unknown customer", "customer", customerID)
		return
	}
	if err := s.persistSavedCard(user, customerID, pmID); err != nil {
		s.logger.Error("auto top-up: persist card from webhook failed", "error", err)
		return
	}
	s.logger.Info("auto top-up: saved card via webhook", "account_id", user.AccountID)
}

// ensureStripeCustomer returns the user's inbound Stripe customer ID, creating
// and persisting one if they don't have it yet.
func (s *Server) ensureStripeCustomer(user *store.User) (string, error) {
	if user.StripeCustomerID != "" {
		return user.StripeCustomerID, nil
	}
	customerID, err := s.billing.Stripe().CreateCustomer(user.Email, map[string]string{
		"account_id": user.AccountID,
	})
	if err != nil {
		return "", err
	}
	if err := s.store.SetUserStripeCustomer(user.AccountID, customerID,
		user.StripeDefaultPaymentMethodID, user.StripePaymentMethodBrand, user.StripePaymentMethodLast4); err != nil {
		return "", err
	}
	user.StripeCustomerID = customerID
	return customerID, nil
}

// persistSavedCard stores paymentMethodID as the user's default card, fetching
// the brand/last4 for display (best-effort — persistence does not fail if the
// display lookup does).
func (s *Server) persistSavedCard(user *store.User, customerID, paymentMethodID string) error {
	brand, last4 := "", ""
	if card, err := s.billing.Stripe().GetPaymentMethod(paymentMethodID); err == nil {
		brand, last4 = card.Brand, card.Last4
	} else {
		s.logger.Warn("auto top-up: payment method lookup failed", "error", err)
	}
	if customerID == "" {
		customerID = user.StripeCustomerID
	}
	return s.store.SetUserStripeCustomer(user.AccountID, customerID, paymentMethodID, brand, last4)
}
