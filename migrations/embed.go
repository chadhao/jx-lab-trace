// Package migrations 内嵌本批全部迁移文件，使交付物保持**单二进制**
// （go:embed 不能跨目录引用 ../spec，故 spec 副本落在本目录；
// 副本与 spec/ 原件必须逐字节一致 —— 由 internal/store/migrations_sync_test.go 机检）。
package migrations

import _ "embed"

// SchemaSQL 是 38 张表的建表 DDL（源：spec/schema.sql）。
//
//go:embed 0001_init.sql
var SchemaSQL []byte

// PermissionSpecJSON 是权限点字典 / 角色 / 授权种子（源：spec/permission-points.json）。
//
//go:embed permission-points.json
var PermissionSpecJSON []byte
