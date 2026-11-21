package redisapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const DurableResultsProtocol = "durable-results-v1"
const DurableConsumerGroup = "durable-workers-v1"
const durableControlReplyLimit = 4096
const maxDurableLeaseMillis = 300000
const maxDurableEpoch = 1<<53 - 1

var errQueueProtocolMismatch = errors.New("queue protocol mismatch")

var ErrDurableDelivery = errors.New("durable delivery refused or unconfirmed")
var durableDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var durableQueue = regexp.MustCompile(`^jobs:v1:[A-Za-z0-9:._-]+$`)

// DeliveryIdentity carries the fixed HTTP tuple, without its bearer. It is not
// execution authority: a candidate must first receive a successful live renewal.
type DeliveryIdentity struct {
	DispatchID    string
	RequestDigest string
	Queue         string
	MessageID     string
	LeaseEpoch    uint64
}

type deliveryClaimData struct {
	identity DeliveryIdentity
	token    string
}

// DeliveryClaim is created only at the HTTP delivery boundary. Its immutable
// capture is opaque even to reflection-based bad-verb formatting diagnostics.
// Passive delivery observations are deliberately not used to create a claim.
type DeliveryClaim struct{ snapshot func() deliveryClaimData }

func (DeliveryClaim) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<delivery-claim>")
}
func (DeliveryClaim) String() string               { return "<delivery-claim>" }
func (DeliveryClaim) GoString() string             { return "<delivery-claim>" }
func (DeliveryClaim) MarshalJSON() ([]byte, error) { return []byte("null"), nil }
func (c *DeliveryClaim) Clone() *DeliveryClaim {
	if c == nil {
		return nil
	}
	copy := *c
	return &copy
}
func (c *DeliveryClaim) Identity() DeliveryIdentity {
	if c == nil || c.snapshot == nil {
		return DeliveryIdentity{}
	}
	return c.snapshot().identity
}

func newDeliveryClaim(identity DeliveryIdentity, token string) (*DeliveryClaim, error) {
	if !validDeliveryIdentity(identity) {
		return nil, ErrDurableDelivery
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return nil, ErrDurableDelivery
	}
	captured := deliveryClaimData{identity: identity, token: token}
	return &DeliveryClaim{snapshot: func() deliveryClaimData { return captured }}, nil
}

func validDeliveryIdentity(identity DeliveryIdentity) bool {
	id, err := uuid.Parse(identity.DispatchID)
	if err != nil || id == uuid.Nil || id.String() != identity.DispatchID {
		return false
	}
	if !durableDigest.MatchString(identity.RequestDigest) || len(identity.Queue) > 256 || !durableQueue.MatchString(identity.Queue) || identity.LeaseEpoch == 0 || identity.LeaseEpoch > maxDurableEpoch {
		return false
	}
	return validDurableStreamID(identity.MessageID)
}

func validDurableStreamID(value string) bool {
	first, second, ok := strings.Cut(value, "-")
	if !ok || len(first) == 0 || len(first) > 20 || len(second) == 0 || len(second) > 20 {
		return false
	}
	a, err := strconv.ParseUint(first, 10, 64)
	if err != nil || a == 0 || strconv.FormatUint(a, 10) != first {
		return false
	}
	b, err := strconv.ParseUint(second, 10, 64)
	return err == nil && strconv.FormatUint(b, 10) == second
}

// RenewDelivery returns only a trusted relative budget. The caller must sample
// its monotonic clock BEFORE this call; response latency consumes that budget.
// expiresAt is validated as advisory wire data, never used as a local deadline.
func (c *Client) RenewDelivery(ctx context.Context, claim *DeliveryClaim) (time.Duration, error) {
	data, err := claimForControl(claim)
	if err != nil {
		return 0, err
	}
	body, err := json.Marshal(map[string]any{"queue": data.identity.Queue, "messageId": data.identity.MessageID, "requestDigest": data.identity.RequestDigest, "leaseEpoch": data.identity.LeaseEpoch})
	if err != nil {
		return 0, ErrDurableDelivery
	}
	raw, err := c.durableRequest(ctx, "/api/fabric/dispatches/"+data.identity.DispatchID+"/renew", data.token, body)
	if err != nil {
		return 0, err
	}
	fields, err := closedControlObject(raw, "dispatchId", "requestDigest", "leaseEpoch", "expiresAt", "remainingLeaseMs")
	if err != nil {
		return 0, err
	}
	var dispatch, digest, expires string
	var epoch uint64
	var remaining uint64
	if json.Unmarshal(fields["dispatchId"], &dispatch) != nil || dispatch != data.identity.DispatchID || json.Unmarshal(fields["requestDigest"], &digest) != nil || digest != data.identity.RequestDigest || !controlInteger(fields["leaseEpoch"], maxDurableEpoch, &epoch) || epoch != data.identity.LeaseEpoch || json.Unmarshal(fields["expiresAt"], &expires) != nil || !validControlTimestamp(expires) || !controlInteger(fields["remainingLeaseMs"], maxDurableLeaseMillis, &remaining) {
		return 0, ErrDurableDelivery
	}
	return time.Duration(remaining) * time.Millisecond, nil
}

// CompleteDelivery uploads an envelope and validates only its commit receipt.
// It never reads terminal result bytes or repairs/reacquires a delivery claim.
func (c *Client) CompleteDelivery(ctx context.Context, claim *DeliveryClaim, envelope []byte) error {
	data, err := claimForControl(claim)
	if err != nil {
		return err
	}
	if len(envelope) == 0 || len(envelope) > 96<<20+1024 {
		return ErrDurableDelivery
	}
	query := url.Values{"queue": {data.identity.Queue}, "messageId": {data.identity.MessageID}, "leaseEpoch": {strconv.FormatUint(data.identity.LeaseEpoch, 10)}}
	raw, err := c.durableRequest(ctx, "/api/fabric/dispatches/"+data.identity.DispatchID+"/complete?"+query.Encode(), data.token, envelope)
	if err != nil {
		return err
	}
	fields, err := closedControlObject(raw, "dispatchId", "status", "envelopeSha256", "committedAt", "payloadExpiresAt", "outboxId")
	if err != nil {
		return err
	}
	var dispatch, state, digest, committed, expires, outbox string
	if json.Unmarshal(fields["dispatchId"], &dispatch) != nil || dispatch != data.identity.DispatchID || json.Unmarshal(fields["status"], &state) != nil || state != "committed" && state != "replayed" && state != "ack_repaired" || json.Unmarshal(fields["envelopeSha256"], &digest) != nil || !durableDigest.MatchString(digest) || json.Unmarshal(fields["committedAt"], &committed) != nil || json.Unmarshal(fields["payloadExpiresAt"], &expires) != nil || !validReceiptTimestamp(committed) || !validReceiptTimestamp(expires) || json.Unmarshal(fields["outboxId"], &outbox) != nil || !validDurableStreamID(outbox) {
		return ErrDurableDelivery
	}
	start, _ := time.Parse(time.RFC3339Nano, committed)
	end, _ := time.Parse(time.RFC3339Nano, expires)
	if !end.After(start) {
		return ErrDurableDelivery
	}
	return nil
}

func claimForControl(claim *DeliveryClaim) (deliveryClaimData, error) {
	if claim == nil || claim.snapshot == nil {
		return deliveryClaimData{}, ErrDurableDelivery
	}
	data := claim.snapshot()
	if !validDeliveryIdentity(data.identity) {
		return deliveryClaimData{}, ErrDurableDelivery
	}
	return data, nil
}

func validControlTimestamp(value string) bool {
	if !strings.HasSuffix(value, "Z") {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && parsed.Year() > 0
}

func (c *Client) durableRequest(ctx context.Context, path, token string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, ErrDurableDelivery
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-claim-token", token)
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, ErrDurableDelivery
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrDurableDelivery
	}
	if response.ContentLength > durableControlReplyLimit {
		return nil, ErrDurableDelivery
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, durableControlReplyLimit+1))
	if err != nil || len(raw) > durableControlReplyLimit {
		return nil, ErrDurableDelivery
	}
	return raw, nil
}

// Control replies are small closed objects. Decode each member independently so
// duplicate keys, wrong cases, trailing values and unexpected fields all refuse.
func closedControlObject(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	if len(raw) > durableControlReplyLimit {
		return nil, ErrDurableDelivery
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrDurableDelivery
	}
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	values := make(map[string]json.RawMessage, len(keys))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, ErrDurableDelivery
		}
		key, ok := token.(string)
		if !ok || !allowed[key] || values[key] != nil {
			return nil, ErrDurableDelivery
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, ErrDurableDelivery
		}
		values[key] = value
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') || len(values) != len(keys) {
		return nil, ErrDurableDelivery
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, ErrDurableDelivery
	}
	return values, nil
}

var receiptTimestamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{6})?Z$`)

func validReceiptTimestamp(value string) bool {
	return receiptTimestamp.MatchString(value) && validControlTimestamp(value)
}

// Interpret the JSON number exactly. Integral decimal/exponent spellings are
// valid, but rounded fractions and unsafe epochs must never match a claim.
func controlInteger(raw []byte, maximum uint64, target *uint64) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	text := number.String()
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		exponent, err := strconv.ParseInt(text[index+1:], 10, 32)
		// A <=4096-byte coefficient cannot make a larger exponent a positive
		// bounded integer. This also bounds rational conversion's allocation.
		if err != nil || exponent > 8192 || exponent < -8192 {
			return false
		}
	}
	rational, ok := new(big.Rat).SetString(text)
	if !ok || !rational.IsInt() || rational.Sign() <= 0 || !rational.Num().IsUint64() {
		return false
	}
	result := rational.Num().Uint64()
	if result > maximum {
		return false
	}
	*target = result
	return true
}
