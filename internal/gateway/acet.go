package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"
)

// PricingTier defines ACET cost per 1K tokens for a model size range.
type PricingTier struct {
	Name        string // "small", "medium", "large", "xlarge"
	MinParams   int    // billions, inclusive
	MaxParams   int    // billions, exclusive (0 = unbounded)
	ACETPer1K   int    // ACET tokens per 1,000 tokens
}

// DefaultPricingTiers returns the standard ACET pricing tiers.
func DefaultPricingTiers() []PricingTier {
	return []PricingTier{
		{Name: "small", MinParams: 0, MaxParams: 8, ACETPer1K: 1},
		{Name: "medium", MinParams: 8, MaxParams: 70, ACETPer1K: 5},
		{Name: "large", MinParams: 70, MaxParams: 400, ACETPer1K: 25},
		{Name: "xlarge", MinParams: 400, MaxParams: 0, ACETPer1K: 100},
	}
}

// TierByName returns the pricing tier for a given name.
func TierByName(name string) (PricingTier, bool) {
	for _, t := range DefaultPricingTiers() {
		if t.Name == name {
			return t, true
		}
	}
	return PricingTier{}, false
}

// CalculateACETCost computes the total ACET cost for a request.
func CalculateACETCost(tier PricingTier, tokensIn, tokensOut int) int {
	totalTokens := tokensIn + tokensOut
	// ACET per 1K tokens, round up to ensure minimum billing
	cost := int(math.Ceil(float64(totalTokens) * float64(tier.ACETPer1K) / 1000.0))
	if cost < 1 && totalTokens > 0 {
		cost = 1 // minimum charge
	}
	return cost
}

// OperatorShare returns the operator's portion: ceil(80% of cost).
func OperatorShare(cost int) int {
	return int(math.Ceil(float64(cost) * 0.80))
}

// PlatformShare returns the platform's portion: cost - operator share.
func PlatformShare(cost int) int {
	return cost - OperatorShare(cost)
}

// settleRequest is the payload sent to the platform settlement endpoint.
type settleRequest struct {
	Model       string `json:"model"`
	TokensIn    int    `json:"tokens_in"`
	TokensOut   int    `json:"tokens_out"`
	ACETCost    int    `json:"acet_cost"`
	ConsumerOrg string `json:"consumer_org"`
}

// balanceResponse is returned from the platform balance check endpoint.
type balanceResponse struct {
	Balance int  `json:"balance"`
	OK      bool `json:"ok"`
}

// ACETClient handles ACET token settlement with the platform.
type ACETClient struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client

	// offlineQueue stores settlements that failed due to connectivity
	offlineQueue []settleRequest
	offlineMu    sync.Mutex
}

// NewACETClient creates a client for ACET settlement.
func NewACETClient(baseURL, apiToken string) *ACETClient {
	return &ACETClient{
		baseURL:  baseURL,
		apiToken: apiToken,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Settle reports a completed request to the platform for ACET settlement.
// If the platform is unreachable, the settlement is queued locally.
func (c *ACETClient) Settle(model string, tokensIn, tokensOut, acetCost int, consumerOrg string) error {
	req := settleRequest{
		Model:       model,
		TokensIn:    tokensIn,
		TokensOut:   tokensOut,
		ACETCost:    acetCost,
		ConsumerOrg: consumerOrg,
	}

	if err := c.doSettle(req); err != nil {
		// Queue for later
		c.offlineMu.Lock()
		c.offlineQueue = append(c.offlineQueue, req)
		c.offlineMu.Unlock()
		return fmt.Errorf("settle queued (offline): %w", err)
	}

	return nil
}

func (c *ACETClient) doSettle(s settleRequest) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/acet/settle", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("settlement failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

// CheckBalance queries the platform for a consumer's ACET balance.
func (c *ACETClient) CheckBalance(consumerOrg string) (int, error) {
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/acet/balance?org=%s", c.baseURL, consumerOrg), nil)
	if err != nil {
		return 0, err
	}
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("balance check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("balance check: HTTP %d", resp.StatusCode)
	}

	var br balanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		return 0, fmt.Errorf("decode balance: %w", err)
	}
	return br.Balance, nil
}

// FlushOfflineQueue attempts to settle all queued transactions.
// Returns the number successfully settled and any remaining errors.
func (c *ACETClient) FlushOfflineQueue() (settled int, remaining int) {
	c.offlineMu.Lock()
	queue := c.offlineQueue
	c.offlineQueue = nil
	c.offlineMu.Unlock()

	var failed []settleRequest
	for _, s := range queue {
		if err := c.doSettle(s); err != nil {
			failed = append(failed, s)
		} else {
			settled++
		}
	}

	if len(failed) > 0 {
		c.offlineMu.Lock()
		c.offlineQueue = append(c.offlineQueue, failed...)
		c.offlineMu.Unlock()
	}
	return settled, len(failed)
}

// QueueLen returns the number of unsettled transactions in the offline queue.
func (c *ACETClient) QueueLen() int {
	c.offlineMu.Lock()
	defer c.offlineMu.Unlock()
	return len(c.offlineQueue)
}
