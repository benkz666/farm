package gateway

import (
	"context"

	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
)

type setTimeProfileRequest struct {
	TimeProfile string `json:"time_profile"`
}

type setTimeProfileResponse struct {
	TimeProfile        string `json:"time_profile"`
	TimeProfileMutable bool   `json:"time_profile_mutable"`
}

// handleSetTimeProfile is intentionally available only when
// FARM_ALLOW_DEBUG_TIME=1 enabled the debug surface at process startup.
// The browser requests a profile change, but authoritative Farm/Gateway
// processes still validate and own the actual value.
func (g *Gateway) handleSetTimeProfile(_ *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	if !g.allowDebug {
		response.Err = pkgerr.BadRequest
		return response
	}
	var payload setTimeProfileRequest
	if err := unmarshalPayload(request.Payload, &payload); err != nil ||
		!gameconf.ValidTimeProfile(payload.TimeProfile) {
		response.Err = pkgerr.BadRequest
		return response
	}
	if err := g.switchTimeProfile(context.Background(), payload.TimeProfile); err != nil {
		response.Err = pkgerr.Internal
		return response
	}
	response.Payload = marshalPayload(setTimeProfileResponse{
		TimeProfile:        g.TimeProfile(),
		TimeProfileMutable: true,
	})
	return response
}
