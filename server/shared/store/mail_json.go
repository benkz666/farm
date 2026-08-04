package store

import (
	"encoding/json"

	"farm/server/shared/clientjson"
)

type mailJSONAlias Mail

// MarshalJSON 只改变传输形状，不改变数据库模型：mail.id 在浏览器协议中始终是
// 十进制字符串，领取时由 clientjson.Uint64 同时兼容新字符串与旧 number 请求。
func (mail Mail) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID             clientjson.Uint64 `json:"id"`
		AttachmentCoin clientjson.Int64  `json:"attachment_coin"`
		mailJSONAlias
	}{
		ID:             clientjson.Uint64(mail.ID),
		AttachmentCoin: clientjson.Int64(mail.AttachmentCoin),
		mailJSONAlias:  mailJSONAlias(mail),
	})
}

func (mail *Mail) UnmarshalJSON(data []byte) error {
	wire := struct {
		ID             clientjson.Uint64 `json:"id"`
		AttachmentCoin clientjson.Int64  `json:"attachment_coin"`
		*mailJSONAlias
	}{
		mailJSONAlias: (*mailJSONAlias)(mail),
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	mail.ID = uint64(wire.ID)
	mail.AttachmentCoin = int64(wire.AttachmentCoin)
	return nil
}

type taskJSONAlias Task

func (task Task) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		RewardCoin clientjson.Int64 `json:"reward_coin"`
		taskJSONAlias
	}{
		RewardCoin:    clientjson.Int64(task.RewardCoin),
		taskJSONAlias: taskJSONAlias(task),
	})
}

func (task *Task) UnmarshalJSON(data []byte) error {
	wire := struct {
		RewardCoin clientjson.Int64 `json:"reward_coin"`
		*taskJSONAlias
	}{
		taskJSONAlias: (*taskJSONAlias)(task),
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	task.RewardCoin = int64(wire.RewardCoin)
	return nil
}

type taskRewardJSONAlias TaskReward

func (reward TaskReward) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Coin clientjson.Int64 `json:"coin"`
		taskRewardJSONAlias
	}{
		Coin:                clientjson.Int64(reward.Coin),
		taskRewardJSONAlias: taskRewardJSONAlias(reward),
	})
}

func (reward *TaskReward) UnmarshalJSON(data []byte) error {
	wire := struct {
		Coin clientjson.Int64 `json:"coin"`
		*taskRewardJSONAlias
	}{
		taskRewardJSONAlias: (*taskRewardJSONAlias)(reward),
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	reward.Coin = int64(wire.Coin)
	return nil
}
