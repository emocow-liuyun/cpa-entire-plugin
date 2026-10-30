package main

import (
	"fmt"
	"net/http"
	"strings"
)

// Quota response types mirroring pluginapi.QuotaFetchResponse and friends.
// They are declared locally so the plugin binary does not depend on the host
// module; the wire shape is the contract.
type quotaSubscription struct {
	Plan     string `json:"plan,omitempty"`
	TierName string `json:"tierName,omitempty"`
	TierID   string `json:"tierId,omitempty"`
}

type quotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

type quotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []quotaBucket `json:"buckets,omitempty"`
}

type quotaMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Unit     string  `json:"unit,omitempty"`
	Format   string  `json:"format,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

type quotaFetchResponse struct {
	Subscription       *quotaSubscription `json:"subscription,omitempty"`
	Summary            []quotaMetric      `json:"summary,omitempty"`
	ServerTimeOffsetMs int64              `json:"serverTimeOffsetMs,omitempty"`
	Groups             []quotaGroup       `json:"groups,omitempty"`
}

// Metric keys emitted through Summary. They are stable and documented in README.md.
const (
	metricCreditBalance   = "entire_credit_balance"
	metricTrialRemaining  = "entire_trial_remaining"
	metricPurchasedCredit = "entire_purchased_credit"
	metricGrantedCredit   = "entire_granted_credit"
	metricAutoTopUpThresh = "entire_autotopup_threshold"
	metricAutoTopUpTarget = "entire_autotopup_target"
	metricOverage         = "entire_overage"
	metricAutoTopUpStatus = "entire_autotopup_enabled"
)

// Bucket windows emitted through Groups.
const (
	windowCreditTotal = "total"
	windowTrial       = "trial"
	windowPurchased   = "purchased"
	windowGranted     = "granted"
	groupCredit       = "Entire credits"
)

// nonSpendableStates are billing states in which no credit can be spent.
var nonSpendableStates = map[string]string{
	"not_started":  "billing is not set up; no credit has been granted",
	"pending_card": "billing awaits a payment method; trial credit is locked",
	"provisioning": "billing setup is incomplete",
}

// quotaEvaluation carries the mapping inputs. quotaUnknown forces a fail-closed result.
type quotaEvaluation struct {
	Status         billingStatus
	RequireTopUp   bool
	Currency       string
	ServerOffsetMs int64
}

// evaluateQuota maps one billing document to the normalized quota response.
//
// Fail-closed rules, in order:
//  1. a billing state the plugin does not model            -> unknown error
//  2. a state where credit is not spendable                -> zero buckets + unknown error
//  3. unavailable balance/trial/overage/runs_paused        -> unknown error
//  4. runs_paused.paused = true                            -> zero buckets
//  5. usable balance.total_cents = 0                       -> explicit zero, no error
//  6. automatic top-up unreadable, failed, or misconfigured -> unknown error
//
// A genuine zero balance is therefore always reported as zero, never as unknown.
func evaluateQuota(eval quotaEvaluation) (quotaFetchResponse, error) {
	status := eval.Status
	currency := strings.ToUpper(strings.TrimSpace(eval.Currency))
	if currency == "" {
		currency = status.currencyValue()
	}

	plan := planLabel(status)
	subscription := &quotaSubscription{Plan: plan, TierName: plan}
	response := quotaFetchResponse{
		Subscription:       subscription,
		ServerTimeOffsetMs: eval.ServerOffsetMs,
	}

	state := status.stateValue()
	if state == "" {
		return quotaFetchResponse{}, newPluginError(codeUnknownQuota,
			"Entire billing response carries no state; credit cannot be determined", 0)
	}
	if reason, blocked := nonSpendableStates[state]; blocked {
		response.Groups = zeroCreditGroups(currency, plan)
		response.Summary = zeroCreditSummary(currency)
		return response, newPluginError(codeUnknownQuota,
			"Entire billing state "+state+" ("+reason+"): balance is not authoritative", 0)
	}
	if state != "active" {
		return quotaFetchResponse{}, newPluginError(codeUnknownQuota,
			"Entire billing state "+state+" is not modeled by this plugin; credit cannot be determined", 0)
	}

	// Sections the control plane itself flagged as unreadable.
	if status.hasUnavailable("balance") || status.Balance == nil || status.Balance.TotalCents == nil {
		return quotaFetchResponse{}, newPluginError(codeBillingUnreadable,
			"Entire billing section balance is unavailable; credit cannot be determined", 0)
	}
	if status.hasUnavailable("overage") || status.Overage == nil || status.Overage.AmountCents == nil {
		return quotaFetchResponse{}, newPluginError(codeBillingUnreadable,
			"Entire billing section overage is unavailable; spend state cannot be determined", 0)
	}
	if status.hasUnavailable("runs_paused") || status.RunsPaused == nil || status.RunsPaused.Paused == nil {
		return quotaFetchResponse{}, newPluginError(codeBillingUnreadable,
			"Entire billing section runs_paused is unavailable; spend state cannot be determined", 0)
	}

	total := *status.Balance.TotalCents
	if total < 0 {
		return quotaFetchResponse{}, newPluginError(codeBillingUnreadable,
			"Entire billing reported a negative credit balance", 0)
	}

	// Entire refuses all runs while a billing restriction is in force.
	if *status.RunsPaused.Paused {
		response.Groups = zeroCreditGroups(currency, plan)
		response.Summary = zeroCreditSummary(currency)
		reason := "runs paused by billing"
		if status.RunsPaused.Reason != nil && strings.TrimSpace(*status.RunsPaused.Reason) != "" {
			reason = "runs paused by billing (" + strings.TrimSpace(*status.RunsPaused.Reason) + ")"
		}
		return response, newPluginError(codeUnknownQuota, reason+": balance is not spendable", 0)
	}

	// A zero balance is a real, spendable-but-empty state. Zero is only usable,
	// though, when the automatic refill path is known: an unreadable or failed
	// top-up block on a zero balance is fail closed.
	if total == 0 {
		topUp, errTopUp := resolveTopUp(status, eval.RequireTopUp)
		response.Groups = zeroCreditGroups(currency, plan)
		response.Summary = zeroCreditSummary(currency)
		if errTopUp != nil {
			return response, errTopUp
		}
		response.Summary = append(response.Summary, overageMetrics(status, currency)...)
		response.Summary = append(response.Summary, topUpStatusMetrics(topUp)...)
		return response, nil
	}

	// Automatic top-up: the only documented automatic refill path.
	topUp, errTopUp := resolveTopUp(status, eval.RequireTopUp)
	if errTopUp != nil {
		return quotaFetchResponse{}, errTopUp
	}

	groups := make([]quotaGroup, 0, 2)
	summary := make([]quotaMetric, 0, 8)

	creditBuckets := make([]quotaBucket, 0, 1)
	creditBuckets = append(creditBuckets, quotaBucket{
		Window:            windowCreditTotal,
		RemainingFraction: 1,
		ResetTime:         nextExpiry(status),
		Description:       "credit balance " + money(currency, total) + " (" + fmt.Sprintf("%d", total) + " cents)",
	})
	groups = append(groups, quotaGroup{DisplayName: groupCredit, Buckets: creditBuckets})

	summary = append(summary, quotaMetric{
		Key:      metricCreditBalance,
		Label:    "Entire credit balance",
		Value:    safeFloat(total),
		Unit:     currency,
		Format:   "currency",
		Currency: currency,
	})
	if status.Balance.TrialCents != nil {
		summary = append(summary, quotaMetric{
			Key:      metricTrialRemaining,
			Label:    "Entire trial credit left",
			Value:    safeFloat(*status.Balance.TrialCents),
			Unit:     currency,
			Format:   "currency",
			Currency: currency,
		})
	}
	if status.Balance.PurchasedCents != nil {
		summary = append(summary, quotaMetric{
			Key:      metricPurchasedCredit,
			Label:    "Entire purchased credit left",
			Value:    safeFloat(*status.Balance.PurchasedCents),
			Unit:     currency,
			Format:   "currency",
			Currency: currency,
		})
	}
	if status.Balance.GrantedCents != nil {
		summary = append(summary, quotaMetric{
			Key:      metricGrantedCredit,
			Label:    "Entire granted credit left",
			Value:    safeFloat(*status.Balance.GrantedCents),
			Unit:     currency,
			Format:   "currency",
			Currency: currency,
		})
	}
	summary = append(summary, overageMetrics(status, currency)...)
	summary = append(summary, topUpStatusMetrics(topUp)...)

	response.Groups = groups
	response.Summary = summary
	return response, nil
}

// topUpStatusMetrics reports whether automatic top-up is enabled as a number
// (1 = enabled, 0 = disabled). It carries no limit, threshold or target value:
// those are top-up settings, not quota facts, and are not derived ratios.
func topUpStatusMetrics(topUp topUpState) []quotaMetric {
	value := 0.0
	if topUp.enabled {
		value = 1
	}
	return []quotaMetric{{
		Key:    metricAutoTopUpStatus,
		Label:  "Entire automatic top-up enabled",
		Value:  value,
		Format: "number",
	}}
}

// topUpState is the validated automatic-top-up result.
type topUpState struct {
	enabled        bool
	thresholdCents int64
	targetCents    int64
}

// resolveTopUp validates the automatic top-up block and reports only its
// enabled state. The block is top-up configuration, not a spending limit.
func resolveTopUp(status billingStatus, requireTopUp bool) (topUpState, error) {
	if status.hasUnavailable("automatic_top_up") || status.hasUnavailable("automatic_top_up.last_charge_failed") {
		// require-top-up-on-failure: false treats an unreadable refill block as
		// "no refill" instead of failing closed.
		if !requireTopUp {
			return topUpState{}, nil
		}
		return topUpState{}, newPluginError(codeBillingUnreadable,
			"Entire billing section automatic_top_up is unavailable; refill path is unknown", 0)
	}
	if status.hasUnavailable("automatic_top_up.last_charge_failed") {
		return topUpState{}, newPluginError(codeBillingUnreadable,
			"Entire automatic top-up charge state is unavailable; refill path is unknown", 0)
	}
	topUp := status.AutomaticTopUp
	if topUp == nil || topUp.Enabled == nil {
		return topUpState{}, newPluginError(codeUnknownQuota,
			"Entire automatic top-up is not reported; refill path is unknown", 0)
	}
	if topUp.LastChargeFailed != nil && *topUp.LastChargeFailed {
		if requireTopUp {
			return topUpState{}, newPluginError(codeUnknownQuota,
				"Entire automatic top-up is disabled after a failed charge; refill path is unknown", 0)
		}
	}
	state := topUpState{enabled: *topUp.Enabled}
	if topUp.ThresholdCents != nil {
		state.thresholdCents = *topUp.ThresholdCents
	}
	if topUp.TargetCents != nil {
		state.targetCents = *topUp.TargetCents
	}
	if !state.enabled {
		return state, nil
	}
	if topUp.ThresholdCents == nil || topUp.TargetCents == nil {
		return topUpState{}, newPluginError(codeBillingUnreadable,
			"Entire automatic top-up settings are incomplete; refill state is unknown", 0)
	}
	if state.thresholdCents < 0 || state.targetCents < 0 || state.targetCents < state.thresholdCents {
		return topUpState{}, newPluginError(codeBillingUnreadable,
			"Entire automatic top-up settings are inconsistent; refill state is unknown", 0)
	}
	return state, nil
}

// planLabel derives the user-facing plan label from billing state and trial.
func planLabel(status billingStatus) string {
	state := status.stateValue()
	switch state {
	case "active":
		if status.Trial != nil && status.Trial.State != nil {
			trialState := strings.ToLower(strings.TrimSpace(*status.Trial.State))
			switch trialState {
			case "active":
				return "Trial credit"
			case "used_up":
				return "Prepaid credit (trial used up)"
			case "expired":
				return "Prepaid credit (trial expired)"
			case "withheld":
				return "Prepaid credit"
			}
		}
		return "Prepaid credit"
	case "pending_card":
		return "Billing pending card"
	case "provisioning":
		return "Billing provisioning"
	case "not_started":
		return "Billing not started"
	}
	if state == "" {
		return "Entire"
	}
	return "Entire (" + state + ")"
}

// zeroCreditGroups builds the canonical "no spendable credit" group set.
func zeroCreditGroups(currency, plan string) []quotaGroup {
	_ = plan
	return []quotaGroup{{
		DisplayName: groupCredit,
		Buckets: []quotaBucket{{
			Window:            windowCreditTotal,
			RemainingFraction: 0,
			Description:       "credit balance " + money(currency, 0) + " (0 cents)",
		}},
	}}
}

// zeroCreditSummary builds the metric set for a confirmed zero balance.
func zeroCreditSummary(currency string) []quotaMetric {
	return []quotaMetric{{
		Key:      metricCreditBalance,
		Label:    "Entire credit balance",
		Value:    0,
		Unit:     currency,
		Format:   "currency",
		Currency: currency,
	}}
}

// overageMetrics exposes spend past credit and the billing pause marker.
func overageMetrics(status billingStatus, currency string) []quotaMetric {
	out := make([]quotaMetric, 0, 2)
	if status.Overage != nil && status.Overage.AmountCents != nil {
		out = append(out, quotaMetric{
			Key:      metricOverage,
			Label:    "Entire overage this period",
			Value:    safeFloat(*status.Overage.AmountCents),
			Unit:     currency,
			Format:   "currency",
			Currency: currency,
		})
	}
	if status.RunsPaused != nil && status.RunsPaused.Paused != nil && *status.RunsPaused.Paused {
		out = append(out, quotaMetric{
			Key:    "entire_runs_paused",
			Label:  "Entire runs paused",
			Value:  1,
			Format: "number",
		})
	}
	return out
}

// nextExpiry returns the earliest credit expiry reported by the control plane.
func nextExpiry(status billingStatus) string {
	if status.Balance == nil || status.Balance.NextExpiry == nil || status.Balance.NextExpiry.ExpiresAt == nil {
		return ""
	}
	return strings.TrimSpace(*status.Balance.NextExpiry.ExpiresAt)
}

// money renders a cent amount as a currency string for bucket descriptions.
func money(currency string, cents int64) string {
	symbol := currency
	switch currency {
	case "USD":
		symbol = "$"
	}
	negative := cents < 0
	if negative {
		cents = -cents
	}
	rendered := symbol + formatCents(cents)
	if negative {
		return "-" + rendered
	}
	return rendered
}

// statusFallbackHTTP maps a plugin error onto the HTTP status the host should use.
func statusFallbackHTTP(err *pluginError) int {
	if err == nil {
		return 0
	}
	if err.HTTPStatus != 0 {
		return err.HTTPStatus
	}
	switch err.Code {
	case codeUnauthenticated:
		return http.StatusUnauthorized
	case codeForbidden:
		return http.StatusForbidden
	case codeMissingCredential, codeInvalidToken, codeBlockedHost:
		return http.StatusBadRequest
	case codeBillingUnreadable, codeUnknownQuota, codeUpstreamStatus, codeUpstreamMalformed:
		return http.StatusBadGateway
	}
	return 0
}
