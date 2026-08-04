package farm

// CrossReceiptTTL 是主人侧跨农场裁决结果的保留时间。它覆盖 Gateway 的 5 秒
// 等待窗口、访客预占的 10 秒兜底窗口，并为消息总线重投留出足够余量。
const CrossReceiptTTL int64 = 10 * 60 * 1000

// maxCrossReceipts 防止离线主人收到大量跨农场请求后无限增长。容量必须覆盖玩法允许
// 的 200 名好友同时操作并留出重投余量，否则热点突发会过早淘汰已成功的回执，使同
// req_id 重试从 OK 退化成 AlreadyWatered。万人压测绕过了好友上限，不属于该保证。
const maxCrossReceipts = 256

// CrossReceipt 是主人侧一次已提交跨农场动作的可重放结果。Code 使用 int 避免
// farm 包依赖 cross 包；cross 在边界处转换为协议错误码。
type CrossReceipt struct {
	ReqID        uint64  `json:"req_id"`
	VisitorUID   uint64  `json:"visitor_uid"`
	OwnerUID     uint64  `json:"owner_uid"`
	Code         int     `json:"code"`
	CropID       uint16  `json:"crop_id,omitempty"`
	Amount       uint16  `json:"amount,omitempty"`
	Compensation int64   `json:"compensation,omitempty"`
	DogType      DogType `json:"dog_type,omitempty"`
	CreatedAt    int64   `json:"created_at"`
}

// FindCrossReceipt returns the exact previous result for a retried request.
// Identity fields are checked as well as req_id, so a corrupt or collided key
// can never reveal another visitor's result.
func (a *Aggregate) FindCrossReceipt(reqID, visitorUID, ownerUID uint64, now int64) (CrossReceipt, bool) {
	if a == nil || reqID == 0 {
		return CrossReceipt{}, false
	}
	a.expireCrossReceipts(now)
	receipt, ok := a.CrossReceipts[reqID]
	if !ok || receipt.VisitorUID != visitorUID || receipt.OwnerUID != ownerUID {
		return CrossReceipt{}, false
	}
	return receipt, true
}

// RecordCrossReceipt persists a new owner-side decision. Replacing the same
// req_id is harmless and keeps retries idempotent.
func (a *Aggregate) RecordCrossReceipt(receipt CrossReceipt, now int64) {
	if a == nil || receipt.ReqID == 0 || receipt.VisitorUID == 0 || receipt.OwnerUID != a.UID {
		return
	}
	a.expireCrossReceipts(now)
	if a.CrossReceipts == nil {
		a.CrossReceipts = make(map[uint64]CrossReceipt, 1)
	}
	receipt.CreatedAt = now
	a.CrossReceipts[receipt.ReqID] = receipt
	for len(a.CrossReceipts) > maxCrossReceipts {
		var oldestID uint64
		var oldestAt int64
		for reqID, entry := range a.CrossReceipts {
			if oldestID == 0 || entry.CreatedAt < oldestAt {
				oldestID, oldestAt = reqID, entry.CreatedAt
			}
		}
		delete(a.CrossReceipts, oldestID)
	}
}

func (a *Aggregate) expireCrossReceipts(now int64) {
	if a == nil || len(a.CrossReceipts) == 0 {
		return
	}
	for reqID, receipt := range a.CrossReceipts {
		if now-receipt.CreatedAt >= CrossReceiptTTL {
			delete(a.CrossReceipts, reqID)
		}
	}
	if len(a.CrossReceipts) == 0 {
		a.CrossReceipts = nil
	}
}
