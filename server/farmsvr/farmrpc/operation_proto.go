package farmrpc

import farmv1 "farm/server/gen/farm/v1"

var operationToProto = map[Operation]farmv1.Operation{
	OperationEnterFarm:    farmv1.Operation_OPERATION_ENTER_FARM,
	OperationPlotAction:   farmv1.Operation_OPERATION_PLOT_ACTION,
	OperationShop:         farmv1.Operation_OPERATION_SHOP,
	OperationSyncFarm:     farmv1.Operation_OPERATION_SYNC_FARM,
	OperationPet:          farmv1.Operation_OPERATION_PET,
	OperationCrossReserve: farmv1.Operation_OPERATION_CROSS_RESERVE,
	OperationCrossSettle:  farmv1.Operation_OPERATION_CROSS_SETTLE,
	OperationTaskList:     farmv1.Operation_OPERATION_TASK_LIST,
	OperationTaskClaim:    farmv1.Operation_OPERATION_TASK_CLAIM,
	OperationAdvanceTask:  farmv1.Operation_OPERATION_ADVANCE_TASK,
	OperationDailyLogin:   farmv1.Operation_OPERATION_DAILY_LOGIN_CLAIM,
	OperationMailList:     farmv1.Operation_OPERATION_MAIL_LIST,
	OperationMailRead:     farmv1.Operation_OPERATION_MAIL_READ,
	OperationMailDelete:   farmv1.Operation_OPERATION_MAIL_DELETE,
	OperationMailClaim:    farmv1.Operation_OPERATION_MAIL_CLAIM,
	OperationCodexList:    farmv1.Operation_OPERATION_CODEX_LIST,
}

var protoToOperation = func() map[farmv1.Operation]Operation {
	out := make(map[farmv1.Operation]Operation, len(operationToProto))
	for operation, proto := range operationToProto {
		out[proto] = operation
	}
	return out
}()

func operationToProtoEnum(operation Operation) (farmv1.Operation, bool) {
	value, ok := operationToProto[operation]
	return value, ok
}

func operationFromProtoEnum(value farmv1.Operation) (Operation, bool) {
	operation, ok := protoToOperation[value]
	return operation, ok
}
