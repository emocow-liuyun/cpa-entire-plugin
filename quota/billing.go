package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// billingStatus mirrors the BillingStatus schema of the Entire control plane.
// Every field is a pointer or has an explicit presence flag so a missing
// section is distinguishable from a legitimate zero.
type billingStatus struct {
	State          *string                `json:"state"`
	Currency       *string                `json:"currency"`
	Balance        *billingBalance        `json:"balance"`
	AutomaticTopUp *billingAutomaticTopUp `json:"automatic_top_up"`
	Overage        *billingOverage        `json:"overage"`
	RunsPaused     *billingRunsPaused     `json:"runs_paused"`
	Trial          *billingTrial          `json:"trial"`
	Unavailable    []string               `json:"unavailable"`

	// serverOffsetMs is the upstream clock skew measured from the Date header.
	serverOffsetMs int64
}

// billingBalance mirrors the BillingBalance schema.
type billingBalance struct {
	TotalCents     *int64  `json:"total_cents"`
	TrialCents     *int64  `json:"trial_cents"`
	GrantedCents   *int64  `json:"granted_cents"`
	PurchasedCents *int64  `json:"purchased_cents"`
	Currency       *string `json:"currency"`
	NextExpiry     *struct {
		AmountCents *int64  `json:"amount_cents"`
		ExpiresAt   *string `json:"expires_at"`
	} `json:"next_expiry"`
}

// billingAutomaticTopUp mirrors the BillingAutomaticTopUp schema.
type billingAutomaticTopUp struct {
	Enabled          *bool   `json:"enabled"`
	ThresholdCents   *int64  `json:"threshold_cents"`
	TargetCents      *int64  `json:"target_cents"`
	Currency         *string `json:"currency"`
	LastChargeFailed *bool   `json:"last_charge_failed"`
}

// billingOverage mirrors the BillingOverage schema.
type billingOverage struct {
	AmountCents *int64  `json:"amount_cents"`
	Currency    *string `json:"currency"`
}

// billingRunsPaused mirrors the BillingRunsPaused schema.
type billingRunsPaused struct {
	Paused *bool   `json:"paused"`
	Reason *string `json:"reason"`
}

// billingTrial mirrors the BillingTrial schema.
type billingTrial struct {
	State              *string `json:"state"`
	CreditRemaining    *int64  `json:"credit_remaining_cents"`
	CreditTotal        *int64  `json:"credit_total_cents"`
	CreditExpiredCents *int64  `json:"credit_expired_cents"`
	Currency           *string `json:"currency"`
	ExpiresAt          *string `json:"expires_at"`
}

// decodeBillingStatus parses one upstream billing document.
func decodeBillingStatus(body []byte) (billingStatus, error) {
	var status billingStatus
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&status); errDecode != nil {
		return billingStatus{}, newPluginError(codeUpstreamMalformed, "Entire billing response is not valid JSON", 0)
	}
	// Some deployments nest the document under "billing".
	if status.State == nil || (status.Balance == nil && status.Trial == nil) {
		var wrapped struct {
			Billing *billingStatus `json:"billing"`
		}
		if errWrapped := json.Unmarshal(body, &wrapped); errWrapped == nil && wrapped.Billing != nil {
			if wrapped.Billing.State != nil || wrapped.Billing.Balance != nil {
				copied := *wrapped.Billing
				status = copied
			}
		}
	}
	if status.State == nil && status.Balance == nil && status.Trial == nil && status.AutomaticTopUp == nil {
		return billingStatus{}, newPluginError(codeUpstreamMalformed,
			"Entire billing response does not match the BillingStatus schema", 0)
	}
	return status, nil
}

// hasUnavailable reports whether a section is listed in the unavailable array.
func (s billingStatus) hasUnavailable(section string) bool {
	target := strings.ToLower(strings.TrimSpace(section))
	for _, item := range s.Unavailable {
		if strings.ToLower(strings.TrimSpace(item)) == target {
			return true
		}
	}
	return false
}

// stateValue returns the billing state or an empty string.
func (s billingStatus) stateValue() string {
	if s.State == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(*s.State))
}

// currencyValue returns the normalized ISO currency code, defaulting to USD.
func (s billingStatus) currencyValue() string {
	candidates := []*string{s.Currency}
	if s.Balance != nil && s.Balance.Currency != nil {
		candidates = append([]*string{s.Balance.Currency}, candidates...)
	}
	if s.AutomaticTopUp != nil && s.AutomaticTopUp.Currency != nil {
		candidates = append(candidates, s.AutomaticTopUp.Currency)
	}
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		code := strings.ToUpper(strings.TrimSpace(*candidate))
		if code != "" {
			return code
		}
	}
	return "USD"
}

// dollars converts integer cents to a dollars amount.
func dollars(cents int64) float64 {
	return float64(cents) / 100
}

// safeFloat converts cents to a display amount, guarding against overflow.
func safeFloat(value int64) float64 {
	if value > math.MaxInt64/100 || value < math.MinInt64/100 {
		return 0
	}
	return dollars(value)
}

// formatCents renders an integer cent amount with two decimals for descriptions.
func formatCents(cents int64) string {
	negative := cents < 0
	if negative {
		cents = -cents
	}
	whole := strconv.FormatInt(cents/100, 10)
	frac := cents % 100
	out := whole + "." + string([]byte{byte('0' + frac/10), byte('0' + frac%10)})
	if negative {
		return "-" + out
	}
	return out
}
