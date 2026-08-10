package gateway

import (
	"context"

	publicv3 "farm/server/gen/farm/public/v3"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
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
		response.Err = errcode.BadRequest
		return response
	}
	var payload setTimeProfileRequest
	if request.CommandRequest != nil {
		payload.TimeProfile = request.CommandRequest.TimeProfile
	} else if err := unmarshalPayload(request.Payload, &payload); err != nil {
		response.Err = errcode.BadRequest
		return response
	}
	if !gameconfig.ValidTimeProfile(payload.TimeProfile) {
		response.Err = errcode.BadRequest
		return response
	}
	if err := g.switchTimeProfile(context.Background(), payload.TimeProfile); err != nil {
		response.Err = errcode.Internal
		return response
	}
	result := setTimeProfileResponse{
		TimeProfile:        g.TimeProfile(),
		TimeProfileMutable: true,
	}
	response.Payload = marshalPayload(result)
	response.CommandResponse = &publicv3.CommandResponse{TimeProfile: result.TimeProfile, TimeProfileMutable: result.TimeProfileMutable}
	return response
}
