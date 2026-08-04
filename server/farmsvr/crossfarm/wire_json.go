package crossfarm

import (
	"encoding/json"

	"farm/server/shared/clientjson"
)

type visitorRewardJSONAlias VisitorReward

// VisitorReward 会直接成为浏览器动作响应；随机 req_id 可能覆盖完整 uint64，
// 因此必须按不透明 ID 编码为十进制字符串。
func (reward VisitorReward) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ReqID        clientjson.Uint64 `json:"req_id"`
		CoinGained   clientjson.Int64  `json:"coin_gained"`
		Compensation clientjson.Int64  `json:"compensation,omitempty"`
		visitorRewardJSONAlias
	}{
		ReqID:                  clientjson.Uint64(reward.ReqID),
		CoinGained:             clientjson.Int64(reward.CoinGained),
		Compensation:           clientjson.Int64(reward.Compensation),
		visitorRewardJSONAlias: visitorRewardJSONAlias(reward),
	})
}

func (reward *VisitorReward) UnmarshalJSON(data []byte) error {
	wire := struct {
		ReqID        clientjson.Uint64 `json:"req_id"`
		CoinGained   clientjson.Int64  `json:"coin_gained"`
		Compensation clientjson.Int64  `json:"compensation,omitempty"`
		*visitorRewardJSONAlias
	}{
		visitorRewardJSONAlias: (*visitorRewardJSONAlias)(reward),
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	reward.ReqID = uint64(wire.ReqID)
	reward.CoinGained = int64(wire.CoinGained)
	reward.Compensation = int64(wire.Compensation)
	return nil
}
