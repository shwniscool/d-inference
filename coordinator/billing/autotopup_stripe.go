package billing

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// This file extends StripeProcessor with the off-session ("merchant-initiated")
// charge primitives that auto top-up needs:
//
//   - CreateCustomer       — once per user, so cards can be saved against it
//   - CreateSetupIntent    — collect + save a card without charging it
//   - GetSetupIntent       — read back the saved payment method after the
//                            frontend confirms the SetupIntent
//   - GetPaymentMethod     — fetch card brand/last4 for display
//   - ChargeOffSession     — charge the saved card with off_session=true so no
//                            user interaction is required mid-session
//
// Checkout (the on-session deposit flow) lives in stripe.go; these reuse the
// same secret key, HTTP client, and the package-level stripeAPIBase override
// that tests point at an httptest server.

// stripeErrorEnvelope is Stripe's standard error response shape. The decline
// `code`/`type` let us produce an actionable failure_reason for the user.
type stripeErrorEnvelope struct {
	Error struct {
		Type        string `json:"type"`
		Code        string `json:"code"`
		DeclineCode string `json:"decline_code"`
		Message     string `json:"message"`
	} `json:"error"`
}

// postForm issues a form-encoded POST to the Stripe API and decodes the JSON
// response into out. On a non-2xx status it returns a *StripeAPIError carrying
// the parsed Stripe error fields.
func (p *StripeProcessor) postForm(path string, params url.Values, out any) error {
	return p.doForm(http.MethodPost, path, params, out)
}

func (p *StripeProcessor) doForm(method, path string, params url.Values, out any) error {
	var body io.Reader
	if params != nil {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequest(method, stripeAPIBase+path, body)
	if err != nil {
		return fmt.Errorf("stripe: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.secretKey)
	if params != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("stripe: API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("stripe: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env stripeErrorEnvelope
		_ = json.Unmarshal(respBody, &env)
		return &StripeAPIError{
			StatusCode:  resp.StatusCode,
			Type:        env.Error.Type,
			Code:        env.Error.Code,
			DeclineCode: env.Error.DeclineCode,
			Message:     env.Error.Message,
			Raw:         string(respBody),
		}
	}

	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("stripe: parse response: %w", err)
		}
	}
	return nil
}

// StripeAPIError is a structured Stripe API error.
type StripeAPIError struct {
	StatusCode  int
	Type        string
	Code        string
	DeclineCode string
	Message     string
	Raw         string
}

func (e *StripeAPIError) Error() string {
	parts := []string{fmt.Sprintf("stripe: API error (status %d)", e.StatusCode)}
	if e.Code != "" {
		parts = append(parts, "code="+e.Code)
	}
	if e.DeclineCode != "" {
		parts = append(parts, "decline_code="+e.DeclineCode)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	return strings.Join(parts, " ")
}

// CreateCustomer creates a Stripe Customer and returns its ID. email may be
// empty. metadata is attached for traceability from the Stripe dashboard.
func (p *StripeProcessor) CreateCustomer(email string, metadata map[string]string) (string, error) {
	params := url.Values{}
	if email != "" {
		params.Set("email", email)
	}
	for k, v := range checkoutMetadata(metadata) {
		params.Set("metadata["+k+"]", v)
	}

	var out struct {
		ID string `json:"id"`
	}
	if err := p.postForm("/v1/customers", params, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("stripe: customer create returned no id")
	}
	return out.ID, nil
}

// SetupIntentResult is returned when creating a SetupIntent. ClientSecret is
// handed to the frontend (Stripe.js) to collect and confirm the card.
type SetupIntentResult struct {
	ID           string `json:"id"`
	ClientSecret string `json:"client_secret"`
	Status       string `json:"status"`
}

// CreateSetupIntent creates an off-session SetupIntent for saving a card
// against the given customer.
func (p *StripeProcessor) CreateSetupIntent(customerID string) (*SetupIntentResult, error) {
	if customerID == "" {
		return nil, fmt.Errorf("stripe: customer ID required for SetupIntent")
	}
	params := url.Values{}
	params.Set("customer", customerID)
	params.Set("usage", "off_session")
	params.Set("payment_method_types[0]", "card")

	var out SetupIntentResult
	if err := p.postForm("/v1/setup_intents", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetupIntentInfo is the read-back of a confirmed SetupIntent.
type SetupIntentInfo struct {
	ID            string `json:"id"`
	Status        string `json:"status"` // "succeeded" once the card is saved
	Customer      string `json:"customer"`
	PaymentMethod string `json:"payment_method"`
}

// GetSetupIntent fetches a SetupIntent so the caller can read the saved
// payment_method after the frontend confirms it.
func (p *StripeProcessor) GetSetupIntent(setupIntentID string) (*SetupIntentInfo, error) {
	var out SetupIntentInfo
	if err := p.doForm(http.MethodGet, "/v1/setup_intents/"+url.PathEscape(setupIntentID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PaymentMethodInfo is the card display detail for a saved payment method.
type CardInfo struct {
	Brand string
	Last4 string
}

// GetPaymentMethod fetches the card brand/last4 for a saved payment method.
func (p *StripeProcessor) GetPaymentMethod(paymentMethodID string) (*CardInfo, error) {
	var out struct {
		Card struct {
			Brand string `json:"brand"`
			Last4 string `json:"last4"`
		} `json:"card"`
	}
	if err := p.doForm(http.MethodGet, "/v1/payment_methods/"+url.PathEscape(paymentMethodID), nil, &out); err != nil {
		return nil, err
	}
	return &CardInfo{Brand: out.Card.Brand, Last4: out.Card.Last4}, nil
}

// OffSessionChargeRequest is the input to ChargeOffSession.
type OffSessionChargeRequest struct {
	CustomerID      string
	PaymentMethodID string
	AmountCents     int64
	Currency        string // defaults to "usd"
	IdempotencyKey  string // dedupe key; Stripe collapses retries with the same key
	Metadata        map[string]string
}

// OffSessionChargeResult is the outcome of an off-session charge.
type OffSessionChargeResult struct {
	PaymentIntentID string `json:"id"`
	Status          string `json:"status"` // "succeeded", "requires_action", ...
}

// ChargeOffSession charges a saved card without user interaction by creating a
// confirmed PaymentIntent with off_session=true. A "requires_action" status
// (or a card-declined error) is surfaced to the caller — auto top-up treats
// both as a failure since there is no user present to authenticate.
func (p *StripeProcessor) ChargeOffSession(req OffSessionChargeRequest) (*OffSessionChargeResult, error) {
	if req.CustomerID == "" || req.PaymentMethodID == "" {
		return nil, fmt.Errorf("stripe: customer and payment method required for off-session charge")
	}
	if req.AmountCents < 50 {
		return nil, fmt.Errorf("stripe: minimum charge is $0.50 (50 cents)")
	}
	currency := req.Currency
	if currency == "" {
		currency = "usd"
	}

	params := url.Values{}
	params.Set("amount", strconv.FormatInt(req.AmountCents, 10))
	params.Set("currency", currency)
	params.Set("customer", req.CustomerID)
	params.Set("payment_method", req.PaymentMethodID)
	params.Set("off_session", "true")
	params.Set("confirm", "true")
	for k, v := range checkoutMetadata(req.Metadata) {
		params.Set("metadata["+k+"]", v)
	}

	req2, err := http.NewRequest(http.MethodPost, stripeAPIBase+"/v1/payment_intents",
		strings.NewReader(params.Encode()))
	if err != nil {
		return nil, fmt.Errorf("stripe: build request: %w", err)
	}
	req2.Header.Set("Authorization", "Bearer "+p.secretKey)
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if req.IdempotencyKey != "" {
		req2.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}

	resp, err := p.httpClient.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("stripe: API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("stripe: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env stripeErrorEnvelope
		_ = json.Unmarshal(respBody, &env)
		return nil, &StripeAPIError{
			StatusCode:  resp.StatusCode,
			Type:        env.Error.Type,
			Code:        env.Error.Code,
			DeclineCode: env.Error.DeclineCode,
			Message:     env.Error.Message,
			Raw:         string(respBody),
		}
	}

	var out OffSessionChargeResult
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("stripe: parse payment intent: %w", err)
	}
	if out.Status != "succeeded" {
		return &out, fmt.Errorf("stripe: off-session charge not completed (status: %s)", out.Status)
	}
	return &out, nil
}
