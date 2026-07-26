package gameconf

// FertilizerConf 描述一种化肥。ReduceHours 是未缩放的绝对缩短时长。
type FertilizerConf struct {
	ID          uint16
	ReduceHours float64
}

var fertilizerTable = []FertilizerConf{
	{ID: 1, ReduceHours: 1.0}, // 普通化肥
	{ID: 2, ReduceHours: 2.5}, // 高速化肥
	{ID: 3, ReduceHours: 5.5}, // 急速化肥
}

// FertilizerByID 按协议中的 fertilizer_id 查找化肥配置。
func FertilizerByID(id uint16) (FertilizerConf, bool) {
	if id == 0 || int(id) > len(fertilizerTable) {
		return FertilizerConf{}, false
	}
	return fertilizerTable[id-1], true
}

// ReduceDuration 返回当前时间档中化肥能缩短的毫秒数。
func (c FertilizerConf) ReduceDuration(profile string) int64 {
	return int64(c.ReduceHours * float64(HourMs(profile)))
}
