package farm

// Clone 深拷贝聚合快照，供异步落盘在 actor 继续修改原对象时安全读取。
// 禁止用 JSON marshal 克隆：既慢又无法保证与内存布局一致。
func (a *Aggregate) Clone() *Aggregate {
	if a == nil {
		return nil
	}
	cp := *a
	cp.Plots = a.Plots

	if len(a.Items) > 0 {
		cp.Items = make(map[ItemKey]uint32, len(a.Items))
		for k, v := range a.Items {
			cp.Items[k] = v
		}
	} else {
		cp.Items = nil
	}

	if len(a.CodexHarvests) > 0 {
		cp.CodexHarvests = make(map[uint16]uint32, len(a.CodexHarvests))
		for k, v := range a.CodexHarvests {
			cp.CodexHarvests[k] = v
		}
	} else {
		cp.CodexHarvests = nil
	}

	if len(a.CrossPending) > 0 {
		cp.CrossPending = make(map[uint64]CrossReservation, len(a.CrossPending))
		for k, v := range a.CrossPending {
			cp.CrossPending[k] = v
		}
	} else {
		cp.CrossPending = nil
	}

	if len(a.CrossReceipts) > 0 {
		cp.CrossReceipts = make(map[uint64]CrossReceipt, len(a.CrossReceipts))
		for k, v := range a.CrossReceipts {
			cp.CrossReceipts[k] = v
		}
	} else {
		cp.CrossReceipts = nil
	}

	return &cp
}
