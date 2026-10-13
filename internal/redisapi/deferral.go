package redisapi

import "context"

// DeferralSource is the internal durable-deferral protocol boundary. The
// request and response types do not define an HTTP or WebSocket wire contract.
type DeferralSource interface {
	Register(context.Context, DeferralRegisterRequest) (DeferralRegisterResponse, error)
	Consume(context.Context, DeferralConsumeRequest) (DeferralConsumeResponse, error)
	Release(context.Context, DeferralReleaseRequest) (DeferralReleaseResponse, error)
	Start(context.Context, DeferralStartRequest) (DeferralStartResponse, error)
	Settle(context.Context, DeferralSettleRequest) (DeferralSettleResponse, error)
	Recover(context.Context, DeferralRecoverRequest) (DeferralRecoverResponse, error)
	Status(context.Context, DeferralStatusRequest) (DeferralStatusResponse, error)
}

type disabledDeferralSource struct{}

// NewDeferralSource returns the production deferral source. R2 has no
// activation mechanism in this source slice, so this constructor accepts no
// configuration, credentials, callbacks, or I/O dependencies and always
// returns the inert implementation.
func NewDeferralSource() DeferralSource {
	return disabledDeferralSource{}
}

func (disabledDeferralSource) Register(_ context.Context, _ DeferralRegisterRequest) (DeferralRegisterResponse, error) {
	return DeferralRegisterResponse{}, ErrR2NotActivated
}

func (disabledDeferralSource) Consume(_ context.Context, _ DeferralConsumeRequest) (DeferralConsumeResponse, error) {
	return DeferralConsumeResponse{}, ErrR2NotActivated
}

func (disabledDeferralSource) Release(_ context.Context, _ DeferralReleaseRequest) (DeferralReleaseResponse, error) {
	return DeferralReleaseResponse{}, ErrR2NotActivated
}

func (disabledDeferralSource) Start(_ context.Context, _ DeferralStartRequest) (DeferralStartResponse, error) {
	return DeferralStartResponse{}, ErrR2NotActivated
}

func (disabledDeferralSource) Settle(_ context.Context, _ DeferralSettleRequest) (DeferralSettleResponse, error) {
	return DeferralSettleResponse{}, ErrR2NotActivated
}

func (disabledDeferralSource) Recover(_ context.Context, _ DeferralRecoverRequest) (DeferralRecoverResponse, error) {
	return DeferralRecoverResponse{}, ErrR2NotActivated
}

func (disabledDeferralSource) Status(_ context.Context, _ DeferralStatusRequest) (DeferralStatusResponse, error) {
	return DeferralStatusResponse{}, ErrR2NotActivated
}
