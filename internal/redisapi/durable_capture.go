package redisapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/deliverymetadata"
)

type deliveryMetadata struct {
	present                                    bool
	valid                                      bool
	protocol, dispatchID, digest, token, epoch string
}

// Keep metadata out of public wire structs, formatting and legacy RawData.
// This capture has no execution authority and is never adopted by the generic
// ParseStreamMessage path. Only authenticated HTTP consume can mint a candidate.
func (data *StreamMessageData) UnmarshalJSON(raw []byte) error {
	type wire StreamMessageData
	if err := json.Unmarshal(raw, (*wire)(data)); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	meta := deliveryMetadata{valid: true}
	targets := []struct {
		name   string
		target *string
	}{
		{"protocol", &meta.protocol}, {"dispatchId", &meta.dispatchID}, {"requestDigest", &meta.digest}, {"claimToken", &meta.token}, {"leaseEpoch", &meta.epoch},
	}
	for _, field := range targets {
		value, ok := fields[field.name]
		if !ok {
			continue
		}
		meta.present = true
		if len(value) > 1024 || string(value) == "null" || json.Unmarshal(value, field.target) != nil {
			meta.valid = false
		}
	}
	// A differently-cased metadata name is never a legacy escape hatch.
	for name := range fields {
		for _, field := range targets {
			if strings.EqualFold(name, field.name) && name != field.name {
				meta.present = true
				meta.valid = false
			}
		}
	}
	captured := meta
	data.deliveryMetadata = func() deliveryMetadata { return captured }
	return nil
}

func (job *Job) HasDurableMetadata() bool        { return job.durableMetadata }
func (job *Job) ClaimedDelivery() *DeliveryClaim { return job.delivery.Clone() }

func adoptHTTPClaim(message StreamMessage, request ConsumeRequest) (*DeliveryClaim, error) {
	if request.Protocol != DurableResultsProtocol || request.Group != DurableConsumerGroup || message.Data.deliveryMetadata == nil {
		return nil, ErrDurableDelivery
	}
	meta := message.Data.deliveryMetadata()
	// Provenance is a syntactic rejection check, never a credential source.
	// The candidate's fields below come independently from the original HTTP data.
	if !meta.present || !meta.valid || meta.protocol != DurableResultsProtocol || message.Data.observation.State() != deliverymetadata.Observed || message.Data.observation.Provenance() != deliverymetadata.Original || meta.dispatchID != message.Data.JobID || message.Data.Type == "" || len(message.Data.Type) > 128 {
		return nil, ErrDurableDelivery
	}
	digest := sha256.Sum256([]byte(message.Data.Payload))
	if hex.EncodeToString(digest[:]) != meta.digest {
		return nil, ErrDurableDelivery
	}
	epoch, err := strconv.ParseUint(meta.epoch, 10, 64)
	if err != nil || strconv.FormatUint(epoch, 10) != meta.epoch {
		return nil, ErrDurableDelivery
	}
	return newDeliveryClaim(DeliveryIdentity{DispatchID: meta.dispatchID, RequestDigest: meta.digest, Queue: request.Queue, MessageID: message.ID, LeaseEpoch: epoch}, meta.token)
}
