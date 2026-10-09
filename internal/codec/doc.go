// Package codec 是「追踪码引擎」（生成 / 解析 / 校验位 / 人读行互转）。
//
// 批 1（M0）只落位本包声明，批 3（M3 收货与打码）已实现 —— 见 codec.go。
//
//	★ 唯一真源：spec/code-rules.json（v1 已冻结）；docs/02-追踪码规则.md 是散文说明，
//	两者若有出入以 JSON 为准（任务包 §6-4）。
//	★ 纯函数：不连库、不起服务即可断言（D1）。
package codec
