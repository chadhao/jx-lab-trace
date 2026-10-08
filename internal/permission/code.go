package permission

// Package permission：权限点常量（代码注册源）＋ 判定逻辑。
//
// ★★ 声明铁律：code.go 是**权限点常量的唯一声明文件**。
//   scripts/check_perm_registry.py 的判据③ 建立在这一约定上：除本文件外，
//   任何 Go 非测试代码都不得出现权限点字符串字面量、也不得定义 Code 类型常量；
//   受保护入口一律写 RequirePerm(permission.SysPermEdit)，字符串→Code 的转换
//   只允许发生在本文件内（Go 类型系统先拦一道，门禁再拦一道）。
//   取值必须与 spec/permission-points.json#permission_points 一一对应（51 条，
//   由 internal/permission/spec_sync_test.go 逐条比对）。

// Code 是权限点标识。用具名类型而非 string：受保护入口若裸写字符串，
// 编译期即报错（cannot use "sys.perm.edit" as Code），把「禁裸写」变成编译约束。
type Code string

// String 返回权限点 code 字面量。
func (c Code) String() string { return string(c) }

// 51 个权限点常量（顺序 = spec/permission-points.json 的声明顺序）。
const (
	MdCustomer                 Code = "md.customer"
	MdMaterial                 Code = "md.material"
	MdTestItem                 Code = "md.test_item"
	MdVehicle                  Code = "md.vehicle"
	MdTeam                     Code = "md.team"
	RecvNoticeCreate           Code = "recv.notice.create"
	RecvNoticeEdit             Code = "recv.notice.edit"
	RecvArriveConfirm          Code = "recv.arrive.confirm"
	RecvWeigh                  Code = "recv.weigh"
	RecvBagGen                 Code = "recv.bag.gen"
	RecvLabelPrint             Code = "recv.label.print"
	RecvLabelReprint           Code = "recv.label.reprint"
	RecvReturn                 Code = "recv.return"
	SampleTake                 Code = "sample.take"
	SampleRetainIn             Code = "sample.retain.in"
	SampleRetainLend           Code = "sample.retain.lend"
	SampleRetainDestroyInit    Code = "sample.retain.destroy.init"
	SampleRetainDestroyApprove Code = "sample.retain.destroy.approve"
	ProdBatchCreate            Code = "prod.batch.create"
	ProdFeedScan               Code = "prod.feed.scan"
	ProdFeedCorrect            Code = "prod.feed.correct"
	ProdOpLog                  Code = "prod.op.log"
	ProdFgGen                  Code = "prod.fg.gen"
	ProdRework                 Code = "prod.rework"
	InspTaskView               Code = "insp.task.view"
	InspScopeEdit              Code = "insp.scope.edit"
	InspResultEntry            Code = "insp.result.entry"
	InspResultCorrect          Code = "insp.result.correct"
	InspFileUpload             Code = "insp.file.upload"
	InspConclusion             Code = "insp.conclusion"
	InspDisposition            Code = "insp.disposition"
	InspConcessionQcSign       Code = "insp.concession.qc_sign"
	InspConcessionDeptSign     Code = "insp.concession.dept_sign"
	InspUrgentReleaseInit      Code = "insp.urgent.release.init"
	InspUrgentReleaseApprove   Code = "insp.urgent.release.approve"
	ShipLoadScan               Code = "ship.load.scan"
	ShipOutRegister            Code = "ship.out.register"
	ShipVoidInit               Code = "ship.void.init"
	ShipVoidApprove            Code = "ship.void.approve"
	TraceForward               Code = "trace.forward"
	TraceBackward              Code = "trace.backward"
	TraceBatchView             Code = "trace.batch.view"
	ReportGenerate             Code = "report.generate"
	ReportShareManage          Code = "report.share.manage"
	RptView                    Code = "rpt.view"
	RptExport                  Code = "rpt.export"
	SysUserManage              Code = "sys.user.manage"
	SysPermEdit                Code = "sys.perm.edit"
	SysAuditView               Code = "sys.audit.view"
	SysCodeRule                Code = "sys.code.rule"
	SysParamEdit               Code = "sys.param.edit"
)
